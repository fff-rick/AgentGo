# AI Agent 企业级智能体平台（Go 版本）

## 项目简介

基于 Go 1.22 构建的企业级 AI Agent 智能体平台，采用自研 Agent 框架，支持多模型路由、原生 Function Calling、RAG 增强检索、工具调用、记忆管理等核心能力。

## 技术栈

| 组件 | 技术选型 | 说明 |
|------|---------|------|
| 语言 | Go 1.22 | 高性能、强类型、原生并发 |
| Web 框架 | Gin | 高性能 HTTP 框架 |
| Agent 框架 | 自研 | AgentHarness / AgentLoop / Function Calling / Reflection |
| 向量数据库 | Milvus | 高性能向量检索 |
| 缓存 | Redis | 会话管理 & 语义缓存 |
| 关系数据库 | PostgreSQL | 持久化存储 |
| 链路追踪 | OpenTelemetry | 全链路可观测 |

## 核心架构

```
┌──────────────────────────────────────────────────────┐
│                    API 网关层 (Gin)                    │
├──────────────────────────────────────────────────────┤
│                   Handler 处理层                      │
│       Session / Chat / Document Handler               │
├──────────────────────────────────────────────────────┤
│             AgentHarness 生命周期管理层               │
│   Context Builder → AgentLoop → Hooks → Memory       │
├──────────────────────────────────────────────────────┤
│  ┌────────┐ ┌────────┐ ┌────────┐ ┌─────────────┐  │
│  │RAG Tool│ │ Tools  │ │3-Layer │ │ Reflection  │  │
│  │       │ │ System │ │ Memory │ │    Hook     │  │
│  └────────┘ └────────┘ └────────┘ └─────────────┘  │
├──────────────────────────────────────────────────────┤
│  ┌────────┐ ┌────────┐ ┌────────┐ ┌─────────────┐  │
│  │  LLM   │ │ Milvus │ │ Redis  │ │ PostgreSQL  │  │
│  │ Router │ │ Client │ │ Cache  │ │   Client    │  │
│  └────────┘ └────────┘ └────────┘ └─────────────┘  │
└──────────────────────────────────────────────────────┘
```

## 目录结构

```
cmd/server/main.go          # 程序入口
internal/
├── config/                  # 配置管理
├── handler/                 # HTTP 处理器
├── router/                  # 路由注册
├── agent/                   # 兼容适配器、Planner 和 Reflection Hook
├── agentloop/               # 模型自主决策与工具调用循环
├── harness/                 # 单次 Agent Run 生命周期管理
├── agentcontext/            # 上下文构建、预算估算与压缩
├── rag/                     # RAG 检索增强生成
├── memory/                  # Session / Semantic Memory
├── user/                    # 无认证用户资料与会话归属
├── tool/                    # 工具系统（注册/路由/内置工具）
├── intent/                  # 意图识别
├── llm/                     # LLM 客户端（多模型路由/熔断）
├── vectordb/                # 向量数据库客户端
├── cache/                   # Redis 缓存
├── trace/                   # 链路追踪
├── etl/                     # 文档 ETL 流水线
└── model/                   # 数据模型定义
pkg/common/                  # 公共工具包
```

## 快速开始

### 环境要求

- Go >= 1.22
- Redis >= 7.0
- Docker Desktop + WSL integration（使用容器启动时）
- Milvus 2.5.x（Compose 会连同 etcd、MinIO 一起启动）
- PostgreSQL >= 15（文档关键词索引与只读数据库工具）

### 本地开发

```bash
# 首次使用可修改 .env；Compose 会启动 AgentGo、PostgreSQL、Redis、Milvus、etcd、MinIO
make docker-run
curl http://localhost:8080/health

# 查看日志 / 停止服务
make docker-logs
make docker-stop
```

### 可观察 TUI

服务启动后，在另一个终端运行：

```bash
make tui
```

TUI 会实时展示 Agent Loop 阶段、模型显式 reasoning、原生 Function Calling 及结果、RAG 引用、错误和最终答案，也能直接导入宿主机上的 Markdown、PDF、DOCX 和 XLSX 文件：

```text
/import /home/xin/docs/knowledge.md
/import "~/docs/path with spaces.md"
/import /home/xin/docs/orders.xlsx
```

Markdown 必须为 UTF-8 且不超过 10 MiB；PDF、DOCX、XLSX 不超过 50 MiB。文件导入异步执行，TUI 会轮询状态并展示精确的页码或 Excel 单元格范围。模型答案和显式推理会逐段流式显示；执行期间底部展示加载动画和耗时。使用 `↑`/`↓`、`PgUp`/`PgDn`、`Home`/`End` 或鼠标滚轮查看历史，`Esc` 可取消当前请求或导入，`/plan <任务>` 显式启用 Planner-Executor，`/clear` 创建新会话，`Ctrl+C` 退出。Planner 模式由调用方指定，与意图识别无关。TUI 使用 `AGENTGO_USER_ID`（默认 `local-user`）创建会话。也可先通过 API 创建会话，再观察 SSE 事件：

