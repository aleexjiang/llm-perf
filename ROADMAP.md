# ROADMAP

> 三件事：① probe 阶段加 tool-call 检测；② trace 回放增强；③ 硬编码改造。
> 定位不变：本工具负责**发现**问题，修复由服务方完成后重测。

---

## 1. probe 增加 tool-call 检测（默认开启，`--no-toolcall` 关闭）

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

## 2. trace 回放增强（P0：保真度）

- **现状**：`trace.go` 只取 user 侧消息，真实会话的 assistant / tool 消息（含大段工具结果）全丢，
  测的是小 context 却标称"回放真实会话"，长上下文衰减曲线横轴失真。
- **要做**：`dataset.replay_mode: user_only | full`（默认 `user_only` 保持兼容）；
  `full` 按原序注入全部 role；`multiturn.max_reply_chars` 可配（默认 2000）。
- **注意**：`role: tool` 消息须带 `tool_call_id`，缺失跳过并计 warning，不静默丢弃；
  `reply[:2000]` 现在按字节切，一并改为按 rune 截（中文切半个字符）。
- **验收**：真实 agent trace 上 full vs user_only 对比 context 深度与 TTFT（数据来源见第 4 节）。

---

## 3. 硬编码改造（一次做完"客户环境适配"）

全量清单曾扫描出 32 处（见 git 历史），必修的就两组，其余一次改造顺带处理：

**必修（阻塞真机 / 数据失真）**：
- 认证：`Bearer ` 硬编码 3 处（`probe.go:94/166`、`client.go:346`）→ `auth_scheme: bearer | raw | none` + `auth_header`（自定义 header 名），默认 `bearer` 兼容
- 回放：见第 2 节（同一处改造）

**建议（一次改造顺带）**：
- 接口路径：`/chat/completions`、`/metrics` 可配
- probe 超时：`probe.go:96` 15s、`probe.go:168` 120s → 读配置 `timeout_seconds`
- 引擎识别：`DetectProvider` 无法识别时不回落 vLLM，改为显式提示"未知引擎，服务端指标跳过"；Server 头匹配不到不生成交叉验证命令时打一行日志
- SSE 字段白名单：`deltaPayload` 补 `tool_calls` 值（随第 1 节一并修）；思考字段命名放开为可配置列表

其余 20 余处（常量、预览截断、文件大小上限等）**不动**，真机反馈有问题再说。

---

## 4. 数据来源（支撑第 2 节验收，做的时候再说）

真实 agent 会话从 AgentLens OpenAPI 导出（`openapi.agentlens.woa.com`，`X-Agentlens-Token` 鉴权，
调用链 space/list → traceList → traceDetail → spanDetail），转成 trace 格式。
两个已知坑：完整 messages 要走 spanDetail（traceList 的 IO 摘要会截断）；
同一 session 各次调用 prompt 是包含关系，取最后一次或做差分，不能拼接。

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

**5.5 跨运行复现性比对**（待做，恒为脚本不进 CLI）

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

**5.7 负载保真度：闭环错峰发车与会话续跑**（待做）

- 闭环 multiturn 指数爬坡发车（内置机制，默认开，`concurrent.ramp: false` 可关）：首批发 1 个会话，
  等该批**全部完成首轮**后放下一批 `min(上批×factor, 剩余)`，`concurrent.ramp_factor` 默认 2（可设 3）；
  最后一批放剩余量。批次节奏由服务端首轮实际耗时决定（自适应，无需按端点调参），在途会话深度混合自然涌现。
- 失败语义：**任一批次首轮失败 → 终止战役**（fail-fast，首轮挂大概率模型服务有问题）；
  中间轮失败会话继续，止损双保险——会话内连续失败 3 轮提前终止该会话，全局连续失败 ≥ 2×level 终止战役
  （默认常量，必要时再配置化）。
- `MultiturnRun` 落盘启动偏移与批次号，报告侧可标爬坡窗口。
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

**5.9 负载保真度：multiturn 起步上下文对齐 agent 真实形状**（配置联动已实现，2026-09-08；AgentLens 取证待做）

