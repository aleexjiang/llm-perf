# ROADMAP

> 原「三件事」（probe tool-call 检测 / trace 回放增强 / 硬编码改造）已全部完成（第 1–3 节）。
> 当前在途：5.8 `slo:` 配置段、5.9 起步值补样（n≥20）、6 Anthropic Messages API 兼容（触发后启动）。
> 5.5 跨运行比对暂缓（等真实比对需求）；8.2 依赖的 min_tps 真机标定仍待做（配置已就绪，阈值待校准）。
> 定位不变：本工具负责**发现**问题，修复由服务方完成后重测。

---

## 1. probe 增加 tool-call 检测（默认开启，`--no-toolcall` 关闭）【已实现，2026-09-09】

只做**前置门禁**：告诉客户引擎能不能正常调工具、不能的话该改哪里。不测性能、不进主压测路径。

**检查项**（多 4 次请求，秒级）：

| 项 | 请求 | 通过判据 |
|---|---|---|
| T1 基线连通 | 复用现有 `chat_nonstream` | 失败即跳过后续 |
| T2 非流式结构化 | `tools` + `tool_choice: auto` | `tool_calls` 数组、name 在请求内、arguments 可解析、`finish_reason=tool_calls` |
| T3 tool_choice 探针 | `tool_choice: required` | 与 T2 联合判定：T2 败 T3 成 = 配置问题；双双败 = parser 缺失 |
| T4 流式聚合 | auto + stream | 按 `delta.tool_calls[i].index` 分桶聚合，与非流式一致；重点看函数名丢失 / 调用数变少 |
| T5 标记泄漏 | 对 T2/T4 的 content 正则 | 命中 `DSML` / `tool▁calls` / `<tool_call>` 即报内容污染 |

**判据分级**：硬特征（DSML 泄漏、纯 JSON 数组、arguments 非法 JSON）→ FAIL；
软特征（finish_reason 错位、自然语言"我来调用"、required 报 4xx）→ WARN。
检查自身超时/网关错 → 输出"检查未完成"，不算 ❌（避免客户拿我们的超时去提工单）。

**失败时给可行动结论**（可直接贴给厂商），如：content 含 DSML ⇒ 查 `--tool-call-parser`；
有 tool_calls 但 finish_reason=stop ⇒ 升级引擎；请求 4xx ⇒ 网关未透传 tools。

**实现落点**：
- `sse.go` `deltaPayload` 补 `ToolCalls` 字段（白名单已有 `tool_calls`，值当前被丢弃）
- `client.go` `TurnMetrics` 加 `ToolCalls`（`json:"-"`，不进压测数据契约）
- 新增 `internal/engine/toolprobe.go`：工具定义 + T2-T5 判据 + 失败细分
- `probe.go` `doChat` 参数化（现硬编码"请原样回复：OK"）；`ProbeOptions` 加 `ToolCall bool`
- `cmd/bench/main.go` 传参 + `--no-toolcall`

**验收**：fixture 回放单测（六种失败特征各一份，验证判据命中）+ 好引擎真机不误报 + 弱端点不崩。
检出能力暂无真实坏引擎可验，今后客户环境碰到坏引擎时用 `--probe-capture <dir>` 落盘原始响应回传补 fixture。

---

## 2. trace 回放增强（P0：保真度）【已实现，2026-09-09】

- **现状**：`trace.go` 只取 user 侧消息，真实会话的 assistant / tool 消息（含大段工具结果）全丢，
  测的是小 context 却标称"回放真实会话"，长上下文衰减曲线横轴失真。
- **要做**：`dataset.replay_mode: user_only | full`（默认 `user_only` 保持兼容）；
  `full` 按原序注入全部 role；`multiturn.max_reply_chars` 可配（默认 2000）。
- **注意**：`role: tool` 消息须带 `tool_call_id`，缺失跳过并计 warning，不静默丢弃；
  `reply[:2000]` 现在按字节切，一并改为按 rune 截（中文切半个字符）。
- **验收**：真实 agent trace 上 full vs user_only 对比 context 深度与 TTFT——物料已就绪（第 4 节 trace-real-16）。

---

## 3. 硬编码改造（一次做完"客户环境适配"）【已实现，2026-09-09】

全量清单曾扫描出 32 处（见 git 历史），必修的就两组，其余一次改造顺带处理：

