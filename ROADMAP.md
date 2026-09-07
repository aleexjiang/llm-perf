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

## 实施顺序

```
1. 认证格式改造        ← 解开裸 key 环境的验证阻塞
2. probe tool-call 检测 ← 不依赖数据集，fixture 单测 + 好端点即可交付
3. trace 回放增强       ← 依赖真实 agent trace 验收
4. 硬编码其余项（路径/超时/引擎识别）与回放改造合并一次提交
```
