# ROADMAP

> 状态：**草案（v3.3，2026-09-07 修订）** — 待确认后实施。
> 已实现能力见 [README](README.md)。
> 定位：本工具负责**发现**问题，修复由服务方完成后重测。核心场景是**基础性能 / 多轮 / 并发**。

---

## 后续要加的能力

| 优先级 | 能力 | 一句话说明 | 主要落点 |
|---|---|---|---|
| **P0** | 多轮回放保真度 | trace 回放时还原完整 role 序列（user / assistant / tool），否则回放上下文系统性偏小 | `internal/engine/trace.go` + `scenario.go` |
| **P0** | 认证格式可配置 | `Bearer ` 硬编码 3 处，裸 key / 自定义 header 环境直接 401 | `probe.go`、`client.go` |
| **P1** | tool-call 健康检查 | **已确认要做**：并入 `bench probe` 且**默认开启**，检出"引擎不支持/未启用 tool-call parser"，输出可行动的修复提示 | 新增 `engine/toolprobe.go` + `probe.go` |
| **P1** | probe 响应取证 | `--probe-capture <dir>` 落盘原始响应，既是给厂商的排障证据，也是判据的回归 fixture | `probe.go` |
| **P1** | 静默失败告警 | 两条指纹，识别"HTTP 200 但内容异常"的假成功 | `engine/sse.go` warnings |
| **P1** | 硬编码可配化 | 认证、接口路径、超时、引擎识别等写死，客户自研网关环境会直接失效或静默拿错数 | 见附录 C（32 处） |
| **P2** | 数据集导入工具 | 从 AgentLens 导出真实会话并转成 llm-perf trace 格式（P0 验收前置） | 新增 `scripts/` 或独立小工具 |

---

## 各项说明

### P0 · 多轮回放保真度

- **现状**：`trace.go` 只取 user 侧消息，真实 agent 会话里的 assistant 与 tool 消息（含大段工具结果）全部丢弃。
  测出的是小 context 下的性能，却标称"回放真实会话"，**长上下文衰减曲线与增量 prefill 速率的横轴失真**。
- **要做**：新增 `dataset.replay_mode: user_only | full`（默认 `user_only` 保持兼容）；
  `full` 模式按原序注入全部 role；`multiturn.max_reply_chars` 可配（默认保持 2000）。
- **注意**：`role: tool` 消息必须带 `tool_call_id`，缺失时跳过并计 warning，不可静默丢弃。

### P0 · 认证格式可配置

- **现状**：`Bearer ` 硬编码于 `probe.go:94`、`probe.go:166`、`client.go:346`。
- **要做**：配置加 `auth_scheme: bearer | raw | none`（默认 `bearer`），三处统一读。
- **为什么优先**：不修的话裸 key 客户环境连 `bench probe` 都跑不起来，卡住所有真机验证。

### P1 · tool-call 健康检查（落 `bench probe`）

- **定位**：只做**前置门禁**——告诉客户"这个引擎能不能正常调工具、不能的话该改哪里"，
  **不测性能、不进主压测路径**。符合本工具"只负责发现问题"的边界。
- **为什么放 probe 而不是新开子命令**：`ProbeCheck{Name,OK,Detail}` + ✅/❌ 渲染 + `Verdicts` 全部现成，
  新增检查项只是往 `res.Checks` 里追加；probe 已有"接新引擎先跑一遍"的使用心智。
- **触发**（已定）：`bench probe` **默认执行**，多 4 次请求、秒级；`--no-toolcall` 关闭。
- **依赖**：认证格式可配（P0）——裸 key 环境下 probe 跑不起来，此项无从验证。
- **输出要求**：失败时给**可行动结论**（直接贴给客户/厂商），见附录 A.3。
  成功时也要说明"本次压测不带 tools，不影响性能结论"，避免客户误读。
- **误报自证（默认开的前提）**：默认开意味着失败结论会常态化出现在客户面前，
  所以每条 ❌ **必须附可核实的原始证据**——请求体摘要 + 响应片段 + capture 落盘路径，
  不能只有一个 ❌ 加一句结论。否则一旦误报，无法自证，工具可信度直接受损。
- **自检提示（默认开的对价）**：每次输出 tool-call 检查结果时，附一条提示，**引导使用者自己跑一次对照实测**——
  把"检出能力未经验证"这件事如实告知使用者，由他在自己环境里确认，而不是默认由我们背书。
  文案与触发规则见 A.7。
