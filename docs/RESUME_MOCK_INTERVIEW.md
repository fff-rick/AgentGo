# AgentGo 简历项目深挖面试题与回答模板

> 生成依据：`README.md` 的项目描述与当前仓库源码。回答模板按“我负责/我实现”的面试口径编写，但使用前应删掉不属于你本人贡献的内容。量化数据仅引用 `docs/BENCHMARK_RESULTS.md`，不要扩大解释。

## 项目事实底稿

### 一分钟项目介绍

AgentGo 是我用 Go 1.22 实现的 AI Agent 服务端平台。它没有依赖现成 Agent 编排框架，而是围绕原生 Function Calling 自研了 AgentHarness 和 AgentLoop，把上下文构建、模型决策、工具执行、结果回填、会话持久化和异步长期记忆串成一次完整 Run。系统还实现了 Milvus 向量检索与 PostgreSQL BM25 的混合 RAG、Redis 会话记忆、PostgreSQL 与 Milvus 组合的长期记忆、多模型路由与三态熔断、文档 ETL、SSE/TUI 可观察执行以及 OIDC 权限隔离。我的设计重点不是“能调用一次模型”，而是让 Agent 的执行边界、失败降级、数据一致性和评测方式都可解释、可验证。

### 真实调用链

```text
cmd/server/main.go
  → router.Register
  → handler.ChatHandler.Chat / ChatStream
  → agent.Orchestrator.ProcessMessage
  → harness.AgentHarness.Run
  → agentcontext.ContextBuilder.Build
  → agentloop.Loop.Run（默认）或 agent.PlannerAgent.Execute（显式 planner）
  → llm.Router.ChatStream / tool.Router.BatchExecuteScoped
  → 保存 Redis 会话消息
  → Precompactor（会话摘要）与 memory.JobRunner（长期记忆）
```

### 功能完成度与面试口径

| 能力 | 状态 | 核心证据 | 面试时应说明的边界 |
|---|---|---|---|
| 统一 Agent Loop 与原生 Function Calling | ✅ 已实现 | `internal/agentloop/loop.go` | 决策由模型完成，循环和上限由服务端控制 |
| Planner-Executor | ✅ 已实现 | `internal/agent/planner.go` | 由调用方显式选择，不是意图识别自动路由 |
| 工具注册、并行执行、惰性加载 | ✅ 已实现 | `internal/tool/registry.go`、`manager.go`、`router.go` | 热工具集存在 Redis，最多加载数由阈值控制 |
| 混合 RAG | ✅ 已实现 | `internal/rag/*`、`internal/database/postgres.go` | 向量与关键词并发召回，单路失败可降级；Rerank 失败保留原排序 |
| 文档 ETL | ✅ 已实现 | `internal/etl/*` | 支持 Markdown/PDF/DOCX/XLSX；PDF 依赖 Docling |
| 三层记忆 | ✅ 已实现 | `internal/memory/*`、`internal/agentcontext/*` | Working Memory 在进程内；长期记忆最终一致，不是强一致双写 |
| 多模型路由与熔断 | ⚠️ 部分符合 README | `internal/llm/router.go`、`circuit_breaker.go` | 当前是静态 priority + 熔断回退，不是按任务复杂度动态选模 |
| 上下文压缩 | ✅ 已实现 | `internal/agentcontext/builder.go`、`compactor.go` | token 数为启发式估算；预压缩队列进程崩溃会丢任务 |
| 文件修改审批 | ✅ 已实现 | `internal/tool/builtin/filesystem.go` | preview/apply 两阶段、哈希校验；默认路径权限等于进程 OS 权限 |
| OIDC 与用户隔离 | ✅ 已实现 | `internal/auth/oidc.go`、Handler 所有权校验 | 默认关闭时所有请求共享 `local-user`，只能用于可信本地环境 |
| 可观测性与 Benchmark | ✅ 已实现 | `internal/metrics/*`、`internal/trace/*`、`cmd/benchmark*` | 固定桩容量不能外推真实模型容量，真实样本量也较小 |
| Reflection Hook | ✅ 可选实现 | `internal/agent/reflection.go` | 默认关闭；会额外增加一次模型成本与延迟 |

## 一、项目定位与架构（1—10）

### 1. 请你整体介绍一下 AgentGo。

**回答模板：** AgentGo 是一个 Go 实现的企业级智能体服务端。我把一次请求拆成 API、Harness、Context、AgentLoop、Tool、Memory 和基础设施几层：Handler 只做协议和身份校验；Harness 管生命周期；ContextBuilder 拼装历史、摘要和长期记忆；AgentLoop 让模型决定直接回答还是调用工具；ToolRouter 执行工具；最后持久化消息并异步提取长期记忆。这个分层让我能分别测试编排、推理和外部依赖，而不是把所有逻辑塞进 Handler。

### 2. 为什么要自研 Agent 框架，而不是直接使用 LangChain 一类框架？

**回答模板：** 我的目标是掌握并控制 Agent 的关键边界：每轮模型输入、工具 Schema 暴露、循环上限、并发工具调用、错误包装、usage 统计和取消传播。现成框架能更快起步，但会带来抽象层和调试成本。这个项目的核心并不是追求组件数量，而是把 Function Calling 的真实协议链路做透。代价是我要自己维护生态适配；如果业务更偏快速试验，我会重新评估采用成熟框架。

### 3. 一次普通聊天请求的完整调用链是什么？

**回答模板：** `ChatHandler` 先校验 JSON、执行模式、工具白名单、Skill 和会话归属，再调用 `Orchestrator.ProcessMessage`。Orchestrator 转成 `RunRequest` 交给 `AgentHarness.Run`。Harness 构建 ToolScope 和上下文，然后默认进入 `agentloop.Loop.Run`。Loop 通过 `llm.Router.ChatStream` 获取模型增量，若有 tool call 就交给 ToolRouter，结果以 tool message 回填继续决策；得到最终答案后，Harness 保存用户与助手消息，再提交预压缩和长期记忆任务。

### 4. 为什么单独设计 AgentHarness？

**回答模板：** AgentLoop 只负责“模型—工具”循环，不应该知道会话、Skill、长期记忆和 Hook。Harness 则负责一次 Run 的横切生命周期：超时、上下文构建、模式选择、运行状态、Reflection Hook、消息落库和后台任务提交。这样 Loop 可以用纯输入输出做单测，Planner 也能复用相同的会话和工具边界。后续增加审计或限额时，也更适合作为 Harness Hook，而不是侵入推理循环。

### 5. 系统为什么同时保留默认 Agent 模式和 Planner 模式？

**回答模板：** 默认 AgentLoop 适合开放式对话，模型可以每轮根据新工具结果动态改变下一步；Planner 适合步骤清晰、依赖明确的复杂任务，先产出 JSON 计划再顺序执行。两者取舍是动态性与可预测性。当前 Planner 由请求中的 `options.mode=planner` 显式开启，意图识别不会自动切换，这是我有意保留的控制权，也避免分类误判突然改变执行语义。

