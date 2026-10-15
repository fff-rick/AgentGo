package builtin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

// DatabaseTool owns a separate pool for explicitly approved, shared business views.
type DatabaseTool struct {
	db           *sql.DB
	databaseName string
	allowedViews map[string]bool
	cfg          config.DatabaseToolConfig
	logger       *zap.Logger
}

func NewDatabaseTool(ctx context.Context, cfg config.DatabaseToolConfig, logger *zap.Logger) (*DatabaseTool, error) {
	if strings.TrimSpace(cfg.DSN) == "" || len(cfg.AllowedViews) == 0 {
		return nil, fmt.Errorf("数据库工具必须配置独立 DSN 和 allowed_views")
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 10 * time.Second
	}
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = 100
	}
	if cfg.MaxSQLBytes <= 0 {
		cfg.MaxSQLBytes = 16384
	}
	if cfg.MaxCellBytes <= 0 {
		cfg.MaxCellBytes = 65536
	}
	if cfg.MaxResultBytes <= 0 {
		cfg.MaxResultBytes = 1048576
	}
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("数据库工具 DSN 无效")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	t := &DatabaseTool{db: db, cfg: cfg, logger: logger, allowedViews: make(map[string]bool)}
	for _, name := range cfg.AllowedViews {
		name = strings.TrimSpace(name)
		if len(strings.Split(name, ".")) != 2 {
			db.Close()
			return nil, fmt.Errorf("allowed_views 必须使用 schema.view 名称")
		}
		t.allowedViews[name] = true
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.QueryTimeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err == nil {
		defer tx.Rollback()
		err = tx.QueryRowContext(ctx, "SELECT current_database()").Scan(&t.databaseName)
		if err == nil {
			err = t.validatePermissions(ctx, tx)
		}
	}
	if err != nil {
		db.Close()
		var permissionErr databasePermissionError
		if errors.As(err, &permissionErr) {
			return nil, permissionErr
		}
		// Connection errors can contain credentials or DSN parameters.
		return nil, fmt.Errorf("数据库工具连接或权限检查失败 (%s)", databaseErrorCategory(err))
	}
	return t, nil
}

func (t *DatabaseTool) Close() error { return t.db.Close() }
func (t *DatabaseTool) Name() string { return "database_query" }
func (t *DatabaseTool) Description() string {
	return "在明确授权的共享业务视图中执行只读 SELECT，返回有大小限制的 JSON；不能查询个人记忆或内部表。视图：" + strings.Join(t.cfg.AllowedViews, ", ")
}
func (t *DatabaseTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"sql":      map[string]interface{}{"type": "string", "description": "单条只读 SELECT 查询；禁止多语句和数据修改"},
			"database": map[string]interface{}{"type": "string", "description": "仅支持工具专用连接所配置的数据库，省略即可"},
		}, "required": []string{"sql"},
	}
}

type databaseQueryParams struct {
	SQL      string `json:"sql"`
	Database string `json:"database"`
}
type databaseQueryOutput struct {
	Columns   []string          `json:"columns"`
	Rows      []json.RawMessage `json:"rows"`
	RowCount  int               `json:"row_count"`
	Truncated bool              `json:"truncated"`
}

