# AGENTS.md — llm-perf 工作指南

给在此仓库工作的 AI 助手。项目定位：客户自部署 LLM 推理服务的性能评测工具（Go 单二进制）。待办与能力规划见 `ROADMAP.md`，本文记录设计决策的"为什么"和工作纪律。

## 设计决策

- **测量哲学：受控变量优先**。定位是测量工具——每个维度可隔离、可归因；真实感来自战役设计（配置组合），不来自把真实因素揉进一次跑分。
- **轮间思考时间（think-time）不做**。TTFT/TPOT 是 per-request 指标，用户思考间隔不影响用户体验；到达节奏用开环 `request_rate`（Poisson）控制，比模拟用户发呆更直接、更可复现。
- **起点分散用"错峰启动"实现**。开环 multiturn（`request_rate` 在 multiturn 语义下 = 会话到达率）已天然支持会话陆续启动、各自增长；不采用开局注入历史的方式。闭环 multiturn 错峰用**内置指数爬坡发车**（默认开，可关；翻倍因子可配）：批次触发挂在"上一批全部完成首轮"事件上，节奏自适应服务端速度；首轮失败即终止战役，中间轮失败继续（带止损机制）。
- **冷、暖 prefill 分开测、分开标注**。真实 agent 长上下文请求多为暖前缀（prefix cache 命中，TTFT 只由新增 token 决定）；冷 156K 只出现在首请求注入巨量上下文或缓存失效，是少数路径，且冷、暖 TTFT 差一个数量级，混报无法解读。冷路径 ≡ single 大 prompt + 换 seed 击败缓存，无需新代码；冷多轮会让 prefill 总量随轮数二次方膨胀，标称并发与真实负载不对等。
- **TTFT 体验判据以 30–40K 输入档为准**（agent 场景）。现代 agent 产品基线上下文即 ~35K（系统提示 + 工具定义 + RAG 注入，用户发一句"你好"请求就带 35K 上下文），短输入档徽章代表不了 agent 体验。报告基线用 3 档制：优/及格（≤4K，MLPerf Interactive/Server 直引）+ agent 大上下文档（30–40K，推导值），判据与出处见 `docs/latency-baselines.md`。
- **tool-call 只落 `bench probe` 健康检查**（默认开），不进压测路径。客户业务若 agent 主导，纯推理压测的吞吐会偏乐观，对外汇报需主动说明此边界。
- **multiturn 起步上下文按 agent 真实形状设定（~35K）**。filler 模式 turn1 只有 15.5k（system 基座 3.2k），测不出大 prefill 冷启动，而首字延迟正是 agent 产品的真实痛点——起步值必须与 30–40K 体验判据同一量级。35K 是**推导值待取证**（AgentLens 按 session 首调 inputTokens 分布回填），先用配置实现、不等数据。形状取"起步 35K + 每轮 10K × 8 轮"（末端 ~108K）：保 8 轮历史深度优先于保单轮增量——多轮衰减曲线的横轴长度比每轮增量大小更关键；起步抬高后原 12.3K 增量会把末端推到 125K+，撞 128K 上限并触发"截断式跳过剩余 session"。
- **配置 schema 不保向后兼容**：迭代期改字段直接改类型，不做双轨兼容。
- **告警必须附可核实证据**（请求摘要 + 响应片段 + capture 路径），判据保守化（不确定降 WARN），capture 显式指定目录才落盘。

## 负载形态矩阵（对外汇报口径）

| 路径 | 场景 | 状态 |
|---|---|---|
| 暖（真实 agent 典型体验） | 错峰开环 multiturn，逐轮共享前缀 | 已有能力 |
| 冷（最坏角落） | single 大 prompt + 换 seed 击败缓存 | 已有能力 |
| 缓存收益量化 | `fixed_seed` true/false 对照 + server_metrics 命中率 | 已有能力 |
| 稳态 soak | 会话续跑（P2 待办，见 ROADMAP） | 待做 |

## 工作纪律

- 未经用户明确许可不 commit / push；提交时单个 commit 收口。
- `configs/customer.yaml`、`configs/customer-*.md` 含客户端点与 key，已被 .gitignore 覆盖，严禁入库。
- ROADMAP 只写"接下来加什么能力"的清单，不展开论证；重要设计拍板沉淀到本文件。
- 验证基线：`go build ./... && go vet ./... && go test ./... -count=1` 全绿再交付；HTML 报告改动用真实 JSON 渲染人工核对。

## 工程陷阱

- 同一文件多个 Edit 并行会互相覆盖 → 串行编辑，改完 grep 复核。
- Bash `grep` 对部分模式静默返回空 → 扫代码用 Grep 工具，关键结论交叉验证。
- bench CLI：配置用 `-c`；并发场景需 `--concurrency cfg`；输出别用 `head` 截断（SIGPIPE 杀进程），重定向到文件。
- `TurnMetrics` 新增字段标 `json:"-"`，避免污染压测 JSON 契约；报告侧能算的指标不进 Go。
- 展示口径 = 执行口径：战役画像（启动总览）的档位列表与上下文 reach 必须复用 `ClampLadder` 与现有估算函数，不另写一套估算；reach 是估算值，输出带 `~`。