### 6. Working Memory 在代码里具体是什么？

**回答模板：** 它是 `agentloop.RunState`，只在单次 Run 内存在，保存 run_id、用户、会话、任务、步骤和工具结果。它不落 Redis，也不跨请求，作用是让执行过程与最终结果有统一载体。会话级短期记忆由 Redis 保存，跨会话长期记忆由 PostgreSQL 元数据加 Milvus 向量实现，所以“三层记忆”不是三个都持久化，而是按生命周期分层。

### 7. 为什么选择 Go？

**回答模板：** Agent 服务大量时间花在模型、Redis、Milvus、PostgreSQL 和搜索等 I/O 上，Go 的 goroutine、context 和标准 HTTP 能较低成本地处理并发与取消；静态类型也适合约束工具协议和事件结构。缺点是 AI 生态不如 Python 丰富，文档解析需要借助 Docling 服务。但这个项目更强调在线服务、并发和工程边界，Go 的收益大于生态损失。

### 8. 项目里 interface 用在什么地方，为什么不是到处抽象？

**回答模板：** 我只在真正存在替换边界的位置定义 interface，例如 LLM Client、Tool、VectorDB、SessionManager、Context Builder 和 DocumentIndexer。这些边界要么有多实现可能，要么需要测试桩。具体编排结构仍使用具体类型，避免为未来假想需求堆抽象。比如 `AgentHarness` 依赖 AgentLoop 接口，单测可替换；Milvus 客户端内部细节则不额外再套多层 Repository。

### 9. 启动过程如何保证依赖正确？

**回答模板：** `cmd/server/main.go` 按配置、日志、鉴权、Redis、Milvus、Embedding、PostgreSQL、Tracing、模型、记忆、工具、Agent、Handler 的顺序组装。关键依赖初始化失败直接终止，Embedding 与 Milvus 维度不一致也 fail fast。Tracing 初始化失败可以降级，因为它不是业务正确性的硬依赖。这样把“必需依赖”和“可选能力”区分开，避免服务看似启动、首个请求才暴露配置错误。

### 10. 优雅关停是怎么实现的？

**回答模板：** 主进程监听 SIGINT/SIGTERM，创建 15 秒超时 Context，先调用 `http.Server.Shutdown` 停止接收新请求并等待在途请求，再关闭 Precompactor 和长期记忆 JobRunner。Redis、Milvus、PostgreSQL 和 Trace Provider 通过 defer 释放。异步文档 Importer 关闭时会把未处理任务标成失败并清理临时目录。边界是超过关停窗口的任务会被取消，调用方需要重试。

## 二、Agent Loop 与 Function Calling（11—22）

### 11. AgentLoop 的核心状态机是什么？

**回答模板：** 每轮把 system prompt、历史消息和当前工具定义发给模型。模型若无 tool call，就清洗并返回最终答案；若返回 tool calls，就记录 assistant 消息，并行执行工具，把每个结果按对应 tool_call_id 写成 tool message，再进入下一轮。循环同时限制业务工具轮次和工具发现次数。达到上限后移除工具能力，用一条 system 消息要求模型只根据已有信息总结，保证请求可以收敛。

### 12. 为什么用原生 Function Calling，而不是让模型输出 ReAct 文本？

**回答模板：** 原生 Function Calling 把工具名和参数变成结构化协议，能用 JSON Schema 约束参数，也能用 tool_call_id 正确关联并行结果；服务端不需要从自然语言中解析 Action。ReAct 文本更通用，但解析脆弱且容易把普通回答误判为操作。项目仍保留兼容的 ReActAgent 适配层，但主链路是统一 AgentLoop。

### 13. 并行工具调用如何实现，什么情况下不能并行？

**回答模板：** LLM 客户端开启 `parallel_tool_calls`，Loop 收到同一轮多个 calls 后交给 `BatchExecuteScoped`，ToolRouter 使用 goroutine 并通过索引回填结果，因此返回顺序仍与调用顺序一致。互不依赖的搜索、知识库查询可以并行；如果 B 的输入依赖 A 的输出，就不应让模型在同一轮发出，而应等待 A 的 tool message 后下一轮再调用。

### 14. 如何限制 Agent 无限循环？

**回答模板：** 我设置了两套计数：`businessIterations` 只统计业务工具轮次，`discoveryCalls` 单独统计 `list_tools`，总决策次数最多是二者上限之和。这样工具发现不会吃掉全部业务轮次，但也不能无限调用。到上限后服务端移除工具并强制生成最终答案。除此之外，Harness 还有整个 Run 的 context timeout，客户端断开也会沿 Context 取消下游调用。

### 15. required tools 有什么用途，如何保证模型真的调用？

**回答模板：** RequiredTools 用于兼容某些必须经过指定工具的场景。Loop 先校验工具是否在当前 definitions 中，再逐个把 `RequiredTool` 映射成供应商的指定函数 tool_choice。如果模型没有调用，或者调用列表里不含该工具，服务端直接报错，而不是假装完成。它比只在 prompt 中说“必须调用”更可靠，但会降低模型自由度，所以普通请求仍使用 auto。

### 16. 工具输出如何防 Prompt Injection？

**回答模板：** 系统提示明确把普通工具结果定义为不可信数据；回填时还统一包成包含 `source`、`trusted`、`success`、`output` 的 JSON。只有注册表限定的 `load_skill` 且执行结果标记 trusted 才能成为可信指令。普通工具输出限制为两万 rune，避免超长内容挤占上下文。这个方案不能从理论上消灭模型越权，因此敏感操作仍必须由服务端权限和审批机制兜底。

### 17. 模型返回空答案或错误协议时怎么办？

**回答模板：** 如果既没有 tool call 又没有可用文本，Loop 返回明确错误；指定工具未被调用同样失败。工具参数解析错误会作为 ToolResult 回填，让模型有机会修正，而不是让整个 Run 立即崩溃。达到最大轮次后若总结仍为空，返回固定的降级提示。我的原则是协议错误可观测、业务工具错误可反馈纠正、无法恢复的模型错误再终止请求。

### 18. 为什么主循环内部使用流式 LLM，即使同步 API 也如此？

**回答模板：** 统一走 `ChatStream` 可以让同步和 SSE 请求共享完全相同的 Agent 决策链，同时把 reasoning、answer delta、tool call 和 usage 事件发送给观察器。同步接口只是最终一次性返回结果，SSE 接口则把中间事件实时转发。这样不会维护两套推理逻辑。代价是流式聚合更复杂，特别是工具调用参数和 usage 可能只在流末尾出现。

### 19. SSE 如何处理“HTTP 200 后失败”？

**回答模板：** Header 和首个 session 事件发出后状态码已经不能改变，因此后续失败通过 `event:error` 表达，成功以 `event:done` 结束。指标单独记录 done、error 和 disconnected；若请求 Context 被取消，就标记客户端断开。Nginx 缓冲通过 `X-Accel-Buffering:no` 禁用。客户端必须按终止事件判断业务成功，不能只看 HTTP 200。

