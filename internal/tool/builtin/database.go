package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

// DatabaseTool executes bounded, read-only SQL against AgentGo's PostgreSQL database.
type DatabaseTool struct {
	db           *sql.DB
	databaseName string
	queryTimeout time.Duration
	maxRows      int
	logger       *zap.Logger
}

func NewDatabaseTool(db *sql.DB, cfg config.PostgresConfig, logger *zap.Logger) *DatabaseTool {
	queryTimeout := cfg.QueryTimeout
	if queryTimeout <= 0 {
		queryTimeout = 10 * time.Second
	}
	maxRows := cfg.MaxRows
	if maxRows <= 0 {
		maxRows = 100
	}
	return &DatabaseTool{db: db, databaseName: cfg.DBName, queryTimeout: queryTimeout, maxRows: maxRows, logger: logger}
}

func (t *DatabaseTool) Name() string { return "database_query" }

func (t *DatabaseTool) Description() string {
	return "在 AgentGo PostgreSQL 数据库中执行只读 SELECT 查询，返回 JSON 格式的列名和数据行"
}

func (t *DatabaseTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"sql": map[string]interface{}{
				"type":        "string",
				"description": "单条只读 SELECT 查询；禁止多语句和数据修改",
			},
			"database": map[string]interface{}{
				"type":        "string",
				"description": "目标数据库；仅支持当前配置的数据库，省略即可",
			},
		},
		"required": []string{"sql"},
	}
}

type databaseQueryParams struct {
	SQL      string `json:"sql"`
	Database string `json:"database"`
}

type databaseQueryOutput struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
}

func (t *DatabaseTool) Execute(ctx context.Context, input string) (*tool.ToolResult, error) {
	var params databaseQueryParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return tool.NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	query, err := validateReadOnlyQuery(params.SQL)
	if err != nil {
		return tool.NewErrorResult(err.Error()), nil
	}
	if params.Database != "" && params.Database != "default" && params.Database != t.databaseName {
		return tool.NewErrorResult(fmt.Sprintf("仅允许查询当前数据库 %q", t.databaseName)), nil
	}
	if t.db == nil {
		return tool.NewErrorResult("PostgreSQL 数据库连接未配置"), nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, t.queryTimeout)
	defer cancel()
	tx, err := t.db.BeginTx(queryCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return tool.NewErrorResult("开启只读事务失败: " + err.Error()), nil
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(queryCtx, fmt.Sprintf("SET LOCAL statement_timeout = %d", t.queryTimeout.Milliseconds())); err != nil {
		return tool.NewErrorResult("设置查询超时失败: " + err.Error()), nil
	}

	t.logger.Info("执行只读数据库查询", zap.String("database", t.databaseName), zap.String("sql", query))
	rows, err := tx.QueryContext(queryCtx, query)
	if err != nil {
		return tool.NewErrorResult("查询失败: " + err.Error()), nil
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return tool.NewErrorResult("读取结果列失败: " + err.Error()), nil
	}
	output := databaseQueryOutput{Columns: columns, Rows: make([][]any, 0)}
	for rows.Next() {
		if len(output.Rows) == t.maxRows {
			output.Truncated = true
			break
		}
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return tool.NewErrorResult("读取查询结果失败: " + err.Error()), nil
		}
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				values[i] = string(bytes)
			}
		}
		output.Rows = append(output.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return tool.NewErrorResult("遍历查询结果失败: " + err.Error()), nil
	}
	if err := rows.Close(); err != nil {
		return tool.NewErrorResult("关闭查询结果失败: " + err.Error()), nil
	}
	if err := tx.Commit(); err != nil {
		return tool.NewErrorResult("结束只读事务失败: " + err.Error()), nil
	}
	output.RowCount = len(output.Rows)
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("序列化数据库查询结果失败: %w", err)
	}
	return tool.NewSuccessResult(string(encoded)), nil
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