- **验收**：原定需"一好一坏"两个端点，**坏引擎 case 已不可复现**（2026-09-07 确认）。
  改为 fixture 驱动的判据回归 + 真机不误报，见 A.6。**检出能力（sensitivity）暂未经验证**，
  首次在真实坏引擎上跑通后需回填文末占位。

### P1 · 静默失败告警

两条指纹加进 `closeOutWarnings`，**不解析 `tool_calls`**：

| 告警名 | 判据 |
|---|---|
| `empty_content_with_tokens` | `ContentChars == 0 && CompletionTokens > 0 && FinishReason == "stop"` |
| `toolcall_text_leak` | 未收到 tool_calls 时 content 命中 `DSML` / `tool▁calls` 标记 |

### P2 · 数据集导入工具

P0 的验收需要含 assistant / tool 消息的真实会话。来源走 **AgentLens OpenAPI**（见附录 B），
不扒本地 WorkBuddy 客户端数据（格式未公开、隐私风险高）。

---

## 明确不做

| 不做 | 原因 |
|---|---|
| tools schema 性能度量、报告「工具调用开销」章节 | 属"测工具调用"而非"测基础性能"，核心场景不带 tools |
| 在主压测路径解析 / 使用 `tool_calls` | 核心场景不带 tools；解析只为 probe 检查项服务，不进指标、不进原始数据 |
| 工具执行结果的正确性验证 | agent 框架的职责 |
| 自研 tool-call 解析器 | 本工具只探测与报告，修复在引擎侧 |
| 把健康检查塞进主压测路径 | 只做前置门禁，提示不阻断 |

---

## 实施顺序

```
认证格式修复（解开真机验证阻塞）
  → P1 tool-call 健康检查   ← 不依赖数据集，有"一好一坏"两个端点即可独立交付
  → 数据集导入工具（AgentLens → trace 文件）
  → P0 多轮回放保真度
  → 真机验收（同一份 trace，full vs user_only 对比 context 深度与 TTFT）
  → P1 静默失败告警
```

tool-call 检查放在认证之后，是因为它**不需要等数据集**——fixture 单测 + 一个好端点即可验收（A.6 的 L1/L2/L3），
能最早出成果；而 P0 回放必须等真实 agent trace 才能验收。

原则：**先跑通再写文档**。P0 必须经真实 agent trace 验证，否则是"改了看不出差别"的假改进。

---

## 开放问题

1. `full` 模式默认开关？建议默认 `user_only`，README 显著提示。
2. P1 告警是否并入现有 `correctness` 金丝雀？（该金丝雀**已实现**：`scenario.go:298` `runCorrectness`，
   数字转写校验，三个场景均调用，报告有 `CorrectnessRow`。此前记的"代码未实现"是误判，已更正）
3. AgentLens Token 是否覆盖 `codebuddy-model-request-prod` 空间？（需实测）
4. 附录 C 的 B 组（回放）与 C/D/E 组（路径 / 超时 / 引擎识别）是否合并成一次「客户环境适配」改造？
5. ~~tool-call 检查默认开还是显式开？~~ **已定：默认开**（`--no-toolcall` 关闭）。
6. ~~坏引擎 case 能否复现？~~ **已确认不可复现** → 验收改为 A.6 的 fixture 驱动方案。
7. ~~是否接受"检出能力未经验证"就默认开？~~ **已定：默认开**，三道对冲同时上：
   ① **误报自证**（❌ 必附原始证据 + capture 路径）；② **自检提示**（A.7，如实告知并引导使用者实测）；
   ③ **判据保守化**（只在命中明确特征时判 FAIL，可疑但不确定降级 WARN，见开放问题 8）。
8. **判据是否按硬度分级（硬特征 FAIL / 软特征 WARN）？** 与自检提示互补——提示解决"告知"，
   分级解决"同一条判据本身的误报概率"。具体分级见 A.8，待确认后落到 A.1 表。

---

## 附录 A · tool-call 健康检查设计（已确认实施，落 `bench probe`）

接入点已探明：`sse.go:20` 的 `knownDeltaKeys` 已含 `tool_calls` / `function_call`，
`ProbeCheck` 结构与 ✅/❌ 渲染现成可用，粗估约 200 行。落点见 A.5。

### A.1 检查项