**必修（阻塞真机 / 数据失真）**：
- 认证：`Bearer ` 硬编码 3 处（`probe.go:94/166`、`client.go:346`）→ `auth_scheme: bearer | raw | none` + `auth_header`（自定义 header 名），默认 `bearer` 兼容
- 回放：见第 2 节（同一处改造）

**建议（一次改造顺带）**：
- 接口路径：`/chat/completions`、`/metrics` 可配 ✅
- probe 超时：`probe.go:96` 15s、`probe.go:168` 120s → 读配置 `timeout_seconds` ✅
- 引擎识别：`DetectProvider` 无法识别时不回落 vLLM，改为显式提示"未知引擎，服务端指标跳过"；Server 头匹配不到不生成交叉验证命令时打一行日志 ✅
- SSE 字段白名单：`deltaPayload` 补 `tool_calls` 值 ✅；思考字段命名放开为可配置列表——
  **被双字段自动识别（`reasoning` / `reasoning_content`）+ 未知字段告警方案覆盖，无需配置化**

其余 20 余处（常量、预览截断、文件大小上限等）**不动**，真机反馈有问题再说。

---

## 4. 数据来源（支撑第 2 节验收）【已完成，2026-09-10】

**物料已入库**：`configs/fixtures/trace-real-16.json.gz`（+ 同名 `.md` 清单）——16 条真实 agent 会话（脱敏），
单会话峰值 prompt 20k–480k 四档，含 assistant/tool 消息与工具定义；`LoadTrace` 实跑
`FullReplay=true`、缺 `tool_call_id`=0，`replay_mode: full` 真正生效不退化。逐条清单与脱敏规则见 `trace-real-16.md`。

**导出协议备忘**（AgentLens OpenAPI，全 POST + JSON）：
- 地址必须 **http://**（https 走内网代理会 502）；Header `X-Agentlens-Token: <明文>`，非 Bearer。
- 调用链 `space/list → scenario/list → traceList → traceDetail → spanDetail`；
  traceList/spanDetail 默认只查最近 1h/24h，**必传 start_time/end_time**（东八区）。
- **口径坑**：traceList 一条 = 一次用户交互（trace），其 `inputTokens` 是链路内多次 LLM 调用的**累加**；
  单次调用的真实 prompt 只在 spanDetail 的 attributes（`gen_ai.usage.input_tokens`）。
  `attr_type=attr`（≈23KB，仅元信息）供批量扫描；`raw-attr`（≈200KB）才含完整 prompts + 工具定义。
- 同一 session 内第 N 次调用的 prompt 包含前 N-1 次全部 history，**不可拼接**——
  取首调（起步基线）、末调（完整上下文）或相邻差分（每轮增量）。
- 取样注意：session 内 seq 靠前的 trace 往往只有 1 个 user 轮（会被 `min_turns=2` 过滤），挑 seq≥3；
  候选池可能混入当前会话自身（含明文 token），必须排除。
- 脱敏重点：**工具定义**（skill 清单带绝对路径与内网域名）是残留风险最高处，须与 messages 一并过脱敏；
  原始未脱敏数据不入库，脱敏完成后即删。

---

## 5. 测量方法论补强（来自两篇生产实测报告的借鉴）

> 背景：vLLM 生产工程 / ModelDoctor 两篇实测显示，工具测得出"正确但无意义"的数：
> 同输入同并发下输出 64→512 让 TPOT 降 54–77%；长短混跑时 TTFT 中位数几乎不动、p99 涨 5.9 倍。
> 前提：per-request `TurnMetrics` 已全量落盘，纯报告侧可算的就不碰 Go。

**5.0 开工前清理（无用兼容逻辑审查结论，2026-09-08）**【已实现，2026-09-08】

配置不保向后兼容（见 5.1），开发前先清掉三类存量：

*因"保兼容"而存在的默认值与格式：*
- `dataset.replay_mode` 默认值 `user_only` → 翻转为 `full`（`config.go:587` 注释明写"与历史版本一致"）；
  `user_only` 降为显式选项（ShareGPT 问答类数据集、快速对比仍有用）。连带更新 README 与本文件第 2 节。✅
- trace sessions 裸二维数组形态（`[["u1","u2"],...]`，`trace.go:266-278`）：README 与示例均未文档化，
  仅 `trace_test.go:61` 在测——无使用者，删除（连带测试子用例）。✅
- `Config.Fillers()`（`config.go:892`）：纯转发 `c.FillerLang` 的包装，11 处调用点内联后删除。✅