### 20. Agent 的步骤与 ToolCalls 为什么都要保存？

**回答模板：** Steps 表达执行时间线，包括 action 和 answer；ToolCalls 更适合 API 消费，保存工具名、输入、输出和耗时。两者服务不同：TUI 用 Steps/事件展示过程，评测与审计更容易聚合 ToolCalls。当前不会把完整思维链落库，只展示模型显式提供的 reasoning 字段，标准模型没有时也不会伪造。

### 21. Reflection 是怎么接入的，为什么默认关闭？

**回答模板：** Reflection 实现 Harness 的 `AfterLoop` Hook，在主循环完成后拿用户问题和初稿，再调用一次模型，从准确性、完整性、清晰度和实用性重写答案。如果返回内容不同就替换最终答案。它默认关闭，因为每次请求多一次模型调用，会增加成本、尾延迟，并且二次生成也不必然更正确。更成熟的方案应按低置信度或高风险请求选择性触发。

### 22. 如果要支持工具结果重试，你会放在哪里？

**回答模板：** 我会区分传输层瞬时错误和业务错误。幂等、只读工具可以在 ToolRouter 外围做有上限的指数退避，并把 attempt 写入指标；有副作用的工具不能自动重试，必须依靠幂等键或显式补偿。模型自行看到错误后再次调用属于语义重试，也受 Agent 轮次限制。这样避免 Router 无脑重试造成重复写入。

## 三、工具系统、Skill 与文件安全（23—34）

### 23. 工具系统如何做到插件化？

**回答模板：** 每个工具实现 `Name、Description、Parameters、Execute` 接口，Registry 负责唯一注册和查询，Router 负责 scope 校验、执行、耗时统计与批量调度。模型看到的是由接口生成的 Function Definition，不依赖具体实现。新增工具只需实现接口并注册，不需要修改 AgentLoop；权限、惰性加载和使用统计则由 Router/Manager 统一处理。

### 24. 为什么要做工具惰性加载？

**回答模板：** 工具数量增加后，把所有 Schema 每轮发给模型会消耗 token，也会提升选错工具的概率。初始只暴露 `list_tools`、可选 `load_skill` 和会话热工具；模型先看目录，再加载最多阈值个业务工具。加载结果会替换当前业务 Schema。当前默认热集为 3，工具发现最多 4 次，是 token 成本和可达性的折中。

### 25. 会话热工具集如何维护？

**回答模板：** Manager 在 Redis 持久化每个会话的工具使用次数和最近访问时间，同时维护全局使用次数。每次使用后更新统计，再按会话频次、全局频次和最近访问进行裁剪，保留 threshold 个。下一轮初始 definitions 直接加载热集。Redis 失败时功能会退化而不是影响 Registry 的真实工具集合，但热集准确性会下降。

### 26. `options.tools` 的 nil、空数组和非空数组有什么区别？

**回答模板：** nil 表示调用方没有限制，Agent 可以使用所有已注册业务工具；显式空数组表示禁用全部业务工具；非空数组是 allowlist，只允许指定名称。Handler 会在运行前校验未知工具并返回 400。这个三态语义避免“字段没传”和“明确禁止”混为一谈，ToolScope 在执行时还会再次检查，不能只依赖 prompt。

### 27. Skill 与普通工具有什么本质差异？

**回答模板：** 普通工具提供外部数据，输出永远不可信；Skill 是部署者维护的操作指令，只有通过注册表中的 `load_skill` 按名字加载后才标记 trusted。启动时递归扫描 `SKILL.md` 并解析 frontmatter，模型初始只看到名称和描述。加载工具不能读取任意路径，也不执行 Skill 附带脚本，所以它是受限的指令注入机制，而不是通用插件运行时。

### 28. Planner 为什么不自动选择 Skill？

**回答模板：** 默认 AgentLoop 能在运行中通过 `load_skill` 动态加载，而 Planner 是先固定工具和计划再执行，动态 Skill 会改变规划语义。当前设计要求 Planner 只使用请求显式指定的 Skill，让调用方承担选择责任。这牺牲了一点自动化，但计划更可复现，也避免规划过程中多一层发现循环。

### 29. 文件修改为什么拆成 preview 和 apply？

**回答模板：** `file_edit_preview` 只生成 diff 和 15 分钟有效的 proposal_id，并记录原文件哈希，不落盘；用户通过下一次请求的 `approved_proposals` 明确批准后，`file_edit_apply` 再验证 proposal、路径和当前哈希，使用临时文件加 rename 原子替换。两阶段把模型建议和真实副作用分开，哈希校验还能防止审批期间文件被别人修改造成覆盖。

### 30. 文件工具目前有哪些安全边界？

**回答模板：** 它只接受绝对路径、UTF-8 普通文件，并限制 1 MiB；修改必须经过审批，proposal 有 TTL，apply 还做内容哈希校验。但默认不限制工作区根目录，最终权限等于 AgentGo 进程的 OS 权限，所以绝不能在关闭 OIDC 时暴露到不可信网络。生产化时我会增加 workspace allowlist、符号链接解析、防目录穿越和独立低权限用户。

### 31. database_query 真的只读吗？

**回答模板：** 服务端先去掉末尾分号，拒绝多语句，并要求首个 token 是 SELECT；随后开启 PostgreSQL 只读事务，设置 statement timeout，并限制最大返回行数。因此即使文本校验有绕过空间，数据库事务仍是第二道防线。更严格时我会使用独立只读数据库账号和 SQL AST 解析，因为只靠字符串首词判断不是完整的 SQL 安全模型。

### 32. web_search 为什么通过自建 SearXNG？

**回答模板：** SearXNG 把多个搜索源封装成统一内部接口，AgentGo 只处理结构化的标题、URL、摘要、引擎和时间，不需要绑定单一商业 SDK。它也便于本地 Compose 复现和网络出口治理。代价是搜索质量、限流和上游变化需要自己维护，工具结果仍必须视为不可信并由模型交叉判断。

### 33. 工具输出为什么限制两万 rune，而不是 byte？

**回答模板：** rune 限制对中文等多字节文本更符合“字符量”的直觉，也不会在 UTF-8 字节中间截断。限制发生在回填模型的 tool message，避免一次网页或文件输出挤爆上下文；ToolCallInfo 仍可能用于观察，因此生产环境还要在事件和日志层分别设上限并做敏感信息脱敏。

### 34. 工具并行执行会有哪些并发安全问题？

**回答模板：** 只读工具通常可以并发，但文件 apply、共享缓存更新或外部有副作用 API 可能互相冲突。当前文件工具靠 proposal 哈希检测写冲突，Manager 用 mutex 保护同进程内热集更新。跨实例时进程锁不够，需要 Redis Lua/CAS、数据库唯一约束或分布式锁；更根本的是让副作用操作具备幂等键，而不是只依赖锁。