func (t *DatabaseTool) Execute(ctx context.Context, input string) (*tool.ToolResult, error) {
	start := time.Now()
	category, digest := "ok", ""
	defer func() {
		t.logger.Info("数据库工具查询", zap.String("query_sha256", digest), zap.Duration("elapsed", time.Since(start)), zap.String("error_category", category))
	}()
	fail := func(kind, message string) (*tool.ToolResult, error) {
		category = kind
		return tool.NewErrorResult(message), nil
	}
	dbFail := func(err error) (*tool.ToolResult, error) {
		category = databaseErrorCategory(err)
		return tool.NewErrorResult("数据库查询失败 (" + category + ")"), nil
	}
	var params databaseQueryParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return fail("invalid_input", "参数解析失败")
	}
	sum := sha256.Sum256([]byte(params.SQL))
	digest = hex.EncodeToString(sum[:])
	if len(params.SQL) > t.cfg.MaxSQLBytes {
		return fail("sql_limit", "SQL 超过字节限制")
	}
	query, err := validateReadOnlyQuery(params.SQL)
	if err != nil {
		return fail("invalid_sql", err.Error())
	}
	if params.Database != "" && params.Database != "default" && params.Database != t.databaseName {
		return fail("database_denied", "仅允许查询工具配置的数据库")
	}
	queryCtx, cancel := context.WithTimeout(ctx, t.cfg.QueryTimeout)
	defer cancel()
	tx, err := t.db.BeginTx(queryCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return dbFail(err)
	}
	defer tx.Rollback()
	timeoutMS := max(int64(1), t.cfg.QueryTimeout.Milliseconds())
	if _, err := tx.ExecContext(queryCtx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeoutMS)); err != nil {
		return dbFail(err)
	}
	if _, err := tx.ExecContext(queryCtx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return dbFail(err)
	}
	// Recheck before every query so accidental later grants fail closed too.
	if err := t.validatePermissions(queryCtx, tx); err != nil {
		return fail("permissions", "工具数据库授权已发生变化，拒绝查询")
	}
	rows, err := tx.QueryContext(queryCtx, query)
	if err != nil {
		return dbFail(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return dbFail(err)
	}
	for _, name := range columns {
		encoded, _ := json.Marshal(name)
		if len(encoded) > t.cfg.MaxCellBytes {
			return fail("cell_limit", "结果列名超过字节限制")
		}
	}
	output := databaseQueryOutput{Columns: columns, Rows: make([]json.RawMessage, 0), RowCount: t.cfg.MaxRows}
	header, err := json.Marshal(output)
	if err != nil {
		return fail("encoding", "结果编码失败")
	}
	resultBytes := len(header) // Reserve the largest row_count and truncated:false envelope.
	if resultBytes > t.cfg.MaxResultBytes {
		return fail("result_limit", "结果列信息超过总字节限制")
	}
	for rows.Next() {
		if len(output.Rows) == t.cfg.MaxRows {
			output.Truncated = true
			break
		}
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return dbFail(err)
		}
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				values[i] = string(bytes)
			}
			encoded, err := json.Marshal(values[i])
			if err != nil {
				return fail("encoding", "结果编码失败")
			}
			if len(encoded) > t.cfg.MaxCellBytes {
				return fail("cell_limit", "结果单元格超过字节限制")
			}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return fail("encoding", "结果编码失败")
		}
		size := len(encoded)
		if len(output.Rows) > 0 {
			size++
		}
		if resultBytes+size > t.cfg.MaxResultBytes {
			output.Truncated = true
			break
		}
		resultBytes += size
		output.Rows = append(output.Rows, encoded)
	}
	if err := rows.Err(); err != nil {
		return dbFail(err)
	}
	if err := rows.Close(); err != nil {
		return dbFail(err)
	}
	// Rollback also clears session changes attempted through SELECT set_config(...).
	output.RowCount = len(output.Rows)
	encoded, err := json.Marshal(output)
	if err != nil {
		return fail("encoding", "结果编码失败")
	}
	return tool.NewSuccessResult(string(encoded)), nil
}

func databaseErrorCategory(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "sqlstate_" + pgErr.Code
	}
	return "database_error"
}

func validateReadOnlyQuery(raw string) (string, error) {
	query := strings.TrimSpace(raw)
	if query == "" {
		return "", fmt.Errorf("SQL 查询不能为空")
	}
	if strings.HasSuffix(query, ";") {
		query = strings.TrimSpace(strings.TrimSuffix(query, ";"))
	}
	if strings.Contains(query, ";") {
		return "", fmt.Errorf("安全限制：仅允许执行一条 SQL 查询")
	}
	fields := strings.Fields(query)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "SELECT") {
		return "", fmt.Errorf("安全限制：仅允许 SELECT 查询")
	}
	return query, nil
}