```bash
SESSION_ID=$(curl -s http://localhost:8080/api/v1/sessions \
  -H 'Content-Type: application/json' \
  -d '{"user":{"user_id":"demo-user","display_name":"Demo"}}' \
  | jq -r '.data.session_id')

curl -N http://localhost:8080/api/v1/chat/stream \
  -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SESSION_ID\",\"message\":\"从知识库介绍 AgentGo\"}"
```

流式 LLM 客户端兼容 `reasoning_content`、`reasoning` 和 `thinking` 三种显式推理字段。标准模型没有这些字段时，TUI 仍会展示 Agent 的阶段状态，但不会伪造推理内容。

若只在宿主机运行 Go 服务，需要先准备 PostgreSQL、Redis 和 Milvus；`make run` 会自动加载 `.env`：

```bash
docker compose up -d postgres redis milvus-standalone
make run
```

[`config.yaml`](config.yaml) 的 `llm.models` 同时注册 `gpt-5.5` 和本地 `qwen2.5:7b`。统一 Agent Loop 默认按 `priority` 选择模型，并由该模型自主决定直接回答或调用工具；`agent.tool_model` 仅供兼容的 ReActAgent 使用。`bge-m3:latest` 继续生成 1024 维向量：

```bash
# Docker 容器通过 host.docker.internal 访问宿主机 Ollama
make docker-run

# 或直接在宿主机运行 AgentGo（把本地服务地址改为 localhost）
APP_QWEN_BASE_URL=http://localhost:11434 \
APP_EMBEDDING_BASE_URL=http://localhost:11434 make run
```

模型地址和密钥通过环境变量注入，模型名称、真实模型 ID 和优先级统一维护在 `config.yaml` 的 `llm.models` 中。指定模型熔断时也会按相同优先级选择其他健康模型。

### 环境变量

项目根目录的 `.env` 是本机配置且已被 Git 忽略；[`.env.example`](.env.example) 是可提交的配置模板。主要变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `APP_REDIS_ADDR` | `localhost:6379` | Redis 地址；Compose 内自动改为 `redis:6379` |
| `APP_POSTGRES_HOST` | `localhost` | PostgreSQL 地址；Compose 内自动改为 `postgres` |
| `APP_POSTGRES_DBNAME` | `ai_agent` | 文档全文索引和数据库工具使用的数据库 |
| `APP_POSTGRES_QUERY_TIMEOUT` | `10s` | `database_query` 单次 SELECT 的最大执行时间 |
| `APP_POSTGRES_MAX_ROWS` | `100` | `database_query` 最多返回的数据行数 |
| `APP_MILVUS_ADDR` | `localhost:19530` | Milvus gRPC 地址；Compose 内自动改为 `milvus-standalone:19530` |
| `APP_MILVUS_DATABASE` | `default` | Milvus database |
| `APP_MILVUS_COLLECTION_NAME` | `documents_bge_m3` | 默认 collection，启动时自动创建 |
| `APP_MILVUS_DIMENSION` | `1024` | Milvus 向量维度，必须与 embedding 输出一致 |
| `APP_MILVUS_METRIC_TYPE` | `COSINE` | `COSINE`、`L2` 或 `IP` |
| `APP_MILVUS_CONNECT_TIMEOUT` | `60s` | 启动连接和 collection 初始化超时 |
| `APP_LLM_BASE_URL` | `https://api.openai.com` | 主模型 API 根地址 |
| `APP_LLM_API_KEY` | 空 | 主模型 API 密钥 |
| `APP_QWEN_BASE_URL` | `http://host.docker.internal:11434` | 本地 Qwen 的 OpenAI-compatible API 根地址 |
| `APP_EMBEDDING_BASE_URL` | `http://host.docker.internal:11434` | Ollama 原生 API 根地址 |
| `APP_EMBEDDING_MODEL` | `bge-m3:latest` | embedding 模型 |
| `APP_EMBEDDING_DIMENSION` | `1024` | embedding 输出维度 |
| `APP_RAG_SCORE_THRESHOLD` | `0.5` | 最低相关性分数（0–1），低于该值的片段不会进入回答上下文 |
| `APP_RAG_ENABLE_RERANK` | `true` | 是否使用 LLM 对向量召回结果重排 |
| `APP_DOCUMENT_DOCLING_URL` | `http://localhost:5001` | PDF 版面分析与 OCR 服务地址；Compose 内自动改为 `http://docling:5001` |
| `APP_DOCUMENT_PARSE_TIMEOUT` | `10m` | 单个 PDF 的 Docling 解析超时 |
| `APP_SEARCH_BASE_URL` | `http://localhost:7070` | SearXNG 地址；Compose 内自动改为 `http://searxng:8080` |
| `APP_SEARCH_TIMEOUT` | `20s` | 单次真实网络搜索超时 |
| `APP_SEARCH_LANGUAGE` | `zh-CN` | 搜索结果语言 |
| `APP_SEARCH_SAFE_SEARCH` | `1` | SearXNG 安全搜索级别：0 关闭、1 适中、2 严格 |
| `APP_SERVER_WRITE_TIMEOUT` | `300s` | 本地模型完整请求的写超时 |
| `APP_AGENT_ENABLE_REFLECTION` | `false` | 是否额外调用一次模型反思答案 |
| `APP_MEMORY_SESSION_TTL` | `720h` | 用户、会话和完整原始消息的滑动 TTL |
| `APP_MEMORY_SEMANTIC_COLLECTION` | `semantic_memory_v1` | 用户隔离的长期语义记忆 collection |
| `APP_CONTEXT_MAX_INPUT_TOKENS` | `30000` | 触发会话压缩的估算输入预算 |
| `APP_CONTEXT_RECENT_MESSAGES` | `20` | 压缩后优先保留的最近消息数 |
| `APP_TOOLS_LAZY_LOAD_THRESHOLD` | `3` | 每个会话最多保留的业务工具 Schema 数量 |
| `APP_TOOLS_MAX_DISCOVERY_CALLS` | `4` | 单次 Agent Run 最多允许的 `list_tools` 控制调用次数 |