*重复实现（改造未收敛）：*
- `smetrics.go:59-77` `applyAuth` 与 `engine/auth.go` 同构（注释自述"不能反向 import engine"）；
  `scenario.go:84-86` 手动三字段复制是同一裂痕。解法：Auth 下沉为独立低层包（engine 与 smetrics 共同依赖）。✅
- `smetrics.go:344` `DetectProviderName(nil)` 返回 `"vllm"`，与其注释『""=未识别』矛盾；
  实际调用路径 sample 恒非 nil——nil 分支改返回 `""`，统一走"未识别"告警。✅

*仓库卫生：*
- `configs/smoke-all.yaml:11` 指向 `/tmp/trace-sessions.json`（不存在、无生成脚本）→ 新克隆 `go test` 必挂；改用仓库内 fixture。✅
- `bench-linux-amd64`（10MB）移出 git，`.gitignore` 补条目。✅

*排查过、确认保留的*：报告工具单/多模型两种落盘布局（递归 glob 一条覆盖，无分支）；
probe 120s 兜底（零值 fallback，配置已生效）；sharegpt 键名/词表变体与引擎指标命名候选表（数据集与引擎生态兼容，是功能）；
thinking mode/levels 双轨（levels 优先已显式声明并告警）；api_key 三来源优先级（设计非遗留）。

**5.1 输出长度作为扫描维度**（防结论方向性错误——最优先）【已实现，2026-09-08】

- `max_tokens` 直接改为「标量或列表」：`max_tokens: 256` 或 `max_tokens: [128, 256, 512]`，
  Go 侧自定义 `IntList` 反序列化（一个 unmarshaler，`config.go` 三处字段共用），**不新增 ladder 字段**。✅
  项目在迭代期，配置 schema 不保向后兼容（2026-09-08 拍板），直接改类型。
- 场景层对列表逐值构造场景；结果行加 `MaxTokens` 字段（SingleRow/MultiturnRun/ConcurrentLevel）；✅
  报告按（场景 × 长度）分组出曲线。✅
- 配置守卫：`max_tokens` 为单值时打提示"输出长度未扫描，容量结论可能系统性偏悲观"。✅（随 5.3 落地）
- 实现中的两个补充决策：① 加载时对列表排序去重（升序执行）；② 思考 floor 抬高后相邻同值去重
  （floor=2048 会把 32/64 两档收敛成同一档，不去重就重复扫）。✅
- `Thinking.MaxTokensList()`：逐档应用 floor 保护的辅助方法，三场景共用。

**5.2 并发报告补 p95 / p99**（防中位数骗人）【已实现，2026-09-08】

- `gen_html_report.py` 的 `stats3()`（median/min/max）扩为含 p95/p99；数据已在原始 JSON 里，纯 Python。✅
- 注意样本量陷阱：p95/p99 只在并发场景显示（level × runs_per_worker 样本充足）；
  single/multiturn 样本少（默认 3），显示样本数、不足时留空不硬算。✅（MIN_PCT_SAMPLE=20，不足显示 `n<20`）

**5.3 配置守卫**（防"测的工作点不对"）【已实现，2026-09-08】

- 加载配置时检查：输出长度远小于典型业务值、并发档位未覆盖、扫描维度缺省——追加进现有 `cfg.Warnings`，约 30 行。✅

**5.4 运行环境与配置原文存档**【已实现，2026-09-08】

- `Report` 加 `Environment *ProbeResult`（probe 已有 Server / EngineGuess / ModelMaxLen）+ `ConfigRaw string`（YAML 原文塞进 JSON，约 5 行）。防止几周后无法复现"上次那组数是什么配置跑的"。✅

**5.5 跨运行复现性比对**（暂缓，2026-09-11 拍板——等真实比对需求再启动；恒为脚本不进 CLI）

- 新增 `scripts/compare_runs.py`：传两份结果 JSON，输出各指标变异系数，回答"这两个数差 8% 是真的吗"。保持脚本形态，不进 CLI。

**5.6 混合负载**【已实现，2026-09-08】

- `Concurrent` 加 `mix: [{weight, label, prompt_tokens, max_tokens}]`：请求形状按权重混跑；✅
  闭环按平滑加权轮转（nginx 同款）展开成确定性序列、请求按发射序对号入座（同配置可复现），开环按到达序号轮转。