## 四、RAG 与检索原理（35—47）

### 35. AgentGo 的 RAG 完整链路是什么？

**回答模板：** 文档先经过 Parser 保留结构信息，再由 Chunker 分块，批量生成 embedding，写入 Milvus，同时用纯 Go 中文分词写 PostgreSQL 倒排词频。查询时 `knowledge_search` 调用 Pipeline，Retriever 并发执行向量检索和 BM25 关键词检索，以 RRF 融合候选，再可选调用 LLM Rerank，最后按阈值过滤并把引用随 ToolResult 返回给 Agent。

### 36. 为什么不只用向量检索？

**回答模板：** 向量检索擅长语义相似，但订单号、专有名词、错误码这类精确 token 可能召回不稳定；BM25 对精确词匹配更敏感，却难处理同义表达。两路组合能互补。比如用户问“APP_RAG_SCORE_THRESHOLD”，关键词检索更可靠；问“检索最低相关度怎么控制”，向量检索更有优势。项目让任一路失败时降级到另一路，提高可用性。

### 37. BM25 的底层原理是什么？

**回答模板：** BM25 用词频、逆文档频率和文档长度归一化计算相关度。项目中 k1=1.2、b=0.75：词在分块里出现越多分数越高，但会饱和；越稀有的词 IDF 越高；长分块会受到长度惩罚。中文先通过 gse 分词，把每个 chunk 的 term_frequency 和 token_count 存入 PostgreSQL，查询时 SQL 动态计算语料统计和分数。

### 38. RRF 为什么适合融合两路结果？

**回答模板：** 向量相似度和 BM25 分数不在同一量纲，直接加权需要校准。RRF 只看各自排名，公式是每一路累加 `1/(60+rank)`，同一 chunk 在两路都靠前就获得更高融合分。它实现简单、对原始分数尺度不敏感。缺点是丢失了分数间距信息，后续可用带标注数据学习融合权重。

### 39. 如何对同一分块去重？

**回答模板：** 融合主键优先使用 `ChunkID`，没有时退化为 `DocID + content`。每一路命中同一个 key 时只保留一份 Reference，同时累计 RRF 分数。文档导入时 chunk ID 由文档 ID 和 chunk_index 做 SHA-256 派生，因此同一逻辑文档重新导入后稳定，既便于 Upsert，也保证融合去重可靠。

### 40. Rerank 为什么使用 LLM，它失败怎么办？

**回答模板：** 初召回追求覆盖，LLM Rerank 能结合完整查询对候选片段做更细的语义判断。实现中每个候选截取前 200 rune，要求返回 index 和 0—10 分，再归一化、阈值过滤和排序。若调用失败或 JSON 解析失败，记录告警并返回原始融合排序，不让增强步骤拖垮主链路。缺点是成本和延迟明显，最好用专用 cross-encoder 替代。

### 41. score threshold 在两阶段分数中如何理解？

**回答模板：** 向量阶段的 threshold 作用于向量相似度；启用 Rerank 后又把 LLM 的 0—10 分归一化，并用同一配置过滤。两种分数语义其实不同，共用阈值只是工程简化，面试时我不会把它说成严格统一概率。更合理的做法是拆成 vector_threshold 和 rerank_threshold，并基于验证集分别调参。

### 42. 混合检索怎么实现并发和降级？

**回答模板：** Retriever 同时发起 embedding+Milvus 路径和 PostgreSQL 关键词路径，等待两路结束后融合。如果一边失败但另一边有结果，就记录错误并返回可用结果；两边都失败才整体失败。这样降低单依赖故障对问答的影响。需要注意 embedding 失败会直接让整个向量路径不可用，因此指标要分别标记 embedding、Milvus 和 PostgreSQL，而不是只看 RAG 总错误率。

### 43. 为什么 embedding 维度必须在启动时校验？

**回答模板：** Milvus collection 的向量维度是固定 schema，embedding 输出维度不一致会在写入或检索时失败。启动时直接比较配置并 fail fast，比运行到首次导入再报错更安全。另外，即使两个模型维度相同，向量空间也不同，不能混用；所以 README 明确要求换模型时换 collection 并重新导入旧文档。

### 44. Milvus 中选择 COSINE、L2、IP 有什么区别？

**回答模板：** COSINE 比较方向，适合很多归一化语义向量；L2 比欧氏距离，对向量模长敏感；IP 是内积，若向量已归一化与 cosine 排名接近。项目把 metric 配置化并在初始化时解析。选择必须与 embedding 模型训练和向量归一化方式一致，不能只凭“哪个分高”决定，还要用检索集测 Recall@K、MRR 和 nDCG。

### 45. 如何评估 RAG 效果？

**回答模板：** 我会把评估拆成检索和生成两层。检索层用带相关 chunk 标注的数据集计算 Recall@K、MRR、nDCG，并分别比较向量、BM25、RRF、Rerank；生成层检查答案正确性、引用覆盖和忠实度。仓库已有 retrieval benchmark 和示例数据集，但示例不是正式 Golden Dataset，所以当前结果只能做回归基线，不能宣称生产准确率。

### 46. RAG 遇到“召回正确但回答错误”怎么排查？

**回答模板：** 先从 SSE/Trace 确认 knowledge_search 的 query、候选引用和最终进入模型的 tool message；若候选正确，再检查片段是否被截断、模型是否忽略引用、系统提示是否冲突。若候选错误，则分别复跑向量与 BM25 看是哪一路、分块还是 Rerank 出问题。把“检索失败”和“生成不忠实”分开定位，避免盲目调 embedding。

### 47. RAG 还可以怎样优化？

**回答模板：** 短期可以把两个 threshold 拆开、加入查询改写、针对标题与正文加权、缓存 embedding，并用专用 reranker 降成本。中期建立真实 Golden Dataset，按文档类型分析召回。规模扩大后，可物化 BM25 语料统计、增加租户过滤和分区，并对热门 query 做结果缓存。所有优化都应以 Recall、延迟和成本共同验证。

## 五、文档 ETL 与数据一致性（48—59）

### 48. 文档导入支持哪些格式，分别怎么解析？

**回答模板：** Markdown/纯文本走本地解析，PDF 调 Docling 做版面和 OCR，DOCX 读取段落与表格，XLSX 保留工作表、行和单元格范围。Parser 输出统一的 `ParsedDocument` 和带 metadata 的 elements，后续 Chunker 才能保留页码、section_path、sheet 和 cell range。统一中间表示避免每种格式单独实现向量化链路。

### 49. 为什么要保留文档结构 metadata？

**回答模板：** 只保存纯文本会丢失页码、章节和表格坐标，答案虽能命中内容，却无法给出可核查引用。Chunker 在合并文本时只合并兼容页码和章节的元素；表格会重复表头，Excel 行保留单元格范围。metadata 随向量和 PostgreSQL chunk 保存，最终 Reference 可以告诉用户内容来自哪一页或哪组单元格。