| 项 | 请求 | 通过判据 |
|---|---|---|
| T1 基线连通 | 无 tools 普通对话 | HTTP 200 + content 非空；失败则跳过后续 |
| T2 非流式结构化 | `tools` + `tool_choice: auto` | `tool_calls` 为数组、`type=function`、name 在请求列表内、arguments 可 json 解析、`finish_reason=tool_calls` |
| T3 tool_choice 探针 | `tools` + `tool_choice: required` | 与 T2 联合判定（A.2） |
| T4 流式聚合 | `tools` + auto + stream | 按 `delta.tool_calls[i].index` 聚合，与非流式一致 |
| T5 标记泄漏 | 对 T2/T4 的 content 跑正则 | 命中 `DSML` / `tool▁calls` / `<tool_call>` 即报内容污染 |

T4 重点看**函数名是否丢失、调用数量是否少于非流式**——流式丢调用的签名。

### A.2 T2 × T3 联合判定

| T2 (auto) | T3 (required) | 结论 |
|---|---|---|
| 结构化 | 结构化 | 正常 |
| 失败 | 结构化 | parser 在，auto 分支未触发（配置问题） |
| 失败 | 失败 | parser 缺失 / 不支持该模型 → 升级引擎 |
| 失败 | 报错 | 引擎未实现 required → 升级引擎 |

### A.3 T2 失败细分（可直接贴客户 / 厂商）

| 失败特征 | 根因 | 提示 |
|---|---|---|
| content 含 DSML / `tool▁calls` | 未启用或不支持该模型的 parser | 查 `--tool-call-parser` |
| content 是纯 JSON 数组 | parser 缺失，required 强制路径生效 | 同上 |
| content 是自然语言「我来调用 xx」 | 模型未走序列化 | 查 chat template |
| 有 tool_calls 但 finish_reason=stop | parser 映射不完整 | 升级引擎 |
| 有 tool_calls 但 arguments 非法 JSON | 截断或增量拼接 bug | 查 max_tokens |
| 请求 4xx（invalid tools） | 网关未透传 tools | 查 router |

### A.4 实现注意

- 流式聚合四要点：按 index 分桶、name 只在首片、arguments 增量拼接、空 chunk 容错
- 检查对服务只读，对生产实例安全
- `--all-models` 显式开启多模型对照，默认关闭以保持 probe 的"快速探针"定位
- **capture 落盘必须脱敏**：客户环境响应体可能含业务数据。落盘前剔除 content 正文？——不行，
  正文正是判据依据。折中：默认落盘**完整内容**但明确提示"含业务数据、按需脱敏后再外发"，
  并在文件名带 endpoint 哈希与时间戳；`--probe-capture` 需显式指定目录才生效（不默认落盘）

### A.5 实施落点（2026-09-07 核准）

| # | 位置 | 改什么 |
|---|---|---|
| 1 | `sse.go:25` `deltaPayload` | 加 `ToolCalls []toolCallDelta`（OpenAI 协议：`index/id/type/function{name,arguments}`）。白名单里本来就有 `tool_calls`，补字段后即关闭附录 C #23（"知道字段却丢弃值、反而压制告警"） |
| 2 | `client.go:80` `TurnMetrics` | 加 `ToolCalls []ToolCall` + 分桶状态，标注 `json:"-"`——**不进压测原始数据**，不污染 JSON 契约 |
| 3 | `sse.go:140` `ingestEvent` | 按 `index` 分桶聚合（A.4 四要点） |
| 4 | `sse.go:275` `applyWholeBody` | 非流式同步解析。注意 `chunkChoice.Message` 同样用 `deltaPayload`，#1 加字段后自动生效 |
| 5 | 新增 `internal/engine/toolprobe.go` | 内置 `get_weather` 工具定义 + T2/T3/T4/T5 判据 + A.3 失败细分 → `Verdicts`（含 A.7 自检提示常量、A.8 判据等级） |
| 6 | `probe.go:149` `doChat` | 现在 messages 硬编码为「请原样回复：OK」（`probe.go:152`），需参数化以支持 tools / tool_choice / 自定义 messages |
| 7 | `probe.go:56` `ProbeOptions` | 加 `ToolCall bool` + `ToolsFile string`（客户自带工具集，可选，比内置单个工具更有说服力） |
| 8 | `cmd/bench/main.go:276` | 调用处传参 + `--no-toolcall` flag |
| 9 | 新增 `probe.go` capture | `--probe-capture <dir>`：把 tool-call 检查的**原始响应**（非流式整包 JSON + 流式 SSE 原始行）落盘。既是给厂商的排障证据，也是判据回归的 fixture 来源 |

T1 不新增检查项——复用现有 `chat_nonstream`（`probe.go:182`），它失败即跳过后续 tool-call 检查。

