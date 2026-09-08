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

**批次建议**：先做 5.2 + 5.3 + 5.4（半天、零风险、不动 Go 主流程）→ 5.1（防方向性错误）→ 5.6（等场景）。
（2026-09-08 更新：5.0–5.4 及 5.6 已全部实现；仅 5.5 待做，恒为脚本不进 CLI。）

---

## 实施顺序

```
1. 认证格式改造        ← 解开裸 key 环境的验证阻塞
2. probe tool-call 检测 ← 不依赖数据集，fixture 单测 + 好端点即可交付
3. trace 回放增强       ← 依赖真实 agent trace 验收
4. 硬编码其余项（路径/超时/引擎识别）与回放改造合并一次提交
5. 测量方法论 5.2/5.3/5.4 → 5.1 → 5.6
```