- `ConcurrentLevel.Shapes` 逐形状聚合（count/TTFT/E2E/tok·s 中位数）落盘，`Requests` 全量不动——压测 JSON 契约未污染。✅
- `mix` 与 `multiturn: true` 互斥报错；配置后 prompt_tokens/max_tokens 单值与 max_tokens 扫描失效（告警）；
  各形状 max_tokens 独立过思考 floor。✅
- 报告：并发表"输出 tk"列显示 mix(labels)；新增"形状分解"表（权重/实测占比/逐形状中位数）；
  结论区自动出形状间尾延迟差异（长短形状 TTFT 比）与混跑 vs 均匀吞吐对比（<85% 标注明显衰减）。✅
- 背景依据：客户线上长短混跑实测吞吐减半；vLLM 该版本插队机制直接崩，只能实例隔离。
  5.1 输出扫描与 5.2 p95/p99 与本能力配套：均匀负载下 p99 意义有限，混跑才能测出真实尾延迟与容量折扣。

**5.7 负载保真度：闭环错峰发车与会话续跑**（爬坡发车已实现 2026-09-11；会话续跑 P2 等 soak 需求）

- 闭环并发指数爬坡发车（内置机制，默认开，`concurrent.ramp: false` 可关）：✅
  首批发 1 个 worker（会话/单轮均可），等该批**全部完成首轮**后放下一批
  `min(上批×factor, 剩余)`，`concurrent.ramp_factor` 默认 2（可设 3）；批次节奏由服务端
  首轮实际耗时决定（自适应，无需按端点调参），在途会话深度混合自然涌现。
  `MultiturnRun` 落盘 `batch`（批次号）与 `start_offset_s`（相对本档位启动偏移），
  报告侧可标爬坡窗口；`concurrent.aborted` 记录提前终止原因。
- 失败语义（爬坡启用时；`ramp: false` 保持旧行为，失败只逐条记录）：✅
  **任一 worker 首轮失败 → 终止本档位与后续场景**（fail-fast，首轮挂大概率模型服务有问题）；
  中间轮失败会话继续，止损双保险——会话内连续失败 3 轮提前终止该会话，全局连续失败 ≥ 2×level
  终止本档位（默认常量，必要时再配置化）。
  实现落点：`runClosedRound` 派生轮级 ctx（fail-fast/止损取消在飞请求），`collectSessionTurns`
  加轮次钩子（turn 0 发首轮信号 + 失败计数）；批次计划抽为纯函数 `rampBatches`（单测覆盖）。
- 会话续跑（P2，等真实 soak 需求再做）：`renew: true` + `duration_seconds`（闭环从次数语义扩展出时长语义），
  会话滚完换新内容重开（上下文清零重涨），暂态后在途会话年龄铺满 0~turns 区间，测稳态吞吐与 KV cache 压力；
  报告标注稳态窗口（暂态剔除或单独标注）。与爬坡组合 = 最接近线上稳态负载。

**5.8 报告体验基线评估（3 档制）**（报告侧已实现；`slo:` 配置段待做。判据与出处见 `docs/latency-baselines.md` 第 7 节）

- 已实现（`gen_html_report.py`）：报告新增"8 · 体验基线评估"区——三场景按输入档归组
  （单发 TTFT 跨输出档池化、TPOT/tok/s 取最大输出档；多轮逐轮归档；混跑逐形状评估），
  ✅/⚠️/❌ 徽章 + 结论段 + 出处注释；TTFT 优先 p99、样本 <20 退回中位数并标注；
  thinking=on 不打 TTFT 徽章（TTFAT 口径）；基线单元写入 perf-summary JSON；不进退出码
- 待做：`slo:` 配置段合流 goodput 阈值 + `baseline: true`（默认开），阈值内置默认可覆盖
  （当前阈值为脚本内置常量 `SLO_TIERS`）
- 档1 优（≤4K：450ms/40ms）、档2 及格（≤4K：2s/200ms）= MLPerf Interactive/Server 直引；
  档3 agent 大上下文（30–40K：优 3s / 及格 6s）= 推导值（particula 10K 实测线性外推 + 405B 档上限佐证），
  报告中标注"推导值"
- 档位归组：≤4K 打档1/2 徽章，≥24K 打档3 徽章，4K–24K 只报数值（prefill 斜率见第 7 节分析表）；
  agent 场景 TTFT 判据以档3 为准（现代 agent 产品基线上下文即 ~35K）

**5.9 负载保真度：multiturn 起步上下文对齐 agent 真实形状**（配置联动已实现 2026-09-08；首轮取证完成 2026-09-10，暂维持 35k）