**验收**：见 A.6（坏引擎不可复现后的替代方案）。

### A.6 验收策略（坏引擎不可复现）

原计划依赖 SGLang + DeepSeek 那个 case 做"坏引擎对照"，2026-09-07 确认**无法复现**。
只在好引擎上跑通 = 检查项永远 PASS 的假安全，比不做更危险。故改为三层：

| 层 | 手段 | 验什么 | 依赖真引擎 |
|---|---|---|---|
| L1 判据正确性 | **fixture 回放单测**：手写/录制 A.3 六种失败模式的 SSE 与 JSON fixture，喂给解析与判定函数 | 每种失败特征确实命中对应分支 | 否 |
| L2 不误报 | 好引擎真机跑：T2/T3/T4 全绿、T5 无泄漏 | 不误报 | 是 |
| L3 不崩 | 找**确定不支持 function calling** 的端点（base 非 chat 模型、或小参数模型）：应得 4xx 或退化 content | 不 panic、不误判为正常 | 是（易得） |
| L4 检出能力 | 真实坏引擎 | **暂缺** | 是（已不可得） |

**L1 的 fixture 怎么来**：先手写（A.3 六种特征各一份，含 DSML 标记泄漏、纯 JSON 数组、
自然语言长答、finish_reason 错位、arguments 非法 JSON、4xx）；
今后任何客户环境跑到真坏引擎，`--probe-capture` 落盘即可沉淀为新增 fixture。

**这里不是"用 mock 自欺"**：mock 不可接受的场景是**性能测量与交付结论**——那必须用真实负载。
而判据函数是纯确定性逻辑（给定字节流 → 给定判定），用 fixture 验证它恰恰比真机更严格，
真机还受引擎版本、随机性干扰。两者分工：L1 保证判据逻辑对，L2/L3 保证真实环境不误报。

**遗留风险（明确记录）**：L4 缺失 ⇒ **sensitivity 未经验证**。
首次在真实坏引擎上跑通后，回填：
① 实际命中的 A.3 分支；② 是否有未预料的第四种失败形态；③ 判据是否需按新形态增补。

### A.7 自检提示（引导使用者实测）

**背景**：L4 缺失意味着我们只能证明"好引擎上不乱报"，证明不了"坏引擎上真会报"。
默认开之后，与其由我们默默背书，不如**把这件事如实告知使用者，请他在自己环境里验一次**。

**文案**（输出在 tool-call 检查结果块之后、`Verdicts` 之前）：

```
ℹ️  tool-call 检查自检提示
    本项的"检出能力"尚未在真实故障引擎上验证——目前只验证过"正常引擎不误报"。
    建议你在自己环境里做一次对照实测：
      1. 找一个确定不支持 function calling 的端点（如 base 非 chat 模型）
      2. bench probe --endpoint <该端点> --probe-capture ./capture
      3. 确认本项确实报出 ❌；并把 ./capture 下的原始响应回传，用于补充 fixture
    手上若有已知有问题的引擎，同样请按上面命令跑一次并回传，这是本项唯一的验证来源。
```

**触发规则**：

- tool-call 检查执行即输出，`--no-toolcall` 时不输出
- **不因结果全绿而省略**——全绿恰恰是最需要提醒的场景（客户会把 ✅ 读成"已确认正常"）
- 用 ℹ️ 而非 ⚠️ / ❌：它是提示，不是结论，**不影响退出码、不计入检查项**
- JSON 报告的 probe 段同步输出 `selfcheck_notice` 字符串字段（便于程序化比对与后续摘除）

**实现要点**：

- 文案收敛为 `toolprobe.go` 里的单一常量 `toolCallSelfCheckNotice`。
  L4 验证回填后**删掉这个常量即可摘除**，不散落在多处
- 提示里必须带 `--probe-capture` 用法——使用者回传的落盘文件就是 L1 的 fixture 来源，
  等于把"每一次客户实测"变成资产

**摘除条件**：A.6 的 L4 跑通、文末占位回填完成后，删除本提示与对应常量。

### A.8 判据分级（待确认，与 A.7 互补）

自检提示解决"告知使用者"，分级解决"单条判据自身的误报概率"。按硬度分开定级，
不用一刀切的"先 WARN 后 FAIL"——那会让硬特征白白损失价值。

