# AgentGo Benchmark 设计

## 1. 目标

Benchmark 同时回答四个问题：

1. **效果**：Agent 是否完成任务，而非只生成看似合理的文本？
2. **性能**：在不同并发下，延迟、吞吐和资源占用是否满足目标？
3. **可靠性**：依赖超时、模型失败和高并发时能否正确降级？
4. **成本**：一次成功任务消耗多少模型 token、工具调用和时间？

每次实验必须固定代码提交、数据集版本、模型及参数、Prompt 版本、依赖版本和机器规格。不要把不同变量同时变化的结果直接比较。

## 2. 两层评测

### 2.1 离线微基准

微基准不依赖 Redis、Milvus 或真实 LLM，覆盖 AgentGo 自身的热点路径：

| 组件 | 场景 | 主要指标 |
|---|---|---|
| ETL | 10 KiB / 1 MiB，中英文，三种分块策略 | ns/op、MB/s、B/op、allocs/op |
| Tool Registry | 10 / 1k / 10k 工具，串行及并行查找 | ns/op、allocs/op |
| Tool Router | 并行 no-op 工具调度 | ns/op、allocs/op |
| RAG RRF | 20 / 200 / 2k 候选融合 | ns/op、B/op、allocs/op |
| Circuit Breaker | 并行 Allow | ns/op、锁竞争、allocs/op |

运行：

```bash
make benchmark
```

保存稳定基线并比较：

```bash
go test -run '^$' -bench . -benchmem -count 10 ./... > old.txt
# 修改代码后
go test -run '^$' -bench . -benchmem -count 10 ./... > new.txt
benchstat old.txt new.txt
```

在同一台空闲机器上运行；预热后至少采样 10 次。本轮只记录 `ns/op` 与 `allocs/op` 基线，不设置统一阻断阈值。

### 2.2 黑盒端到端评测

`cmd/benchmark` 只调用公开的 `/api/v1/chat`，可直接比较本地、预发、模型或 Prompt 版本。JSONL 每行是一条独立用例；`turns` 用于同一会话的顺序多轮测试。

```json
{
  "id": "calculator-001",
  "category": "tool",
  "message": "计算 6*7",
  "expected": {
    "keywords_all": ["42"],
    "keywords_any": ["结果", "答案"],
    "forbidden": ["无法计算"],
    "tool": "calculator",
    "reference_doc_ids": ["doc-001"],
    "max_latency_ms": 15000
  }
}
```

支持的确定性断言：

| 字段 | 含义 |
|---|---|
| `keywords_all` | 最终答案必须包含全部文本，忽略英文大小写 |
| `keywords_any` | 至少命中一个文本 |
| `forbidden` | 不得出现的文本 |
| `tool` | 必须调用的工具名 |
| `reference_doc_ids` | 应召回的文档 ID；汇总为 micro Recall |
| `max_latency_ms` | 单条用例总延迟上限；多轮时为所有轮次总和 |

示例运行：

```bash
# 先启动完整 AgentGo 服务
make benchmark-e2e

# 容量测试：20 并发，每条用例重复 10 次，预热 20 次
go run ./cmd/benchmark \
  -base-url http://localhost:8080 \
  -dataset benchmarks/datasets/golden.v1.jsonl \
  -concurrency 20 -repeat 10 -warmup 20 \
  -output benchmark-report.json
```

Runner 输出 HTTP/业务成功率、任务通过率、工具准确率、检索 Recall、P50/P95/P99、吞吐量、分类型通过率和逐用例失败原因。默认示例数据只用于验证评测链路；正式门禁必须使用团队审核、版本化的 golden dataset。

## 3. Golden Dataset

建议 v1 使用 240 条人工审核用例，并避免训练/调试数据泄漏：

| 类别 | 数量 | 覆盖点 |
|---|---:|---|
| 普通对话 | 30 | 中英文、长短输入、拒答边界 |
| 单工具 | 50 | calculator/search/database，参数与返回值 |
| 多步任务 | 40 | 规划、工具链、达到最大迭代、部分失败 |
| RAG | 60 | 单跳、多跳、无答案、相似干扰文档、引用 |
| 多轮记忆 | 30 | 指代、事实保持、会话隔离、超长上下文 |
| 鲁棒/安全 | 30 | Prompt injection、恶意 SQL、超时、无效工具参数 |

数据维护规则：

