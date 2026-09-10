# AI Agent 企业级智能体平台（Go 版本）

## 项目简介

基于 Go 1.22 构建的企业级 AI Agent 智能体平台，采用自研 Agent 框架，支持多模型路由、ReAct 推理、RAG 增强检索、工具调用、记忆管理等核心能力。

## 技术栈

| 组件 | 技术选型 | 说明 |
|------|---------|------|
| 语言 | Go 1.22 | 高性能、强类型、原生并发 |
| Web 框架 | Gin | 高性能 HTTP 框架 |
| Agent 框架 | 自研 | ReAct / Planner / Reflection 多模式 |
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
│           Chat Handler / Document Handler             │
├──────────────────────────────────────────────────────┤
│                  Agent 编排层                         │
│    ┌──────────┐  ┌──────────┐  ┌───────────────┐    │
│    │  ReAct   │  │ Planner  │  │  Reflection   │    │
│    │  Agent   │  │  Agent   │  │    Agent      │    │
│    └──────────┘  └──────────┘  └───────────────┘    │
├──────────────────────────────────────────────────────┤
│  ┌────────┐ ┌────────┐ ┌────────┐ ┌─────────────┐  │
│  │  RAG   │ │  Tool  │ │ Memory │ │   Intent    │  │
│  │ Engine │ │ System │ │ Manager│ │ Recognizer  │  │
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
├── agent/                   # Agent 编排（ReAct/Planner/Reflection）
├── rag/                     # RAG 检索增强生成
├── memory/                  # 记忆管理（短期/长期）
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
- PostgreSQL >= 15（当前持久化仓储尚未接入）

### 本地开发

```bash
# 首次使用可修改 .env；Compose 会启动 AgentGo、Redis、Milvus、etcd、MinIO
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

TUI 会实时分类展示意图识别、执行阶段、ReAct 显式 Thought、工具调用与结果、RAG 引用、错误和最终答案，也能直接导入宿主机上的 Markdown 文件：

```text
/import /home/xin/docs/knowledge.md
/import "~/docs/path with spaces.md"
```

文件限制为 UTF-8 编码、`.md`/`.markdown` 后缀且不超过 10 MiB。模型答案和显式推理会逐段流式显示；执行期间底部展示加载动画和耗时。使用 `↑`/`↓`、`PgUp`/`PgDn`、`Home`/`End` 或鼠标滚轮查看历史，`Esc` 可取消当前请求或导入，`/clear` 创建新会话，`Ctrl+C` 退出。也可直接观察 SSE 事件：

```bash
curl -N http://localhost:8080/api/v1/chat/stream \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"demo","message":"从知识库介绍 AgentGo"}'
```

流式 LLM 客户端兼容 `reasoning_content`、`reasoning` 和 `thinking` 三种显式推理字段。标准模型没有这些字段时，TUI 仍会展示 Agent 的阶段状态，但不会伪造推理内容。

若只在宿主机运行 Go 服务，需要先准备 Redis 和 Milvus；`make run` 会自动加载 `.env`：

```bash
docker compose up -d redis milvus-standalone
make run
```

默认 `.env` 使用宿主机 Ollama 的 `qwen2.5:7b` 生成回答、`bge-m3:latest` 生成 1024 维向量。Docker 通过 `host.docker.internal` 访问 Ollama：

```bash
# Docker 容器通过 host.docker.internal 访问宿主机 Ollama
make docker-run