| 判据 | 硬度理由 | 等级 |
|---|---|---|
| content 命中 `DSML` / `tool▁calls` 控制标记 | 正常回答里几乎不可能出现 U+2581 私有标记 | **FAIL** |
| content 是纯 JSON 数组 | 同上，正常对话不会吐裸 JSON 数组 | **FAIL** |
| 有 `tool_calls` 但 arguments 非法 JSON | 结构确定，无歧义 | **FAIL** |
| 有 `tool_calls` 但 `finish_reason=stop` | 偏硬，但个别引擎确实这么实现 | **WARN** |
| content 是自然语言「我来调用 xx」 | 软，模型正常闲聊也可能这么写 | **WARN** |
| `tool_choice: required` 报 4xx | 软——可能是网关不透传，未必是引擎问题 | **WARN** |
| 检查自身失败（超时 / 网关错 / 解析异常） | 与引擎能力无关 | **单独输出「检查未完成」**，不计 ❌ |

最后一条在默认开之后会经常撞上（客户网关千奇百怪），
必须与"检查未通过"区分——否则客户会拿着我们自己的超时当引擎缺陷去提工单。

---

## 附录 B · AgentLens 取数要点（P2 依据）

- 域名 `openapi.agentlens.woa.com`，全部 `POST` + `application/json`
- 鉴权：`X-Agentlens-Token`（自定义 header，非 Bearer），Token 在 agentlens.woa.com 页面右上角取
- 调用链：`space/list` → `traceList`（按 `session_id` 聚合）→ `traceDetail` → `spanDetail`
- 自带 perf 相关字段：`inputTokens` / `outputTokens` / `cacheReadTokens` / `cacheHitRate` / `duration`(ns)

**两个坑**：

1. `traceList` 的 `input`/`output` 是首尾 IO 摘要，很可能截断，完整 messages 要走 `spanDetail`；
   prompt 落在哪个 attribute key 下**需实测一次确认**后再写转换脚本。
2. 同一 session 下各次调用的 prompt 是**包含关系**（第 N 次含前 N-1 次全部 history），**不能拼接**。
   正确取法：取最后一次调用的完整 messages，或取相邻两次的差分（每轮增量）。

**代表性风险**：CodeBuddy 是 coding agent（长代码上下文、文件读写工具），客户多为金融场景（短问答、检索工具）。
可用来验证功能可行性，但其数值不能直接作为客户交付结论。

---

## 附录 C · 硬编码改造清单（2026-09-07 全量扫描）

分级：**必修** = 客户环境会直接失效或静默出错；**建议** = 影响准确度但不阻塞；**可不动** = 仅影响展示或已有合理默认。

### C.1 认证格式（必修）

| # | 位置 | 硬编码 | 影响 |
|---|---|---|---|
| 1 | `probe.go:94` | `Authorization: Bearer <key>` | GET /models 401 |
| 2 | `probe.go:166` | 同上 | probe chat 401 |
| 3 | `client.go:346` | 同上 | 压测主路径 401 |
| 4 | `config.go:204-205` | 只有 `api_key` / `api_key_env`，无 header 名配置位 | 自定义 header（如 `X-API-Key`）无法表达 |

方案：`auth_scheme: bearer | raw | none` + `auth_header`（自定义 header 名），默认 `bearer` 兼容。
（AgentLens 自家 OpenAPI 用 `X-Agentlens-Token`，说明自定义 header 不是过度设计。）

### C.2 多轮回放（必修，P0 同源）

| # | 位置 | 硬编码 | 影响 |
|---|---|---|---|
| 5 | `trace.go:132` `isUser()` | 只认 `human` / `user` | assistant / tool / system 全丢弃 |
| 6 | `trace.go:160-163` | 只收 `isUser()` 的 turn | 同上 |
| 7 | `trace.go:171-201` `sessionsFile` | `Turns []string` 无 role 位 | 数据格式层面就承载不了 tool 消息 |
| 8 | `scenario.go:503` | `reply[:2000]` | 截断；且**按字节切**，中文会切出半个 UTF-8 字符（`filler.go:42` 自己都注明要按 rune 截） |
| 9 | `scenario.go:595` | 同上（并发路径重复一份） | 同上 |
| 10 | `scenario.go:319-321` | `reply[:200]`（correctness 展示） | 仅影响报告展示 |

### C.3 接口路径（建议）

| # | 位置 | 硬编码 | 影响 |
|---|---|---|---|
| 11 | `client.go:340` | `BaseURL + "/chat/completions"` | 客户 router 路径不等于 `/v1/chat/completions` 即 404 |
| 12 | `probe.go:163` | 同上 | 同上 |
| 13 | `smetrics.go:56` | `TrimSuffix("/v1") + "/metrics"` | metrics 不在根路径（如 `/actuator/prometheus`）抓不到，服务端指标章节静默为空 |