- 为每条用例设置稳定 ID、类别、难度、来源和期望；不得写入真实密钥或个人数据。
- RAG 集合必须随数据集发布固定语料快照，并明确 query 对应的 relevant document IDs。
- 拆分 `dev/test`；Prompt 调试只看 dev，发布门禁只看冻结的 test。
- 对开放式答案，确定性断言只做第一道门禁，再由双人盲评或固定 judge 模型评价正确性、完整性、忠实性，各 1–5 分。Judge 的模型、Prompt 和温度也必须版本化。
- 失败用例进入数据集前先归因，避免大量同义样本让单一缺陷主导总分。

## 4. 核心指标

### Agent / Tool

- **Task Pass Rate** = 全部断言通过的任务数 / 总任务数。
- **Tool Accuracy** = 调用了正确工具的工具任务数 / 工具任务数。
- **Intent Macro-F1 / Route Accuracy**：意图识别器当前未接入默认 Agent 执行链；使用 `cmd/benchmark-intent` 单独测识别器 Macro-F1，端到端任务通过率不得称为意图准确率。默认 Agent 的路径选择仍需单独证据。
- **Function Calling**：记录参数准确率、并行调用正确率、强制工具遵循率、平均工具调用次数和 iteration-limit rate；HTTP 响应中的 `steps` 可用于回放执行链路。

### RAG

- Retrieval：分别记录各模式的 Recall@K、Precision@K、MRR@K、nDCG@K，首轮不设门禁。
- Generation：答案正确性、引用准确率、引用完整率、无答案拒答准确率。
- 忠实性必须人工或固定 judge 评价；关键词命中不能替代事实一致性评估。

### 性能与可靠性

- 非流式：端到端 P50/P95/P99、RPS、错误率。
- 流式：TTFT、token 间隔、完整响应时间。普通对话、RAG 生成和 Function Calling 最终答案均直接转发模型流式增量。
- 资源：CPU、RSS、goroutine、GC pause、Redis/Milvus/LLM 连接池。
- 故障注入：模型 429/500/超时、Redis/Milvus 不可用、工具超时。记录降级成功率、熔断开启时间和恢复时间。
- 资源用量：记录供应商提供的输入/输出 token 及 usage 覆盖率；货币费用与 cost per passed task 暂缓。

## 5. 基线阶段

本轮分别记录固定桩和真实模型的性能、效果、资源与故障结果。统一阈值、货币成本和发布门禁待稳定基线及正式数据集确定后再设计。

## 6. 当前实现边界

示例语料只检验评测链路。正式数据集审核、24 小时 Soak、货币成本和发布门禁暂缓。意图识别器、检索器与默认 Agent 路径分别报告；固定桩容量与真实模型效果分别报告。

## 7. 本地隔离实验

当前实现增加了独立 Compose project，固定桩和真实模型使用不同 project 与端口。Compose project 名也隔离了 PostgreSQL、Redis、etcd、MinIO、Milvus、Prometheus、Tempo 和 Grafana 的数据卷。以下命令在仓库根目录运行：

```bash
# 固定桩：AgentGo 18080、Prometheus 19090、桩管理端口 18088、TCP 故障代理 18099
sh benchmarks/compose.sh up -d --build
BENCH_URL=http://localhost:18080 sh benchmarks/seed.sh
go run ./cmd/benchmark -base-url http://localhost:18080 \
  -dataset benchmarks/datasets/full.example.jsonl -backend stub -scenario full-example \
  -corpus benchmarks/datasets/corpus.example.jsonl \
  -model benchmark-primary -config-version benchmarks/config.stub.yaml \
  -output benchmarks/reports/stub-full.json
go run ./cmd/benchmark-sse -base-url http://localhost:18080 -backend stub \
  -count 20 -concurrency 5 -output benchmarks/reports/stub-sse.json
go run ./cmd/benchmark-sse -base-url http://localhost:18080 -backend stub \
  -count 10 -concurrency 5 -slow-read 200ms
bash benchmarks/slow-client.sh # 固定 300 个流式增量，比较正常与慢客户端
```

真实模型运行 `sh benchmarks/compose-real.sh up -d --build`，服务端口为 `18081`。该环境使用 `benchmarks/config.real-gpt.yaml` 和 `.env` 中的 GPT-5.5 凭据，工具决策也由 GPT-5.5 完成；Benchmark Compose 将后台记忆提取超时设为 60 秒。先确认模型与 Embedding 可用，再以 `-backend real`、`-base-url http://localhost:18081` 跑少量效果用例。不要混合固定桩与真实模型的容量结果。两个 Compose project 可分别用对应脚本加 `down` 停止；要清空测试数据卷时显式加 `-v`。

### 指标口径

