# 开发导航

## 技术栈

| 组件 | 技术选型 | 说明 |
|------|---------|------|
| 语言 | Go 1.22 | 高性能、强类型、原生并发 |
| Web 框架 | Gin | 高性能 HTTP 框架 |
| Agent 框架 | 自研（借鉴PI Agent） | AgentHarness / AgentLoop / Function Calling / Reflection |
| 向量数据库 | Milvus | 高性能向量检索 |
| 缓存 | Redis | 会话管理 & 语义缓存 |
| 关系数据库 | PostgreSQL | 持久化存储 |
| 链路追踪 | OpenTelemetry | 全链路可观测 |

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