- 问题：filler 模式 turn1 ≈ 15.5k（system 基座 `system_tokens 1000` + `tool_defs_tokens 2000`，×1.07 模板开销），
  远低于真实 agent 产品的首调上下文规模 → multiturn turn1 测不出"大 prefill 冷启动"，而首字延迟恰是 agent 产品的真实痛点。
- 目标：turn1 起步 ≈ **35k**。原始为推导值（与 5.8 档3 同源）；取证协议与坑见第 4 节。
- 取证结果（2026-09-10，n=4，办公/轻开发场景各 2 session）：session **首调** prompt 31.7k / 43.0k / 45.0k / 67.4k，
  **中位 ~44k**；首调缓存命中 0–16%、TTFT 4.3–5.8s，第二次调用起 90–99%、TTFT 1.3–2.6s
  （TTFT 由缓存主导，与大 prompt 关系弱——真实会话几乎不测冷 prefill）。
  结论：35k 假设偏低约 25%，但 n=4 不足以定基线——**暂维持 35k 起步、配置不动**；
  补样至 n≥20（覆盖 CLI/WorkBuddy/app_cloud 等入口与更多场景）出 P50/P90 后再定，
  上调路径：`system_tokens + tool_defs_tokens` 基座 21k → ~44k 档。
- 配置联动 ✅（无需改代码，按预期）：`example.yaml` 与 `customer.yaml` multiturn 已改为
  `system_tokens: 18000` + `tool_defs_tokens: 3000` + `turn_tokens: 10000`、`turns: 8`（turn1 ≈ 32.5k，末轮 ≈108k）。
  qwen3.8-27b.yaml（256k 模型自算预算，**该文件已迁出仓库**，2026-09-11）与 smoke 系列保持不变。
- 末端约束（2026-09-08 定，方案 B）：`turn_tokens` 由 12300 降到 **10000**、保持 `turns: 8`
  → 末端 prompt ≈108k（thinking on 含 8k 输出 ≈116k），128k 模型留 ~12k 余量；probe 确认各模型上限后再微调。
  代价：逐轮增量变小（模拟"每轮新增工具结果 + 追问"，10k 仍不失真）。✅ 口径已注明：multiturn 报告 Note
  追加"上下文 ~Xk 起步 → 末轮 ~Yk（每轮增量 Ntk：模拟每轮新增工具结果+追问）"，防止被当配置失误。
- 可选（不占决策位）：借 schema 不保兼容窗口把 `system_tokens` 语义拆为 `base_context_tokens`（system + tools + memory 三段）。
- 连带：`config.go:851` 深上下文告警（base + turns×turn_tokens ≥ 100k）改后必触发，属预期 ✅；
  probe `LargestPromptTokens`（`config.go:1134`）随配置自动跟上，无需改动 ✅。

**5.10 启动时打印测试画像（plan profile）**【已实现，2026-09-08】

- 问题：`sessions` / `turns` / 上下文爬升 / 请求数只在逐请求日志或结果 JSON 里间接可见，
  开跑前没有"这次要跑什么形状"的总览，核对配置只能翻 YAML。
- 实现 ✅：`internal/scenario/plan.go` 新增 `PlanSummary(cfg, modelFilter, items)` 纯函数 +
  `internal/report` 的 `Plan/PlanModel/PlanScenario` 结构与 `Render()`；`cmd/bench/main.go` 在"执行计划"行后打印总览块。
  逐模型逐场景一行（single 档位矩阵 × runs × thinking / multiturn sessions×turns 与上下文 ~起步→~末轮 /
  concurrent levels × runs/worker 或每用户会话），末行总请求估算（不含预热与金丝雀）。
- 展示口径 = 执行口径 ✅：档位复用 `ClampLadder`（含 max_prompt_tokens 截断→有效轮数）、输出档与 floor 复用
  `MaxTokensList`、变体复用 `Variants`、模型差异复用 `ForModel`——估算与场景循环逐层同构（含 plan_test.go 三个单测）。
  reach 按 4 字符/token + 1.07 模板开销，展示带 `~` 前缀；trace 模式按取样上限 16 会话估算并标注。
- 报告侧 ✅：同一份 Plan 随每份场景 JSON 落盘（`Report.Plan`，`PartitionByModel` 带入分区），
  `gen_html_report.py` 第 2 节渲染"测试画像"表（与 5.4 配置原文存档合并消费）。E2E（mock 两模型四场景）：
  打印估算 36 请求 = 实际执行量，HTML 画像表正常渲染。