# 或直接在宿主机运行 AgentGo
APP_LLM_BASE_URL=http://localhost:11434 \
APP_EMBEDDING_BASE_URL=http://localhost:11434 \
APP_LLM_MODEL=qwen2.5:7b make run
```

云端模型可同时设置 `APP_LLM_API_KEY`。完整多模型配置继续使用 `config.yaml` 中的 `llm.models`。

### 环境变量

项目根目录的 `.env` 是本机配置且已被 Git 忽略；[`.env.example`](.env.example) 是可提交的配置模板。主要变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `APP_REDIS_ADDR` | `localhost:6379` | Redis 地址；Compose 内自动改为 `redis:6379` |
| `APP_MILVUS_ADDR` | `localhost:19530` | Milvus gRPC 地址；Compose 内自动改为 `milvus-standalone:19530` |
| `APP_MILVUS_DATABASE` | `default` | Milvus database |
| `APP_MILVUS_COLLECTION_NAME` | `documents_bge_m3` | 默认 collection，启动时自动创建 |
| `APP_MILVUS_DIMENSION` | `1024` | Milvus 向量维度，必须与 embedding 输出一致 |
| `APP_MILVUS_METRIC_TYPE` | `COSINE` | `COSINE`、`L2` 或 `IP` |
| `APP_MILVUS_CONNECT_TIMEOUT` | `60s` | 启动连接和 collection 初始化超时 |
| `APP_LLM_BASE_URL` | `http://host.docker.internal:11434` | Ollama 的 OpenAI-compatible API 根地址 |
| `APP_LLM_MODEL` | `qwen2.5:7b` | 模型 ID |
| `APP_EMBEDDING_BASE_URL` | `http://host.docker.internal:11434` | Ollama 原生 API 根地址 |
| `APP_EMBEDDING_MODEL` | `bge-m3:latest` | embedding 模型 |
| `APP_EMBEDDING_DIMENSION` | `1024` | embedding 输出维度 |
| `APP_RAG_SCORE_THRESHOLD` | `0.5` | 最低相关性分数（0–1），低于该值的片段不会进入回答上下文 |
| `APP_RAG_ENABLE_RERANK` | `true` | 是否使用 LLM 对向量召回结果重排 |
| `APP_SERVER_WRITE_TIMEOUT` | `300s` | 本地模型完整请求的写超时 |
| `APP_AGENT_ENABLE_REFLECTION` | `false` | 是否额外调用一次模型反思答案 |

文档上传会经过分块、Ollama 批量向量化并写入 Milvus；RAG 查询和长期记忆使用同一个 embedding 模型。验证 Milvus 数据链路：

```bash
make test-milvus
```

### Docker 部署

```bash
# 仅构建本地镜像 ai-agent-go:local
make docker-build

# 构建并启动应用、Redis、Milvus、etcd 和 MinIO
make docker-run
```

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/chat` | 对话（同步） |
| POST | `/api/v1/chat/stream` | 对话（SSE 流式） |
| POST | `/api/v1/documents` | 上传文档 |
| POST | `/api/v1/documents/import` | multipart 上传本地 Markdown，文件字段为 `file` |
| GET  | `/api/v1/documents/:id` | 查询文档状态 |
| GET  | `/health` | 健康检查 |

## 设计亮点

1. **三态熔断器**：支持 Closed/Open/HalfOpen 三种状态，保护 LLM 调用链路
2. **多模型路由**：根据任务复杂度智能选择模型，兼顾成本和效果
3. **ReAct 推理循环**：Thought → Action → Observation 迭代式推理
4. **混合检索**：向量检索 + 关键词检索 + Rerank 重排序
5. **分层记忆**：短期记忆（Redis）+ 长期记忆（PostgreSQL + Milvus）
6. **工具系统**：基于 Go interface 的插件化工具注册和调度
7. **优雅关停**：信号监听 + Context 取消传播 + 超时等待
8. **可观察执行**：SSE 事件流 + Bubble Tea TUI，展示意图、阶段、显式推理、工具和 RAG 引用

## Benchmark

项目包含可复现的离线微基准，以及基于公开 HTTP API 的效果/性能评测 runner：

```bash
make benchmark       # 无外部依赖：ETL、工具路由、ReAct 解析、RRF、熔断器
make benchmark-e2e   # 对已启动的 localhost:8080 运行 smoke 数据集
```

完整指标、Golden Dataset 规范、发布门禁与已知边界见 [Benchmark 设计](docs/BENCHMARK.md)。

## 许可证

MIT License