### 50. 文档 ID 和内容哈希为什么分开？

**回答模板：** 文档 ID 由规范化 content_type 和 trim 后标题生成，代表逻辑文档身份；ContentHash 由原始字节生成，代表具体版本。同标题同类型再次上传会定位同一文档，hash 未变就跳过；hash 改变则覆盖索引。若直接用内容生成 ID，更新内容会变成新文档，旧向量难以清理。

### 51. Chunk ID 如何保证稳定？

**回答模板：** Chunk ID 用 `docID + chunk_index` 做 SHA-256。相同逻辑文档按相同分块顺序重建时 ID 稳定，可以在 Milvus 中 Upsert；如果新版本 chunk 数减少，代码按旧 chunk_count 删除尾部 stale IDs。它的边界是前部插入会导致后续索引整体漂移，但重导会覆盖对应 ID，最终结果仍正确，只是写放大较大。

### 52. 分块策略是怎样的？

**回答模板：** 对解析后的 elements 优先按结构分块：同页同章节的普通文本在 chunkSize 内合并；超长文本按句子或固定大小拆；表格保留表头；Excel 超长行按 UTF-8 byte 安全拆分；图片若有 caption 单独形成块。只有解析器没有 elements 时才退化为句子分块。这样比纯固定窗口更能保留语义和引用位置。

### 53. overlap 有什么作用，项目哪里真正使用了？

**回答模板：** overlap 用于固定大小切分，避免答案横跨分块边界时上下文完全断裂，步长是 chunkSize-overlap。结构化 element 合并更多依赖章节和句子边界，不是每种路径都机械重叠。重叠过大增加存储和重复召回，过小降低跨边界 Recall，需要通过检索数据集调参。

### 54. 文档异步导入怎么做背压？

**回答模板：** Importer 有容量为 8 的 jobs channel 和 slots 信号量。Submit 先抢 slot，抢不到立即返回 queue full；成功后把上传内容写临时文件，数据库标记 processing，再投递任务。单 worker 顺序消费，结合 Pipeline 全局锁保证单实例导入不会交叉覆盖。这个实现简单可靠，但吞吐有限，扩展时应改为持久化队列和按文档粒度锁。

### 55. 服务重启时 processing 文档怎么办？

**回答模板：** Importer 初始化时执行 `FailInterruptedDocuments`，把遗留 processing 状态更新为 failed，提示重新上传；关停时也尽量 drain 队列并标记未执行任务失败。临时文件在进程目录中，关闭时清理。当前没有断点续传，选择的是显式失败加幂等重传，而不是让状态永久卡住。

### 56. Milvus 与 PostgreSQL 双写如何保证一致性？

**回答模板：** 当前不是跨库事务。流程先向量 Upsert、清理空块和旧尾块，再在 PostgreSQL 事务中原子替换文档及关键词 chunk。中途失败会记录“可重复导入修复”，依靠稳定文档/分块 ID 和幂等 Upsert 收敛。它属于可修复的最终一致性；严格生产化可引入 outbox，把解析结果落主库后由异步任务驱动两个索引并记录版本。

### 57. 为什么 PostgreSQL 侧可以原子替换？

**回答模板：** `IndexDocument` 在一个事务中 upsert documents、删除旧 chunks、插入新 chunks 与 terms，任何一步失败都会 rollback。外键 `ON DELETE CASCADE` 清理分词，`(doc_id, chunk_index)` 唯一约束避免重复。这样至少保证关系库内部文档元信息、chunk 和倒排索引是同一个版本。

### 58. 全局导入锁有什么优缺点？

**回答模板：** 优点是实现极简，能避免同一进程中两个文档更新交叉影响依赖并降低外部服务压力；当前单 worker 下它也是一道防御。缺点是不同文档也被串行化，无法利用 embedding 批处理和并发。规模提升后应改为以 docID 为粒度的锁，并用队列控制总体并发；多实例还需要数据库 advisory lock 或分布式协调。

### 59. PDF 解析服务不可用时怎么处理？

**回答模板：** Parser 调 Docling 带独立超时，失败会让导入任务标记 failed，并保留错误信息供状态 API 查询，不会生成不完整索引。健康检查也包含文档解析依赖。若业务要求更高可用性，我会增加文本层 fallback，但必须标记解析质量，因为无版面/OCR 的降级结果可能让表格和页码失真。

## 六、三层记忆与上下文管理（60—72）

### 60. 三层记忆分别解决什么问题？

**回答模板：** Working Memory 是单次 Run 的步骤和工具结果；Session Memory 用 Redis 保存原始对话、序号和摘要，解决同一会话连续性；Semantic Memory 抽取高价值用户事实，以 PostgreSQL 管理版本和状态、Milvus做跨会话相似检索。分层依据是生命周期和一致性要求，而不是简单复制三份数据。

### 61. 会话消息如何保证顺序？

**回答模板：** 每次 AppendMessage 先对 Redis sequence key 原子 INCR，再把 sequence 写入消息并 LPUSH 到列表。读取后按 sequence 排序，因此底层逆序插入不会影响对话顺序。需要诚实说明：INCR 成功而 LPUSH 失败会留下序号空洞；压缩逻辑会检测消息序号连续性，发现 gap 就暂不压缩，避免摘要越过缺失消息。

### 62. 为什么摘要保存要用 CAS？

**回答模板：** 同一会话可能有并发请求同时读取旧摘要并生成新摘要。如果直接覆盖，后完成的旧视图会丢掉另一请求更新。`SaveSummary` 比较预期旧值并要求 through_sequence 前进，只有一个写入成功；失败方重新加载最新摘要和消息再构建上下文。这是乐观并发控制，比持有跨 LLM 调用的长锁更合适。

### 63. 上下文是怎么构建的？

**回答模板：** ContextBuilder 并发加载用户资料、完整会话消息、会话摘要和与当前 query 相关的长期记忆。然后把基础系统提示、运行时指令、用户、摘要和记忆拼成 system prompt，追加摘要水位线之后的消息与当前问题，再估算 token。超预算时先压缩旧消息、减少长期记忆、最后按完整轮次丢弃最老历史。

### 64. 为什么丢历史要按完整 turn，而不是一条消息？

**回答模板：** 如果只删 user 消息，可能留下无来源的 assistant 回答，模型会误解上下文；如果只删 assistant，也会留下看似未回答的问题。`dropOldestTurn` 从最旧 user 开始，一直删到下一个 user 前，保证不会让 assistant 孤立。工具消息在持久化会话中没有单独保存，因此当前主要维护 user/assistant 配对。

### 65. token 预算是精确的吗？

**回答模板：** 不是。项目用启发式估算：ASCII 大约四字符一个 token，非 ASCII 一个字符按一个 token，再加消息开销。这避免绑定某个 tokenizer，速度快，但对不同模型和混合文本会有误差。值得指出的是当前 Builder 中“基础 system+tools+当前消息先验超限”的检查被注释了，最终总预算仍会检查，但错误定位不够精确，是可改进点。