- 连带修复：`gen_html_report.py` gen_conclusions 的 decode 吞吐对比在 TPS=0（mock/异常数据）时除零崩溃，加 >0 守卫。

**批次建议**：先做 5.2 + 5.3 + 5.4（半天、零风险、不动 Go 主流程）→ 5.1（防方向性错误）→ 5.6（等场景）。
（2026-09-08 更新：5.0–5.4 及 5.6 已全部实现；5.8 报告侧 + 5.9 配置联动 + 5.10 已实现。
2026-09-10 更新：5.9 首轮取证完成（首调中位 ~44k，n=4，暂维持 35k）+ 真实 trace 物料入库（第 4 节）。
2026-09-11 更新：5.7 爬坡发车已实现。剩余：5.5（恒为脚本，暂缓）、5.7 会话续跑（P2 等 soak 需求）、
5.8 `slo:` 配置段、5.9 补样至 n≥20 后定起步值。）

---

## 6. Anthropic Messages API 兼容（P2，触发后启动）

> 触发条件：首个 Anthropic-only 客户环境（agent 侧以 `/v1/messages` 消费，常见于 Claude Code /
> Claude SDK 生态 + LiteLLM / one-api / 自研网关转换链路），或引擎横评需要覆盖该协议面。

- **不是换 URL 就完事**：SSE 是事件制（`message_start` → `content_block_delta`(`thinking_delta`/`text_delta`)
  → `message_delta` → `message_stop`），与 OpenAI 的单流 chunk 模型完全不同；TTFT 分思考/正文两口径的
  计时锚点要按事件重锚。
- **适配清单**：
  - `internal/engine/sse.go` 对位物：Anthropic 事件解析器（`sse_anthropic.go`），按 `delta.type` 分流思考/正文；
    usage 拆在 `message_start`（input_tokens）与 `message_delta`（output_tokens）两处回收。
  - 口径校准：Anthropic 缓存是显式上报（`cache_read_input_tokens` / `cache_creation_input_tokens`），
    与 OpenAI `cached_tokens` 语义不同——报告侧按数据源标注口径，不混算。
  - 请求侧差异：system 顶层字段、`max_tokens` 必填（三场景构造请求时注意）、`stop_reason`
    （`end_turn`/`max_tokens`/`tool_use`）替换 `finish_reason` 语义。
  - probe T2–T5 tool-call 判据翻写：`tool_use` content block + `stop_reason=tool_use` 对应
    OpenAI 的 `tool_calls` + `finish_reason=tool_calls`；T4 流式聚合按 content block 序号分桶。
  - smetrics 不受影响（服务端 Prometheus 与 API 格式无关）。
- **工作量定位**：中等——新增协议适配层 + probe 判据翻写，不碰测量核心算法与报告管线；
  报告侧已实现的三档基线、p95/p99、混合负载分解全部直接复用。
- **验收**：fixture 回放单测（参照 toolprobe 的六特征模式）+ 网关转换环境真机（可用 LiteLLM
  现场把现有 OpenAI 端点转 Anthropic 格式自造环境，零客户依赖）。

---

## 7. 降速熔断 stall_guard（长测不白跑）【已实现，2026-09-11】

- **问题**：max_tokens 放宽到 10K 后单轮全量要数小时；服务端中途退化（decode 掉到十几 tok/s）时，
  客户端只会老实把剩下的档位/场景跑完，时间全打水漂。
- **做法**：`stall_guard` 按**聚合输出速度**判定——窗口内所有在飞请求的输出 token 之和 / 窗口时长
  （并发下单请求慢可能只是排队，聚合才反映服务端真实产出能力）；速度持续低于 `min_tps` 达
  `window_seconds` → 只中止**当前场景**（不是整轮），冷却 `cooldown_seconds` 后继续下一个场景。
  已完成数据照常落盘，报告 `note` 与 run.log 标注熔断原因、现场速度与持续时长。
  （冷却层级的分层评估与后续改造见第 8 节）
- **两个防误触发设计**：空闲（没有请求在飞）不计入判定；有请求在飞但均未吐出首个 token
  （长 prompt 的 prefill 阶段）也不计入——只有"正在输出却输出得慢"才算降速。速度回升即重置计时
  （对应"持续不升"的语义）。