文档上传会按规范化后的内容类型和标题生成稳定文档 ID，并按原始内容生成 ContentHash；同一文档内容未变化时跳过处理，内容变化时覆盖 PostgreSQL 索引和 Milvus 向量并清理多余旧分块。分块经 Ollama 批量向量化后 Upsert 到 Milvus，同时通过纯 Go 中文分词写入 PostgreSQL 倒排词频索引并使用 BM25 排序。RAG 默认并发执行两路召回并通过 RRF 融合；任一路暂时失败时会降级到另一路。`database_query` 仅接受单条 SELECT，并在 PostgreSQL 只读事务中执行。RAG 查询和长期记忆使用同一个 embedding 模型。

工具 Schema 按会话惰性加载：每轮默认只携带 `list_tools` 和最多 3 个最近使用的业务工具。`options.tools` 缺省或为 `null` 时允许全部注册工具，显式 `[]` 时禁用全部业务工具，非空数组作为工具允许列表；未知工具名会返回 HTTP 400。

从旧版“按内容生成文档 ID”升级时，需要先清空 PostgreSQL 文档索引和配置的 Milvus 文档 collection，再重新导入知识库；服务不会自动删除存量知识。验证 Milvus 数据链路：

```bash
make test-milvus
```

### Docker 部署

```bash
# 仅构建本地镜像 ai-agent-go:local
make docker-build

# 构建并启动应用、PostgreSQL、Redis、Milvus、etcd、MinIO 和 SearXNG
make docker-run
```

`web_search` 会调用 Compose 内的私有 SearXNG 聚合真实搜索结果，并把标题、链接、摘要、来源引擎和发布时间作为原生 tool 消息返回给模型。宿主机运行 `make run` 时，需要先启动搜索服务：

```bash
docker compose up -d searxng
```

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/sessions` | 使用客户端提供的用户信息创建会话 |
| POST | `/api/v1/chat` | 对话（同步） |
| POST | `/api/v1/chat/stream` | 对话（SSE 流式） |
| POST | `/api/v1/documents` | 上传文档 |
| POST | `/api/v1/documents/import` | 异步 multipart 上传 Markdown/PDF/DOCX/XLSX，文件字段为 `file` |
| GET  | `/api/v1/documents/:id` | 查询文档状态 |
| GET  | `/health` | 健康检查 |

聊天请求可通过 `"options":{"mode":"planner"}` 显式启用 Planner-Executor；省略或使用 `agent` 时进入默认 AgentLoop。

## 设计亮点

1. **三态熔断器**：支持 Closed/Open/HalfOpen 三种状态，保护 LLM 调用链路
2. **多模型路由**：根据任务复杂度智能选择模型，兼顾成本和效果
3. **统一 Agent Loop**：模型可在同一次 Run 中直接回答，或组合调用知识库、搜索、计算和数据库工具
4. **混合检索**：Milvus 向量检索 + PostgreSQL 中文分词/BM25 + RRF 融合 + Rerank 重排序
5. **三层记忆**：Redis Session Memory、Milvus Semantic Memory、单次 Run Working Memory，并支持上下文压缩
6. **工具系统**：基于 Go interface 的插件化工具注册和调度
7. **优雅关停**：信号监听 + Context 取消传播 + 超时等待
8. **可观察执行**：SSE 事件流 + Bubble Tea TUI，展示 Loop 阶段、显式推理、工具和 RAG 引用

## Benchmark

项目包含可复现的离线微基准，以及基于公开 HTTP API 的效果/性能评测 runner：

```bash
make benchmark       # 无外部依赖：ETL、工具路由、RRF、熔断器
make benchmark-e2e   # 对已启动的 localhost:8080 运行 smoke 数据集
```

完整指标、Golden Dataset 规范、发布门禁与已知边界见 [Benchmark 设计](docs/BENCHMARK.md)。

## 许可证

MIT License