### 66. 会话压缩的真实流程是什么？

**回答模板：** 当估算超出 max tokens 且旧消息多于 recentMessages 时，从旧消息里切出可压缩区，校验 sequence 连续，把已有摘要、旧消息和保留消息交给 LLM Compactor。生成的新摘要带 through_sequence，通过 Redis CAS 保存。构建阶段只使用水位线后的消息，摘要失败则退化为按预算裁剪近期消息，不阻断整个聊天。

### 67. Precompactor 的作用是什么？

**回答模板：** 如果每次都等请求已经超预算才现场压缩，会给用户增加一次模型延迟。回复保存后，当历史条数足够且估算占预算约 70% 时，Precompactor 把会话提交到容量 16 的队列，由两个 worker 提前生成摘要；同一 session 用 pending map 去重。它是 best-effort 的进程内队列，队列满或崩溃可丢，但下次构建仍能同步压缩保证正确性。

### 68. 长期记忆如何避免把模型幻觉存进去？

**回答模板：** 提取 prompt 只允许从用户原话取本人事实或偏好，不读取助手回答；开启 strict evidence 后，候选必须包含逐字 evidence，且 evidence 必须是 question 的子串，confidence 至少 0.7、importance 达阈值。JSON 用 DisallowUnknownFields 严格解析，类型、长度和数量也有限制。它不能完全消除错误，所以还提供记忆查看、纠正、过期和删除 API。

### 69. 长期记忆如何处理重复和冲突？

**回答模板：** 新候选先按用户、类型和规范化内容生成稳定 ID，再按 topic 精确查找并做向量相似召回；若有候选，调用模型分类 duplicate、conflict 或 none。重复直接跳过，冲突沿用旧 topic 并写入新版本逻辑。关系返回必须引用候选中的已知 ID，否则判定失败，防止模型凭空指定对象。

### 70. 长期记忆为什么需要 PostgreSQL 和 Milvus 两套存储？

**回答模板：** Milvus适合相似检索，但不擅长版本历史、乐观锁、状态、有效期和分页管理；PostgreSQL 作为事实主库保存这些强结构数据。修改或删除先在 PostgreSQL 事务中更新版本并写 index job，后台再更新或删除 Milvus 向量，因此读写是最终一致。用户管理 API 查主库，构建上下文的语义召回走向量库并按 user_id 过滤。

### 71. 记忆异步任务如何保证不丢？

**回答模板：** 与 Precompactor 不同，长期记忆任务持久化在 PostgreSQL `memory_jobs`。Worker 用 `FOR UPDATE SKIP LOCKED` 抢占，并设置 lease；进程中断后 lease 过期可被重新领取。失败按 5 秒、30 秒、2 分钟、10 分钟、1 小时退避，最多 5 次后标记 failed。任务本身还检查目标 version，避免旧任务覆盖新记忆。

### 72. 长期记忆会不会污染不同用户？

**回答模板：** 所有记忆记录和查询都携带 user_id，Milvus 搜索使用过滤条件，管理 API 也以认证身份而不是请求体 user_id 为准。会话 Handler 校验 session.UserID 必须等于认证 Identity，不匹配统一返回 not found，减少资源枚举。默认无 OIDC 时大家共享 local-user，所以这种模式明确只用于单机可信环境。

## 七、多模型路由、熔断与容错（73—83）

### 73. 多模型路由的实际策略是什么？

**回答模板：** 当前实现不是按任务复杂度动态分类，而是对配置中的模型按 priority 升序、同优先级按名称排序；请求指定 model 时先选指定模型，否则选最高优先级。目标模型熔断后，再按同样顺序选择未熔断的模型。README 中“根据任务复杂度智能选择”表述偏超前，我面试时会按源码说明，并把复杂度路由作为后续优化。

### 74. 三态熔断器如何转换？

**回答模板：** Closed 正常放行，连续失败达到 failureThreshold 后转 Open；Open 在 timeout 内拒绝请求，超时后的第一次 Allow 转 HalfOpen；HalfOpen 中连续成功达到 successThreshold 回 Closed，任一失败立即回 Open。状态和计数用 mutex 保护，每个注册模型有独立 breaker，避免一个供应商失败直接拖垮全部模型。

### 75. HalfOpen 当前实现有什么并发问题？

**回答模板：** 代码在 HalfOpen 状态会允许所有并发请求通过，并没有只放一个或固定数量探针。高并发恢复瞬间可能形成惊群，如果上游仍不稳定会再次施压。改进方法是增加 half-open in-flight 计数或信号量，只允许少量探测，并对 Open 到 HalfOpen 加随机抖动。

### 76. 为什么一次 LLM 失败后不立即在同一请求切换备用模型？

**回答模板：** 当前 Router 只在调用前发现 breaker 已 Open 时 fallback；若本次主模型调用失败，它记录失败并把错误返回，不会自动重放到备用模型。这样避免非幂等语义、重复 token 成本和流式输出已部分发送后的拼接问题。同步、尚未输出且明确瞬时错误时可以做受限 fallback，但流式中途切模必须设计输出一致性。

### 77. 流式调用如何统计成功、失败和 usage？

**回答模板：** Router 包装供应商 channel，直到流关闭才结束 span 和记录 duration；首个 content 或 reasoning delta 记录 TTFT。任何 event.Err 只首次触发 breaker failure，完整结束且无错误才 RecordSuccess。usage 只在后端真实上报时累计，不做伪估算；因此 Grafana 还展示 usage 覆盖率，避免把缺失统计当成零消耗。

### 78. 后台记忆任务为什么可能影响前台请求？

**回答模板：** 记忆提取、冲突分类、Rerank、Compactor 和前台 Agent 共用同一个 `llm.Router` 与每模型 breaker。Benchmark 已出现记忆提取超时触发共享熔断，进而导致 SSE 失败。这是实际已知风险。优化上应按 workload 使用独立 breaker 和并发池，或者给后台任务单独模型、低优先级限流，避免后台失败污染前台健康度。

### 79. 模型健康检查是否参与每次路由？

**回答模板：** Client 有 Healthy 方法，但当前 `selectClient` 主要依据注册表、priority 和 breaker，并不会每次先发 ping。这样避免每个业务请求多一次模型调用。更合适的是后台周期健康探测并缓存状态，与 breaker 组合；请求路径只读状态，同时保留真实调用失败作为最终判断。

### 80. Context 取消是如何传播的？

**回答模板：** Handler 使用请求 Context，Harness 可再套默认超时；该 Context 传给 ContextBuilder、LLM、Tool、数据库和向量库。客户端断开或超时后，下游 SDK 应停止网络请求，SSE 指标标记 disconnected。后台长期记忆并不直接复用已取消请求，而是持久化 traceparent 后由独立 worker Context 执行，避免响应结束就取消任务。

### 81. 为什么错误要包装成业务错误码？