- **CLI**：`--stall-tps / --stall-window / --stall-cooldown` 覆盖配置，`--no-stall-guard` 直接关闭。
- **验收**：`internal/engine/stall_test.go`（零产出触发 / 高于阈值不触发 / 回升重置 / 空闲与 prefill
  不触发 / 回调只一次 / nil 安全）+ `internal/config/config_test.go`（默认值填充与非法值报错）
  + `scripts/stall-e2e/` 假慢速服务端端到端回归（触发 → 中止当前场景 → 冷却 → 续跑下一场景，
  报告 note 留痕）。

---

## 8. 熔断冷却策略与降速可观测性（2026-09-11 讨论结论）

> 起因：第 7 节的冷却只发生在**场景之间**（熔断 → 止本场景 → 冷却 → 下一个场景）。讨论
> "冷却要不要下沉到每个场景里"后，结论是**分层评估、两层明确不做、两层待做**。
> 前置：`min_tps` 需按实例稳态 decode 速率标定——实测日志里 `Avg generation throughput:
> 16.6 tok/s` 若属实，20 的阈值会在**正常运行时**触发，届时冷却/探针/续跑怎么设计都无从验证。

**8.1 已评估、明确不做（记录结论，避免重复讨论）**

*档位之间常态化冷却（每跑完一个上下文/并发档固定停 N 秒）*
- 直觉「晾一会儿清掉缓存」在本部署上不成立：vLLM 的 prefix cache 是**显存里的 LRU 块缓存**，
  空闲不淘汰，只有新请求挤占显存时才驱逐——隔离缓存靠 `seed_salt`（每轮换盐让 prompt 内容变），
  不靠等待。KV cache 占用也随请求结束自然释放，与时间间隔无关。
- 温度墙 / 抢卡属环境问题，插空档只是掩盖症状；固定间隔还会让 E2E 不再代表稳态服务
  （报告数字偏乐观，且从数据里看不出来）。
- 代价具体：single 是 6 档 × 2 输出档 × 3 runs = 36 次请求，每档插 60s 就多半小时级纯等待。

*场景内「熔断 → 冷却 → 从断点续跑同一场景」*
- ① 档位是**浅 → 深**排序（4k → 204.8k），熔断几乎必然发生在尾部最贵的档，那时前面约 85%
  的数据已在手——续跑能救回的，恰好是最不可能因"歇 5 分钟"变快的那几档。
- ② 只对**暂态的服务端退化**有效。熔断成因分两类：(a) 形状本身慢（深上下文 prefill、并发 8
  超订、thinking 长思考）；(b) 服务端整体退化。若是 (a)，续跑会立刻二次熔断——白付 5 分钟冷却
  + 一档完整超时（900s 兜底），换来同一份失败。
- ③ **可比性被破坏（关键）**：续跑成功会让同一份 JSON 的前半截跑在健康实例、后半截隔了 5 分钟
  跑在"刚缓过来"的实例，事后看图出现无法解释的台阶；而熔断是在持续 10 分钟低速**之后**才
  触发的，意味着触发前那十几分钟的数据其实也已带病——正确结论是"该场景整份可疑"，而非
  "补几档进去凑齐"。
- ④ 实现成本不低：`Scenario` 接口要支持跳档 + 返回断点，三套场景循环结构各不相同（阶梯 /
  会话 / 并发档），两份 partial 报告还要按档位合并去重，画像与 warnings 口径都得重定义。
- **替代做法**：熔断即认输——JSON 照常落盘但标注"降速熔断、不可比"；要补数据，人工在实例
  修好后单跑那一个场景或那几档（如 `--max-ctx 40960` 收窄）。人在回路判断"是否真修好"，
  比自动续跑可靠。

**8.2 冷却 + 探针（替换盲等计时）**【已实现，2026-09-11；`min_tps` 真机标定仍待做】

- **问题**：冷却纯计时两头堵——服务端已恢复 → 白等 5 分钟（本轮动辄数小时，5 分钟 × N 不可忽略）；
  服务端没恢复 → 也白等 5 分钟，下个场景照样熔断，继续烧时间。
- **做法**：冷却结束时先发一条**短探针**（4k prompt / 256 输出 × 3 取中位，thinking 关），
  实测聚合 tok/s：`≥ probe_factor × min_tps`（默认 2）→ 继续下一个场景；低于阈值 → **直接结束
  整轮**，打印「服务端未恢复，停止后续场景」，被熔断场景的 JSON `note` 追加
  「熔断后探针 x.x tok/s < 恢复阈值 y.y（×min_tps），停止后续场景」。
  实现 ✅：`cmd/bench/main.go` 冷却后调 `recoveryProbe`（复用 `engine.Client` 单发请求，此时
  `client.Stall` 已置 nil，探针不触发二次熔断）；落盘路径由 `run` 记录、`patchReportNote`
  读改写回（读写失败只告警不丢数据）。
