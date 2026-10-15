package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/enterprise/ai-agent-go/internal/config"
)

// Uses a disposable database and a real password-authenticated restricted login, never SET ROLE.
func TestDatabaseToolRestrictedRoleIntegration(t *testing.T) {
	adminDSN := os.Getenv("F01_POSTGRES_ADMIN_DSN")
	if adminDSN == "" {
		t.Skip("set F01_POSTGRES_ADMIN_DSN to a PostgreSQL admin URL; creates/drops an isolated database and role")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	dbName, role, password := "f01_"+suffix, "f01_reader_"+suffix, uuid.NewString()
	execSQL := func(db *sql.DB, query string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatalf("fixture SQL failed: %v", err)
		}
	}
	execSQL(admin, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`, role, password))
	defer admin.ExecContext(context.Background(), "DROP ROLE "+role)
	execSQL(admin, "CREATE DATABASE "+dbName)
	defer admin.ExecContext(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
	u, err := url.Parse(adminDSN)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("admin DSN must be a postgres URL")
	}
	u.Path = "/" + dbName
	owner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	execSQL(owner, fmt.Sprintf(`
 REVOKE CREATE,TEMPORARY ON DATABASE %s FROM PUBLIC;
 REVOKE CREATE ON SCHEMA public FROM PUBLIC;
 CREATE TABLE public.memories (secret text);
 INSERT INTO public.memories VALUES ('private-memory');
 CREATE TABLE public.memory_versions (secret text);
 CREATE TABLE public.memory_jobs (secret text);
 CREATE TABLE public.products (name text);
 INSERT INTO public.products VALUES ('approved-product');
 CREATE SCHEMA tool_api;
 CREATE VIEW tool_api.products AS SELECT name FROM public.products;
 CREATE FUNCTION public.leak() RETURNS text LANGUAGE sql SECURITY DEFINER AS 'SELECT secret FROM public.memories LIMIT 1';
 REVOKE ALL ON FUNCTION public.leak() FROM PUBLIC;
 GRANT USAGE ON SCHEMA tool_api TO %s;
 GRANT SELECT ON tool_api.products TO %s;
 `, dbName, role, role))
	u.User = url.UserPassword(role, password)
	cfg := config.DatabaseToolConfig{DSN: u.String(), AllowedViews: []string{"tool_api.products"}, QueryTimeout: time.Second, MaxRows: 3, MaxSQLBytes: 256, MaxCellBytes: 64, MaxResultBytes: 200}
	core, logs := observer.New(zap.InfoLevel)
	queryTool, err := NewDatabaseTool(ctx, cfg, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	defer queryTool.Close()
	var currentUser, sessionUser string
	if err := queryTool.db.QueryRowContext(ctx, "SELECT current_user, session_user").Scan(&currentUser, &sessionUser); err != nil || currentUser != role || sessionUser != role {
		t.Fatalf("not a restricted login: %q %q %v", currentUser, sessionUser, err)
	}
	// Verify database ACLs independently of the tool's SQL guard.
	for _, query := range []string{
		"SELECT * FROM public.memories", "SELECT * FROM public.memory_versions", "SELECT * FROM public.memory_jobs",
		"SELECT * FROM tool_api.products JOIN public.memories ON true",
		"SELECT (SELECT secret FROM public.memories LIMIT 1) FROM tool_api.products",
		"SELECT public.leak()", "SELECT * FROM public.products",
		"SELECT pg_read_file('/etc/passwd')",
		"INSERT INTO tool_api.products VALUES ('injected')",
	} {
		rows, err := queryTool.db.QueryContext(ctx, query)
		if rows != nil {
			rows.Close()
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("restricted role query %q: want permission denied, got %v", query, err)
		}
	}
	for _, tc := range []struct {
		name, query        string
		success, truncated bool
		category           string
	}{
		{"approved view", "SELECT name FROM tool_api.products", true, false, ""},
		{"internal join", "SELECT * FROM tool_api.products JOIN public.memories ON true", false, false, "sqlstate_42501"},
		{"rows", "SELECT generate_series(1,10)", true, true, ""},
		{"bytes", "SELECT repeat('x',40) FROM generate_series(1,10)", true, true, ""},
		{"escaped cell", "SELECT repeat('<',12)", false, false, "cell_limit"},
		{"large cell", "SELECT repeat('x',65)", false, false, "cell_limit"},
		{"sql length", "SELECT 1" + strings.Repeat(" ", 257), false, false, "sql_limit"},
		{"timeout", "SELECT pg_sleep(2)", false, false, ""},
		{"error privacy", "SELECT 'sensitive-SQL-literal'::integer", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := queryTool.Execute(ctx, mustJSON(t, databaseQueryParams{SQL: tc.query}))
			if err != nil || result.Success != tc.success {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if strings.Contains(result.Error, "sensitive-SQL-literal") {
				t.Fatal("SQL literal leaked in error")
			}
			if tc.success {
				if len(result.Output) > cfg.MaxResultBytes {
					t.Fatalf("output bytes=%d", len(result.Output))
				}
				var output databaseQueryOutput
				if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
					t.Fatal(err)
				}
				if output.Truncated != tc.truncated || output.RowCount > cfg.MaxRows {
					t.Fatalf("output=%+v", output)
				}
			}
			if tc.category != "" && logs.All()[logs.Len()-1].ContextMap()["error_category"] != tc.category {
				t.Fatalf("logs=%+v", logs.All())
			}
		})
	}
	originalLimit := queryTool.cfg.MaxResultBytes
	queryTool.cfg.MaxResultBytes = 10
	tooSmall, err := queryTool.Execute(ctx, `{"sql":"SELECT name FROM tool_api.products"}`)
	if err != nil || tooSmall.Success {
		t.Fatalf("column metadata limit not enforced: %+v %v", tooSmall, err)
	}
	queryTool.cfg.MaxResultBytes = originalLimit
	wrongDatabase, err := queryTool.Execute(ctx, `{"sql":"SELECT 1","database":"another_database"}`)
	if err != nil || wrongDatabase.Success {
		t.Fatalf("database override accepted: %+v %v", wrongDatabase, err)
	}
	for _, entry := range logs.All() {
		encoded, _ := json.Marshal(entry.ContextMap())
		if strings.Contains(string(encoded), "sensitive-SQL-literal") || strings.Contains(string(encoded), "SELECT") {
			t.Fatalf("SQL leaked in log: %s", encoded)
		}
	}
	// Constructor rejects unsafe roles, hidden column grants, and PUBLIC routine grants.
	unsafeCfg := cfg
	unsafeCfg.DSN = ownerDSN(adminDSN, dbName)
	if unexpected, err := NewDatabaseTool(ctx, unsafeCfg, zap.NewNop()); err == nil {
		unexpected.Close()
		t.Fatal("owner/superuser accepted")
	}
	for _, tc := range []struct{ grant, revoke string }{
		{"GRANT SELECT (secret) ON public.memories TO PUBLIC", "REVOKE SELECT (secret) ON public.memories FROM PUBLIC"},
		{"GRANT EXECUTE ON FUNCTION public.leak() TO PUBLIC", "REVOKE EXECUTE ON FUNCTION public.leak() FROM PUBLIC"},
		{"GRANT SELECT ON public.memory_jobs TO " + role, "REVOKE SELECT ON public.memory_jobs FROM " + role},
		{"GRANT UPDATE (name) ON tool_api.products TO " + role, "REVOKE UPDATE (name) ON tool_api.products FROM " + role},
		{"ALTER ROLE " + role + " INHERIT", "ALTER ROLE " + role + " NOINHERIT"},
		{"GRANT pg_read_all_data TO " + role, "REVOKE pg_read_all_data FROM " + role},
	} {
		execSQL(owner, tc.grant)
		unexpected, err := NewDatabaseTool(ctx, cfg, zap.NewNop())
		if err == nil {
			unexpected.Close()
			t.Fatalf("unsafe grants accepted: %s", tc.grant)
		}
		result, err := queryTool.Execute(ctx, `{"sql":"SELECT name FROM tool_api.products"}`)
		if err != nil || result.Success {
			t.Fatalf("running tool accepted changed grants: %+v %v", result, err)
		}
		execSQL(owner, tc.revoke)
	}
}

func ownerDSN(adminDSN, dbName string) string {
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + dbName
	return u.String()
}