**回答模板：** LLM Router 把供应商错误包装为统一的 LLM_FAILED，Handler 对会话不存在、上下文过大和内部错误映射不同 HTTP 语义。这样外部 API 不依赖底层 SDK 文案，指标也能按类别聚合。内部日志保留原始 cause 便于排查，但对客户端不直接暴露密钥、SQL 或堆栈。

### 82. 如果所有模型都熔断怎么办？

**回答模板：** `findFallback` 遍历后没有可用模型就返回明确错误，当前不会排队等待恢复。上层同步接口返回失败，SSE 发 error 终止事件。生产环境可以增加快速失败后的 Retry-After、请求级降级答案或排队，但不能无限等待，因为那会占满连接和 goroutine。

### 83. 怎样验证熔断器正确性？

**回答模板：** 单元测试使用可控 client 构造连续失败、超时、半开成功和再次失败，断言状态、fallback 模型和指标；并发测试配合 race detector 检查计数安全。仓库还有熔断微基准和 benchmark fault 场景。真实环境还要观察 open 次数、fallback 数、恢复耗时和错误率，防止阈值过敏或失效。

## 八、安全、鉴权与多租户（84—92）

### 84. OIDC 验证流程是什么？

**回答模板：** 启动时读取 issuer 的 discovery 文档，校验返回 issuer 并获取 JWKS URL；只允许 HTTPS，localhost 开发例外。JWKS 仅接受 RSA、RS256、签名用途且参数合法的 key。请求解析 Bearer JWT，固定算法白名单，按 kid 找公钥，未知 kid 时刷新 JWKS，再校验 issuer、audience、exp 和 sub。

### 85. 为什么内部 user_id 不直接使用 JWT sub？

**回答模板：** 同一个 sub 在不同 issuer 下可能代表不同主体，直接使用会产生租户碰撞。项目用 `SHA256(issuer + 分隔符 + subject)` 的前 16 字节编码成内部 ID，既把 issuer 纳入命名空间，也避免把外部主体标识直接暴露到存储 key。前提是 issuer 配置稳定，切换 issuer 不会自动迁移旧数据。

### 86. 为什么越权访问会话返回 404 而不是 403？

**回答模板：** Handler 先取 session，再比较 session.UserID 和认证身份；不匹配时统一返回“会话不存在”。这样不向攻击者确认某个 session ID 是否真实存在，降低资源枚举信息泄露。内部日志仍可区分真正不存在和所有权不匹配，便于审计。

### 87. 默认关闭鉴权是不是安全问题？

**回答模板：** 如果暴露公网当然是。项目定位包含本地开发，所以默认使用 local-user，Compose 端口绑定 127.0.0.1，并在启动日志和 README 强提示。只要修改 bind host 或进入多人网络，就必须先开启 OIDC、限制 metrics，并以低权限进程运行。不能把“默认可运行”宣传为“默认生产安全”。

### 88. Prompt Injection 为什么不能只靠系统提示解决？

**回答模板：** 模型仍可能被恶意文档或网页诱导，所以系统提示只是软边界。真正安全依赖服务端硬控制：ToolScope allowlist、Skill 注册表、文件两阶段审批、只读数据库事务、OIDC 会话归属和调用上限。即便模型被诱导，也不应获得越权能力。高风险工具还应加入独立策略引擎和人工审批。

### 89. Metrics 接口为什么需要保护？

**回答模板：** 指标可能暴露模型名、错误率、吞吐、依赖状态和内部运行特征，能帮助攻击者侦察。Compose 默认仅本机绑定，但如果远程暴露服务，应通过网络 ACL、反向代理认证或独立管理端口限制 `/metrics`。同时 label 不能放用户输入、session ID 等高基数字段，避免隐私泄露和 Prometheus 内存膨胀。

### 90. Trace 中如何处理隐私？

**回答模板：** Span 记录阶段、耗时、模型、结果数量和 token 等元数据，不记录 prompt、对话、工具输入输出。API 返回 X-Trace-ID 便于排查，但请求 ID 和 Trace ID 概念分开。异步记忆和预压缩用 linked trace 延续关联，而不是强行复用已经结束的父 span。这样兼顾诊断和数据最小化。

### 91. 文件审批 proposal 如何防重放？

**回答模板：** Proposal 有 UUID、15 分钟 TTL、目标路径、预期内容和原文件 hash；apply 时必须在当前请求 ApprovedProposals 中，且文件 hash 仍一致，成功后应消费记录。即使拿到旧 ID，过期或文件变化也不能直接覆盖。跨实例部署时 proposal 必须在共享 Redis 中原子消费，避免两个实例同时应用。

### 92. 如果实现真正多租户还缺什么？

**回答模板：** 当前已有用户级会话和记忆隔离，但文档知识库主要是全局 collection，文件工具也没有租户 workspace。真正多租户需要 document tenant_id、Milvus 分区或过滤、PostgreSQL 行级权限、租户级密钥与限额、审计日志、数据删除流程和加密策略。还要对工具 allowlist 做租户配置，不能只隔离聊天记录。

## 九、可观测性、测试与 Benchmark（93—101）

### 93. 项目有哪些可观测手段？

**回答模板：** 日志使用 zap；Prometheus 记录 HTTP、Agent 模式与耗时、循环轮次、LLM 请求/错误/TTFT/token、熔断与 fallback、RAG、记忆、SSE 连接等；OpenTelemetry Trace 覆盖请求、Agent、LLM、RAG、Redis/PostgreSQL/Milvus 等阶段；SSE 与 Bubble Tea TUI 面向人展示执行事件。三者分别用于聚合、单请求定位和交互观察。

### 94. 如何定位一次慢请求？

**回答模板：** 先用 X-Trace-ID 在 Tempo 找总链路，判断时间在 context load、LLM、RAG、tool 还是 reflection；再看 Grafana 同期 P95、TTFT 和依赖耗时判断是单点还是系统性问题。如果 TTFT 高是模型排队或输入过长；TTFT 正常但总时长高，可能是多轮工具或慢流。最后结合结构化日志和 Agent iterations 复现。

### 95. Benchmark 为什么区分固定桩和真实模型？

**回答模板：** 固定桩响应确定、无外部模型波动，适合测 AgentGo HTTP、SSE、依赖和编排层容量；真实模型测试反映端到端体验，但受供应商排队、网络和模型生成影响。两者不能混算。当前固定桩 200 VU 的结果不能证明真实 GPT-5.5 能承载同等并发，这是报告里明确的边界。

### 96. 你会如何陈述现有 Benchmark 数据？

**回答模板：** 在 8 CPU、约 16.5 GB 的本地环境，固定桩压力场景最高 200 VU、43,657 请求、检查失败 0、P95 724ms；真实 GPT-5.5 的 30 请求、并发 5 小样本全部成功，P50 4.71s、P95 22.71s，任务吞吐 0.472 次/s。我要强调样本小，只是基线，不能外推最大稳定并发。