- **语义变化** ✅：`cooldown_seconds` 退化为"给恢复留的时间窗"，判定交给实测；`min_tps` 从
  "只用于中止"升级为"决策继续与否"。
- **配置** ✅：`stall_guard.probe_factor`（默认 2；显式 0 = 关闭探针、退回纯计时；负数报错）；
  CLI `--stall-probe-factor` 覆盖（>0 生效，关闭只能走配置）。
- **验收** ✅：`scripts/smoke.sh` 12b/12c 两例——mock 持续慢 → 首场景熔断后探针不达标 →
  停整轮且 note 留痕；mock 首个慢速请求 4s 后恢复（恢复时钟锚定首请求，对 bench 启动方差免疫）
  → 探针通过 → 继续下一场景。**注意：探针实测 tok/s 含 chunk 开销略低于 mock 标称速率，
  时序裕量按此校准**。

**8.3 降速采样序列落盘**【已实现，2026-09-11】

- **问题**：现在采样循环每 `LogEvery`（默认 60s）只在**低于阈值时**打一条 ⚠️ 日志，且只存在于
  日志、无任何落盘。事后只剩一个熔断点，无法回答"从第几档开始掉、是渐变还是断崖、掉了多久"。
- **做法**：把每次采样的 `(t_s, agg_tps, in_flight, emitting)` 追加写入侧文件
  `<结果 JSON 同名>.stall.csv`（与 `output_dir` 同层）；**高于阈值的正常段也记录**，否则看不出
  掉速的起始点。体积估算：2s 采样 × 3 小时 ≈ 5400 行 ≈ 200KB，可忽略。
- 实现 ✅：`StallGuard.W io.Writer`（nil = 不落盘，仅采样 goroutine 写、无并发写）；
  首行表头 `t_s,agg_tps,in_flight,emitting,phase`，phase 取 `emit / prefill / idle`——
  空闲与 prefill 相位 agg_tps 留空（"测不出"≠ 0，与不可测时长的口径一致）；熔断时以
  `# tripped: rate=… low_for=… min_tps=…` 注释行就地留痕。`Report.StallTrace` 记相对路径
  （多模型分区 = `../<同名>.stall.csv`）；main.go 场景开始建文件（先 MkdirAll——首场景的
  -o 目录此时还不存在）、`<-guardDone` 等采样 goroutine 退出再关文件。
- 连带修复 ✅：`stall_guard.sample_seconds` 由 int 改 float64——此前 `0.5` 会被静默吞成 0
  再回落默认 2s（亚秒采样是熔断回归的合理需求，schema 不保兼容窗口内直接改类型）。
- **开关**：默认开启（零行为变化），`--no-stall-trace` 关闭。
- **验收** ✅：`stall_test.go`（三相位齐全 / t_s 单调 / 高于阈值段有值且非 emit 相位留空 /
  # tripped 行 / nil writer 不影响触发）+ smoke.sh 第 12 节（两场景相继熔断续跑、note 与
  stall_trace 双留痕、csv 行数与 t_s 单调、--no-stall-trace 不落盘）。

---

## 实施顺序

```
已完成：认证格式改造 → probe tool-call 检测 → trace 回放增强 → 硬编码其余项
        → 5.0–5.4 / 5.6（测量方法论第一批）→ 5.8 报告侧 → 5.9 配置联动+首轮取证 → 5.10 测试画像
        → 7 降速熔断 stall_guard → 8.3 降速采样序列落盘 → 熔断回归并入 smoke.sh
        → 5.7 闭环错峰发车（爬坡+fail-fast+双止损；续跑 P2 等 soak 需求）
        → 8.2 冷却 + 探针（min_tps 真机标定仍待做：配置/CLI 已就绪，上真机前先按稳态速率校准阈值）
剩余：  5.5 跨运行比对脚本（恒为脚本，暂缓——等真实比对需求再启动，2026-09-11 拍板）
        → 5.8 `slo:` 配置段 → 5.9 补样至 n≥20 后定起步值 → 6 Anthropic 兼容（触发后启动）
```