### C.4 超时与重试（建议）

| # | 位置 | 硬编码 | 影响 |
|---|---|---|---|
| 14 | `probe.go:96` | 15s（/models） | 慢网关误判为不可达 |
| 15 | `probe.go:168` | 120s（probe chat） | 配置的 `timeout_seconds` **完全不作用于 probe** |
| 16 | `smetrics.go:59` | 5s（scrape） | 高负载时抓取超时 |
| 17 | `client.go:66-68` | IdleConn 90s / TLS 10s / ExpectContinue 1s | 一般无需动 |
| 18 | `client.go:282-286` | 退避 300ms 起、5s 封顶 | 一般无需动 |

### C.5 引擎识别与指标名（建议，自研引擎会静默拿错数）

| # | 位置 | 硬编码 | 影响 |
|---|---|---|---|
| 19 | `probe.go:409-425` `guessEngine` | 按 Server 响应头子串匹配 | 客户自研 router 不吐 Server 头 → 识别不出 → 交叉验证命令不生成 |
| 20 | `smetrics.go:249-277` | vLLM 指标名硬编码表 | 引擎版本改名即失效 |
| 21 | `smetrics.go:290-298` | SGLang 仅实现 gauge，counter / hist 为空 | SGLang 缓存命中率不出数 |
| 22 | `smetrics.go:302-307` `DetectProvider` | 无法识别时**回落 vLLM** | 自研引擎指标要么全丢要么套错命名，**且无任何告警** |

### C.6 协议字段白名单（建议，与 P1 告警联动）

| # | 位置 | 硬编码 | 影响 |
|---|---|---|---|
| 23 | `sse.go:20-23` `knownDeltaKeys` | 含 `tool_calls` / `function_call`，但 `deltaPayload` 无对应字段 | 值被丢弃，且**压制 `unknown_delta_fields` 告警** → 随 P1 tool-call 检查项一并修（附录 A.5 #1） |
| 24 | `sse.go:30-31` | 思考字段只认 `reasoning` / `reasoning_content` | 第三方命名（如 `thinking_state`）被记 unknown |
| 25 | `sse.go:205-209` | 两代命名硬编码 | 同上 |

### C.7 常量（多数可不动）

| # | 位置 | 硬编码 | 影响 |
|---|---|---|---|
| 26 | `corpus.go:27-32` | `zh=1.4`、其他 `4.0` 字符/token | 日韩、代码场景 token 估算偏差大 |
| 27 | `probe.go:152` | 探测 prompt「请原样回复：OK」 | 中文 prompt，模板异常时误报 |
| 28 | `probe.go:381-385` | 交叉验证默认 `1024/256/num-prompts 64/rate 4` | 与客户实际压测参数不符（已有 `XV*` 字段，未全量透传） |
| 29 | `scenario.go:224` | warmup `MaxTokens: 1` | 无影响 |
| 30 | `client.go:169` | `PreviewHeadTail` 120 / 280 | 仅日志展示 |
| 31 | `client.go:452` | 错误摘要只保留前 4 条消息 | 多轮长 history 排障时看不到实际内容 |
| 32 | `client.go:173` / `trace.go:100` | 响应体 2MB / trace 文件 512MB 上限 | 超大响应或大文件被截断 |

### C.8 优先级

```
必修：#1-4（认证）→ #5-9（回放）        ← 直接阻塞真机 / 直接导致数据失真
建议：#11-13（路径）#14-16（超时）#19-22（引擎识别）#23（tool_calls 白名单）
可不动：#10 #17-18 #24-32（观察真机反馈再定）
```

---

## 占位（待真机数据回填）

- **A.6 的 L4**：真实坏引擎上首次运行的结果（命中分支 / 未预料形态 / 判据增补）——tool-call 检查 sensitivity 的唯一验证机会
- **A.7 自检提示的回执**：使用者按提示做的对照实测结果（报出 / 未报出 + `--probe-capture` 落盘文件）；
  累计到若干份后若均未报出，需重新审视判据而非常驻提示
- `spanDetail` attributes 中 prompt 内容的实际 key 名
- `full` 模式下的 `tool_call_id` 缺失率基线
- 脱敏规则（剔除凭证、手机号、邮箱、客户名、内部代码）
- 各引擎 × tool-call parser 支持对照表（vLLM / SGLang / MindIE / TGI / llama.cpp）
