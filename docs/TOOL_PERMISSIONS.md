# 数据库与文件工具权限（F01）

默认不注册 `database_query`，文件工具也默认关闭。应用 PostgreSQL 连接只用于文档索引、记忆等内部存储，不能传给 SQL 工具。

## 数据库工具

配置 `APP_DATABASE_TOOL_DSN`（独立连接池）和 `APP_DATABASE_TOOL_ALLOWED_VIEWS`（逗号分隔的 `schema.view`）。未配置 DSN 时不注册工具；已配置但连接、视图或权限检查失败时启动失败，不回退到应用账号。原 `APP_POSTGRES_QUERY_TIMEOUT/MAX_ROWS` 已迁移为 `APP_DATABASE_TOOL_QUERY_TIMEOUT/MAX_ROWS`。

专用角色必须是 `NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`，不能有任何角色成员关系，也不能拥有数据库、schema 创建权限或临时表创建权限。除了 PostgreSQL 自带的系统目录，只允许对列出的普通视图有完整 SELECT 授权，不允许对其他表、分区表、物化视图、外部表或序列有权限；列级授权、PUBLIC 授权同样检查。自定义/扩展函数及 SECURITY DEFINER 函数的有效 EXECUTE 权限一律拒绝。启动及每次查询前重新检查，后续误授权会使工具拒绝执行。

PostgreSQL 默认向 PUBLIC 授予函数执行权限，仅给新角色声明“只读”不够；需要收紧已有及未来函数的授权。依据：[PostgreSQL 权限说明](https://www.postgresql.org/docs/16/ddl-priv.html)、[SECURITY DEFINER 函数安全](https://www.postgresql.org/docs/16/sql-createfunction.html)。

以应用对象的创建者（同时具有 CREATEROLE 权限）连接目标数据库，执行一次 [tool_role.sql](../deploy/postgres/tool_role.sql)。脚本从 `AGENTGO_TOOL_DB_PASSWORD` 环境变量读取密码。示例命令中的 `APP_OWNER_DSN` 应在服务端管理环境中设置，不要把应用所有者密码提供给工具：

```bash
psql "$APP_OWNER_DSN" -f deploy/postgres/tool_role.sql
```

脚本收回当前数据库的 PUBLIC CREATE/TEMP、公有 schema 的创建权及已有表、序列、函数的 PUBLIC 授权，会影响依赖 PUBLIC 的其他账号，应为它们补充明确授权。其他业务 schema、其他对象创建者的默认权限也需同样处理；检查器不会忽略它们。脚本不会自动创建或开放任何业务视图。

DBA 审查视图定义、底层来源、函数依赖和视图所有者后，逐个创建并授权。例如，业务明确允许所有工具用户读取共享文档状态计数时：

```sql
CREATE VIEW tool_api.document_summary AS
SELECT status, count(*) AS document_count FROM public.documents GROUP BY status;
GRANT SELECT ON tool_api.document_summary TO agentgo_tool_reader;
```

然后设置独立账号 DSN，并设置 `APP_DATABASE_TOOL_ALLOWED_VIEWS=tool_api.document_summary`。不要使用 `GRANT ... ON ALL TABLES`，不要赋予应用角色成员关系，也不要把个人记忆、记忆版本或任务包装成视图来规避边界。允许列表表示管理员已审查的共享数据，检查器不会证明视图的业务含义或自动建立用户隔离。个人记忆继续使用现有带用户校验的记忆接口；私有业务数据开放前需要另行设计服务端身份绑定和数据库行级权限。

默认限制：查询 10 秒、100 行、SQL 16 KiB、JSON 编码后单元格/列名 64 KiB、完整 JSON 结果 1 MiB。行数或总结果超限返回完整行并设置 `truncated=true`；单元格或列信息超限返回固定错误。日志只记录查询 SHA-256、耗时和错误类别，工具路由不记录 SQL 输入，错误响应不回显 PostgreSQL 中可能包含常量的详细错误。数据库服务器自身的 SQL/错误审计日志由 DBA 单独配置。

字节限制约束工具输出；PostgreSQL 执行表达式、驱动接收单行仍可能临时分配更大内存。对不可信高负载查询应使用独立数据库资源配额，不把这个输出上限当作数据库内存配额。

## 文件工具

设置 `APP_FILESYSTEM_ENABLED=true` 和专用的绝对路径 `APP_FILESYSTEM_WORKSPACE_ROOT`；根目录必须已存在且不能是 `/`。Compose 默认使用 `./data/workspaces:/workspace`，不挂载项目根目录。多用户部署须同时启用 OIDC，关闭认证时所有请求共用本地用户。

服务端按上下文 UserID 的 UTF-8 字节计算 SHA-256 十六进制字符串，分配 `workspace_root/<hash>`（不存在时以 0700 创建）。路径可以是该目录内的绝对路径或相对路径；`file_inspect` 使用 `path: "."` 列出用户根目录。请求中的目录配置或用户 ID 不参与目录分配，读、搜索、预览、提交都复核目录边界；符号链接一律拒绝，包括指向目录内的链接。由运维将文件放到对应用户目录，不要使用符号链接共享资料。

proposal 仍绑定用户、会话、批准 ID 和原文件 SHA-256。提交会重新检查路径，在同一进程内按规范路径加锁，锁覆盖“复核哈希 → 原子替换”，两个不同 proposal 不能同时提交同一份旧内容。

所有文件访问均通过持有的用户目录 `os.Root` 句柄完成：遍历、读取、创建临时文件和 Rename 不依赖先前检查过的绝对路径；即使检查后替换父目录为外部符号链接，实际 I/O 也不能越出用户根目录。该实现使用 Go 1.25 的 `Root.Rename`，项目与构建镜像已同步升级。依据：[Go 标准库的目录边界防护](https://go.dev/blog/osroot)。不提供安全目录句柄的 JS/Plan 9 平台禁用文件工具。

**部署边界：** 顶层工作区和用户根目录的分配仍由服务端管理。`os.Root` 不隔离硬链接、挂载点或设备节点，因此不应将可创建这些对象的不可信程序放进同一服务权限域。应用内锁不解决多个 AgentGo 进程共享目录或外部程序同时改写同一文件内容的冲突；这些部署需要跨进程锁和写入所有权管理。

## 验收

```bash
go test ./...
go test -race ./internal/tool/... ./internal/config ./cmd/server
F01_POSTGRES_ADMIN_DSN='postgres://admin@127.0.0.1:5432/postgres?sslmode=disable' \
  go test ./internal/tool/builtin -run TestDatabaseToolRestrictedRoleIntegration -v -count=1
```

最后一项需要可创建数据库及角色的测试管理员连接；测试自动创建随机命名的隔离数据库与登录角色，结束后删除，不使用应用库，不通过 `SET ROLE` 模拟受限连接。未设置变量时会明确跳过，普通单测通过不能替代此项。

数据库验收涵盖获准视图、内部表直读/JOIN/子查询、SECURITY DEFINER、角色继承/PUBLIC 列级授权变更、行数/字节/超时和日志常量脱敏。文件验收涵盖跨用户/跨会话 proposal、无批准、重放、目录穿越、跨用户目录、递归搜索中的符号链接、预览后替换文件/父目录、内容变更，以及相同/不同 proposal 的并发提交，另有确定性测试在路径检查后替换父目录再执行读、列目录、搜索和写入。
