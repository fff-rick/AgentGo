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

在同一台空闲机器上运行；预热后至少采样 10 次。任一热点 `ns/op` 回退超过 10% 或 `allocs/op` 回退超过 15% 时需要解释或阻断合并。

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
- **Intent Macro-F1 / Route Accuracy**：分别衡量四类意图的均衡分类效果，以及实际进入的处理路径是否正确。当前公开响应不暴露 intent/route，只能先以端到端任务通过率间接覆盖，不能据此宣称意图准确率。
- **Function Calling**：记录参数准确率、并行调用正确率、强制工具遵循率、平均工具调用次数和 iteration-limit rate；HTTP 响应中的 `steps` 可用于回放执行链路。

### RAG

- Retrieval：Recall@5 为主门禁，同时记录 MRR@10、nDCG@10。
- Generation：答案正确性、引用准确率、引用完整率、无答案拒答准确率。
- 忠实性必须人工或固定 judge 评价；关键词命中不能替代事实一致性评估。

### 性能与可靠性

- 非流式：端到端 P50/P95/P99、RPS、错误率。
- 流式：TTFT、token 间隔、完整响应时间。普通对话、RAG 生成和 Function Calling 最终答案均直接转发模型流式增量。
- 资源：CPU、RSS、goroutine、GC pause、Redis/Milvus/LLM 连接池。
- 故障注入：模型 429/500/超时、Redis/Milvus 不可用、工具超时。记录降级成功率、熔断开启时间和恢复时间。
- 成本：输入/输出 token、模型费用、工具费用，以及 **cost per passed task**。

## 5. 建议发布门禁

先连续记录 5 次稳定基线，再启用门禁。初始建议：

| 指标 | 门禁 |
|---|---:|
| HTTP/业务成功率 | >= 99.5% |
| 总 Task Pass Rate | >= 90%，且不低于基线 2 个百分点 |
| Tool Accuracy | >= 95% |
| RAG Recall@5 | >= 90% |
| RAG 引用准确率 | >= 95% |
| P95 延迟 | 不高于 SLO，且不比基线回退 10% |
| cost per passed task | 不比基线回退 10% |
| 安全关键用例 | 100% 通过 |

不要把本表直接当作当前项目已经达到的数值；它是建立首轮基线后的候选门槛。

## 6. 当前实现边界与推进顺序

Milvus、Ollama embedding、PostgreSQL 中文分词/BM25、真实搜索、只读数据库工具和 SSE 流式输出均已接入；当前主要缺口是正式 Golden Dataset、成本指标和故障注入基线。因此建议按以下顺序推进：

1. 立即在 CI 运行离线微基准和 smoke runner。
2. 冻结 RAG 语料快照并为 BM25 与混合检索启用 Recall/MRR/nDCG 门禁。
3. 补齐 token usage、模型名、Agent iterations、intent 和降级原因的响应/trace 字段，使成本和路由评测可观测。
4. 为现有流式调用补充 TTFT 门禁。
5. 在预发进行 1/5/20/50 并发阶梯压测及故障注入，不在共享生产环境直接压测。