- 问题：filler 模式 turn1 ≈ 15.5k（system 基座 `system_tokens 1000` + `tool_defs_tokens 2000`，×1.07 模板开销），
  远低于真实 agent 产品的首调上下文规模 → multiturn turn1 测不出"大 prefill 冷启动"，而首字延迟恰是 agent 产品的真实痛点。
- 目标：turn1 起步 ≈ **35k**。**假设值，待取证**：与 5.8 档3"现代 agent 产品基线上下文 ~35K"同一来源，均为推导值。
- 取证（待做）：AgentLens `codebuddy-model-request-prod` 空间按 `session_id` 聚合，取每个 session **第一次** LLM 调用的
  `inputTokens` 分布（P50/P90）回填基线。session 内 prompt 是包含关系 → 只取首调，不差分、不拼接（第 4 节坑 2）。
  需内网环境 + `X-Agentlens-Token`，本机不可达，取证后如 P50 偏离 35k 再回填调整。
- 配置联动 ✅（无需改代码，按预期）：`example.yaml` 与 `customer.yaml` multiturn 已改为
  `system_tokens: 18000` + `tool_defs_tokens: 3000` + `turn_tokens: 10000`、`turns: 8`（turn1 ≈ 32.5k，末轮 ≈108k）。
  qwen3.8-27b.yaml（256k 模型自算预算）与 smoke 系列保持不变。
- 末端约束（2026-09-08 定，方案 B）：`turn_tokens` 由 12300 降到 **10000**、保持 `turns: 8`
  → 末端 prompt ≈108k（thinking on 含 8k 输出 ≈116k），128k 模型留 ~12k 余量；probe 确认各模型上限后再微调。
  代价：逐轮增量变小（模拟"每轮新增工具结果 + 追问"，10k 仍不失真）。✅ 口径已注明：multiturn 报告 Note
  追加"上下文 ~Xk 起步 → 末轮 ~Yk（每轮增量 Ntk：模拟每轮新增工具结果+追问）"，防止被当配置失误。
- 可选（不占决策位）：借 schema 不保兼容窗口把 `system_tokens` 语义拆为 `base_context_tokens`（system + tools + memory 三段）。
- 连带：`config.go:851` 深上下文告警（base + turns×turn_tokens ≥ 100k）改后必触发，属预期 ✅；
  probe `LargestPromptTokens`（`config.go:1134`）随配置自动跟上，无需改动 ✅。

**5.10 启动时打印战役画像（campaign profile）**【已实现，2026-09-08】

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
  `gen_html_report.py` 第 2 节渲染"战役画像"表（与 5.4 配置原文存档合并消费）。E2E（mock 两模型四场景）：
  打印估算 36 请求 = 实际执行量，HTML 画像表正常渲染。
- 连带修复：`gen_html_report.py` gen_conclusions 的 decode 吞吐对比在 TPS=0（mock/异常数据）时除零崩溃，加 >0 守卫。

**批次建议**：先做 5.2 + 5.3 + 5.4（半天、零风险、不动 Go 主流程）→ 5.1（防方向性错误）→ 5.6（等场景）。
（2026-09-08 更新：5.0–5.4 及 5.6 已全部实现；5.8 报告侧 + 5.9 配置联动 + 5.10 已实现——
剩余：5.5（恒为脚本不进 CLI）、5.7 闭环错峰发车（续跑 P2 等 soak 需求）、5.8 `slo:` 配置段、
5.9 AgentLens 取证（内网 + token，本机不可达）。）

---

## 实施顺序

```
1. 认证格式改造        ← 解开裸 key 环境的验证阻塞
2. probe tool-call 检测 ← 不依赖数据集，fixture 单测 + 好端点即可交付
3. trace 回放增强       ← 依赖真实 agent trace 验收
4. 硬编码其余项（路径/超时/引擎识别）与回放改造合并一次提交
5. 测量方法论 5.2/5.3/5.4 → 5.1 → 5.6 → 5.8（报告侧+配置段）→ 5.9（配置✅，取证待内网）→ 5.10 ✅；剩 5.5 / 5.7
```