### 97. 为什么小样本 P95 容易误导？

**回答模板：** nearest-rank 算法下，10 个样本的 P95 实际就是最大值，极易被单个异常影响；反过来，小样本也可能没覆盖尾部故障。报告已经注明这一点。正式容量结论需要更长稳态、warm-up、重复实验、置信区间，并记录限流、错误率、资源曲线和供应商侧指标。

### 98. 测试策略如何分层？

**回答模板：** 纯逻辑如 RRF、熔断、token 估算、参数校验用单元测试；Router、AgentLoop、Handler 用 stub 验证协议和错误路径；PostgreSQL/Milvus 用可选 integration test；公开 HTTP 用 benchmark runner 和 k6；真实模型只做小规模端到端验证。这样日常测试快速稳定，外部依赖验证又不被省略。

### 99. 当前测试通过能证明什么，不能证明什么？

**回答模板：** 当前 `go test ./...` 全部通过，证明仓库默认单元测试和不需外部环境的包级测试没有回归。它不能证明真实 Redis、Milvus、PostgreSQL、Docling、SearXNG 和模型供应商在当前机器都可用，也不能证明性能目标。集成测试、Compose smoke、故障注入和真实模型 benchmark 仍需单独执行。

### 100. 如何测试 SSE 慢客户端和断连？

**回答模板：** 仓库用 benchmark stub 生成大量 stream chunks，客户端故意延迟读取，观察连接数、goroutine、内存、写超时和终止指标；断开时取消请求，确认下游 LLM/Tool 收到 Context cancellation，SSE outcome 记为 disconnected。还要测试反向代理缓冲关闭和 HTTP 200 后 error 事件语义。

### 101. 如果建立发布门禁，你会选哪些指标？

**回答模板：** 正确性门禁包括 smoke 成功率、工具选择准确率、检索 Recall@K 和关键安全用例；性能门禁包括固定环境 P95/TTFT、错误率和资源上限；稳定性门禁包括依赖故障降级、SSE 断连和队列恢复。真实模型有波动，不适合用单次绝对延迟硬卡，应用多次基线和允许区间。

## 十、难点、复盘与压力面试（102—110）

### 102. 这个项目最难的地方是什么？

**回答模板：** 最难的不是调用模型 API，而是处理跨轮状态和失败边界：工具 call 必须和结果正确配对；历史过长要压缩但不能丢并发更新；Milvus 和 PostgreSQL 双写要可修复；后台记忆又不能拖垮前台。我通过分层、稳定 ID、CAS、持久任务、超时和可观测事件逐个约束，而不是依赖模型“自己做对”。

### 103. 你解决过最有代表性的线上式问题是什么？

**回答模板：** Benchmark 中默认 10 秒记忆提取超时触发共享模型熔断，导致 3 次 SSE 有 2 次失败。排查依据是 Trace 与熔断指标能把后台 memory extraction 和前台 LLM failure 关联起来。实验中把提取超时调到 60 秒减少干扰，但这只是缓解；根因是共享 breaker，正确修复应做工作负载隔离、后台限流和独立模型策略。

### 104. 如果面试官说“这只是套壳调用大模型”，你怎么回应？

**回答模板：** 单次 Chat Completion 确实很薄，但项目的工程量在模型之外：原生 Function Calling 循环、工具发现与权限、混合检索、结构化 ETL、三层记忆和版本管理、上下文 CAS 压缩、多模型熔断、文件审批、OIDC、SSE/Trace 以及可复现评测。这些都由确定性服务端代码约束。我也不会夸大：模型效果仍依赖上游，动态选模和跨库强一致还未完成。

### 105. 为什么不用 Kafka 或其他 MQ？

**回答模板：** 当前规模下，长期记忆任务直接用 PostgreSQL 表加 `SKIP LOCKED`，同时获得事务、lease、重试和运维复用；预压缩即使丢失也可同步补偿，所以用进程内队列。引入 MQ 会增加部署和一致性成本。只有任务吞吐、跨服务消费、保留回放或隔离需求明显增长时，我才会考虑 Kafka/RabbitMQ。

### 106. 这个系统能水平扩展吗？

**回答模板：** HTTP、会话、工具热集和长期任务大部分基于 Redis/PostgreSQL，可支持多实例；memory job 的 `SKIP LOCKED` 也适合多 worker。但文档 Importer 是本地临时文件加进程内队列，Pipeline 锁也是进程级，Precompactor 可能重复工作，文件路径依赖实例可见文件系统。因此聊天链路较容易扩展，文档和文件能力还需对象存储、持久队列及分布式协调。

### 107. 你会优先优化哪三个问题？

**回答模板：** 第一，隔离前后台 LLM breaker 和并发预算，解决已观测的故障传播；第二，把文档导入改为“主库版本+outbox+索引 worker”，提升跨库一致性和多实例能力；第三，建立正式检索与 Agent Golden Dataset，用指标指导阈值、Rerank 和工具选择。它们分别对应稳定性、一致性和效果验证，比继续增加新工具更有价值。

### 108. 如果成本突然要求下降 50%，你怎么做？

**回答模板：** 先按 usage 覆盖率完整采集成本，再分场景优化：默认关闭 Reflection；记忆抽取批处理或只对高价值消息触发；Rerank 换小型 cross-encoder；减少初始工具 Schema；提升摘要复用；简单请求路由到本地模型。每项都要对正确率和延迟做 A/B，不能只换便宜模型导致工具调用失败率上升、反而增加轮次。

### 109. 如果要实现真正的“按复杂度选模型”，你会怎么设计？

**回答模板：** 我会先定义可观测特征：是否需工具、上下文长度、任务类型、风险级别和历史工具失败率；用规则或小模型输出候选等级，再结合模型能力、价格、熔断状态和租户预算打分。路由决策要写入 Trace，并保留调用方显式指定覆盖。最后用离线数据比较质量/成本 Pareto，而不是把“智能路由”停留在宣传语。

### 110. 你从这个项目学到的最重要一点是什么？

**回答模板：** 我最大的体会是 Agent 系统必须把概率性能力包在确定性边界里。模型可以决定“下一步做什么”，但工具权限、审批、循环上限、幂等、一致性、身份和观测必须由代码控制。另一个体会是失败数据比功能列表更有价值：共享熔断问题正是 Benchmark 暴露的，它让我从“功能能跑”转向关注隔离、尾延迟和可恢复性。

## 面试前使用提醒

1. 先熟记“分钟介绍”和 3 条主链路：普通 Agent、RAG 文档导入、长期记忆。
2. 回答任何量化问题都带上环境、样本量和边界，不把固定桩结果说成真实模型能力。
3. 不要声称已实现动态复杂度路由、跨库强事务、完整多租户或持久化预压缩队列。
4. 对不属于自己完成的模块，把“我实现”改成“项目中实现/我重点阅读和验证”。
5. 压力面试时优先讲实际权衡与已知不足，不用抽象术语掩盖代码现状。
