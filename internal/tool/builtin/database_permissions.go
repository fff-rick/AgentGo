package builtin

import (
	"context"
	"database/sql"
	"fmt"
)

type databasePermissionError string

func (e databasePermissionError) Error() string { return string(e) }

// Audit effective grants, including PUBLIC and column grants; SQL text is not an access boundary.
func (t *DatabaseTool) validatePermissions(ctx context.Context, tx *sql.Tx) error {
	var unsafe bool
	err := tx.QueryRowContext(ctx, `SELECT
		current_user <> session_user OR r.rolsuper OR r.rolinherit OR r.rolcreaterole OR
		r.rolcreatedb OR r.rolreplication OR r.rolbypassrls OR
		EXISTS (SELECT 1 FROM pg_auth_members WHERE member = r.oid) OR
		EXISTS (SELECT 1 FROM pg_database WHERE datname = current_database() AND datdba = r.oid) OR
		has_database_privilege(current_database(), 'CREATE,TEMP') OR
		EXISTS (SELECT 1 FROM pg_namespace WHERE has_schema_privilege(oid, 'CREATE'))
		FROM pg_roles r WHERE rolname = current_user`).Scan(&unsafe)
	if err != nil {
		return fmt.Errorf("检查工具角色失败: %w", err)
	}
	if unsafe {
		return databasePermissionError("工具角色必须为 NOINHERIT、无角色成员关系、无管理/创建权限的独立只读登录角色")
	}

	rows, err := tx.QueryContext(ctx, `SELECT n.nspname || '.' || c.relname,
		c.relkind = 'v', has_table_privilege(c.oid, 'SELECT') AND has_schema_privilege(n.oid, 'USAGE'),
		has_table_privilege(c.oid, 'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR
		has_any_column_privilege(c.oid, 'INSERT,UPDATE,REFERENCES')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
		AND c.relkind IN ('r','p','v','m','f') AND
		(has_table_privilege(c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR
		has_any_column_privilege(c.oid, 'SELECT,INSERT,UPDATE,REFERENCES'))`)
	if err != nil {
		return fmt.Errorf("检查工具表权限失败: %w", err)
	}
	defer rows.Close()
	found := make(map[string]bool)
	for rows.Next() {
		var name string
		var view, readable, writable bool
		if err := rows.Scan(&name, &view, &readable, &writable); err != nil {
			return err
		}
		if !t.allowedViews[name] || !view || !readable || writable {
			return databasePermissionError("工具角色拥有未授权的关系权限或缺少 schema USAGE: " + name)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for name := range t.allowedViews {
		if !found[name] {
			return databasePermissionError("工具视图不存在或没有 SELECT 权限: " + name)
		}
	}

	// Custom/extension routines can hide privileged reads, even behind a SELECT or operator.
	err = tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE (n.nspname NOT IN ('pg_catalog', 'information_schema') OR p.prosecdef)
		 AND has_function_privilege(p.oid, 'EXECUTE')) OR
		EXISTS (SELECT 1 FROM pg_class WHERE CASE WHEN relkind = 'S'
		 THEN has_sequence_privilege(oid, 'USAGE,SELECT,UPDATE') ELSE false END)`).Scan(&unsafe)
	if err != nil {
		return fmt.Errorf("检查工具函数权限失败: %w", err)
	}
	if unsafe {
		return databasePermissionError("工具角色不得执行自定义/扩展函数、SECURITY DEFINER 函数或访问序列；请检查 PUBLIC 授权")
	}
	return nil
}