- `cmd/benchmark` 把 HTTP 2xx、业务码 0、全部断言通过分别记为 `http_success_rate`、`success_rate`、`task_pass_rate`。预期的故障 HTTP 状态可以通过 `expected_status` 断言通过，但仍计入 HTTP/业务失败。
- Runner 支持 `tool`、`tools_exact`（只比较业务工具，忽略内部 `list_tools` 调用）、`tool_args`（JSON 对象子集）、`max_tool_calls`、`min/max_planner_steps`、`planner_step_keywords`、`max_iterations`、`reference_titles` 或 `reference_doc_ids`、`reference_order`、`isolation_probe` 和 `isolation_forbidden`。`new_session_at` 与 `turn_delay_ms` 可构造跨会话长期记忆用例。用户隔离用例必须在 OIDC 环境提供属于另一个用户的 `AGENTGO_ISOLATION_ACCESS_TOKEN`；同一用户的新会话允许召回长期记忆。逐用例报告包含 Trace ID、状态码、工具调用、引用 ID、usage 与失败原因。
- HTTP Runner 的引用排名指标针对**答案返回的引用**；`cmd/benchmark-retrieval` 直接调用检索器，分别报告 vector、keyword、hybrid 模式的 Recall@K、Precision@K、MRR@K、nDCG@K。二者不能互换。固定语料为 `benchmarks/datasets/corpus.example.jsonl`，标题经 `etl.DocumentID` 得到稳定 ID。
- `cmd/benchmark-sse` 逐事件读取 `answer_delta`、`error`、`done`。TTFT 是请求开始到首个非空 `answer_delta`；增量输出速率按字符数计算。只有 `done.usage` 含供应商上报的 completion tokens 时才给出 Agent Loop 的 completion tokens / 请求总耗时；上下文压缩、Planner 和后台记忆调用不计入这份 usage。缺失值在 `unavailable` 中标记。
- JSON 报告记录提交 SHA、数据集 SHA、backend、场景、模型、非敏感参数/配置版本和机器信息。真实凭据不要传入元数据参数。当前不计算货币成本、不设发布门禁。

### 检索、意图与负载

检索命令需要从宿主机连接本地测试库；固定桩环境可在加载 `.env` 后覆盖 `APP_POSTGRES_PORT=15432`、`APP_MILVUS_ADDR=localhost:19531`、`APP_EMBEDDING_API_KEY=stub`、`APP_EMBEDDING_BASE_URL=http://localhost:18088/v1`、`APP_EMBEDDING_MODEL=benchmark-stub`，然后运行 `go run ./cmd/benchmark-retrieval -backend stub`。意图识别器**不在默认 Agent 执行路径**；固定桩环境使用 `BENCH_STUB_BASE_URL=http://localhost:18088 go run ./cmd/benchmark-intent -config benchmarks/config.stub.yaml -backend stub`，读取 `intent.example.jsonl` 并报告 Macro-F1。真实模型环境可直接运行 `go run ./cmd/benchmark-intent`。

```bash
bash benchmarks/run-k6.sh baseline
bash benchmarks/run-k6.sh load
bash benchmarks/run-k6.sh stream
bash benchmarks/run-k6.sh stress
sh benchmarks/resources.sh
bash benchmarks/resources-series.sh 300 # 压测期间每 5 秒记录一次资源快照
```

k6 的 SSE 请求只反映整条流的完成时间；TTFT 和断连行为使用 Go SSE 客户端。`resources.sh` 从 Prometheus 取 CPU、RSS、goroutine、GC、Redis/Milvus/Embedding 延迟，并从 Docker 取网络 I/O 快照。压力拐点应结合错误率、P95/P99 和资源曲线判断，不按单次最大 QPS 定义稳定容量。

### 故障实验

`bash benchmarks/fault.sh SCENARIO` 为每个场景输出注入前、注入中、恢复后三份报告；脚本退出时会重置代理。支持 `llm_429`、`llm_500`、`llm_timeout`、`invalid_tool`、`embedding_timeout`、`search_500`、`tool_timeout`、`redis_drop`、`milvus_drop`、`sse_disconnect`、`context_limit`。桩的 `/admin/stats` 请求计数写入汇总，可用于观察实际重试次数。在 Grafana/Tempo 对照 Trace ID 与 `agentgo_llm_circuit_open_total`、`agentgo_llm_fallbacks_total`、`agentgo_dependency_errors_total`、`agentgo_sse_outcomes_total`，记录真实恢复结果。HTTP 500 或任务失败是实验观察结果，不等于自动判定系统具有重试或降级能力。

示例数据仅验证链路，不能当正式效果基线。24 小时 Soak、经审核的 Golden Dataset、货币成本与发布门禁在本轮范围外。
