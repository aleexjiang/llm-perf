# trace-real-16：真实 agent 会话样本（脱敏）

16 条真实 LLM 调用链路，覆盖 **单轮总 token 量 40k–500k** 四档，用于 `dataset.mode: trace` 回放。

- **文件**：`trace-real-16.json.gz`（gzip，解压后约 6.3 MB，JSON / sharegpt 格式）
- **格式识别**：`sharegpt`（`conversations[].from` = `system` / `user` / `assistant` / `tool`，tool 消息带 `tool_call_id`）
- **来源**：内部 LLM 可观测平台（OTel 链路），2026-09-03 ~ 2026-09-10 的真实调用，**已脱敏**
- **入口分布**：WorkBuddy 6 / CLI 6 / app_cloud 2 / IDE 2；模型覆盖 hy3、hy4-preview、glm-5.3-flash、deepseek-v4-pro、gpt-5.6-terra、gpt-5.6-luna
- **采样口径**：每个会话取该链路中 **prompt 最大的那次 LLM 调用** 的完整 messages（= 该会话的上下文峰值快照），并额外把该次请求的工具定义拼成一条 `system` 消息（`## Available Tools`），以保留真实 prefill 体积与前缀结构。

## 逐会话清单

| # | 档位 | 入口 | 模型 | trace 总量 | span 数 | 本次 prompt | user 轮 | sys/user/asst/tool¹ | prompt 字符 | 估算 tokens | 缓存读 | TTFT |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 40-80k | WorkBuddy | hy3 | 75,111 | 1 | 75111 | 5 | 2/5/25/21 | 210,478 | ~52,619 | 7,488 | 8.8s |
| 2 | 40-80k | CLI | gpt-5.6-terra | 73,409 | 4 | 20240 | 2 | 2/2/3/5 | 87,269 | ~21,817 | 20,076 | 5.5s |
| 3 | 40-80k | WorkBuddy | hy4-preview | 70,773 | 1 | 70773 | 3 | 2/3/11/9 | 217,043 | ~54,260 | 26,176 | 3.3s |
| 4 | 40-80k | WorkBuddy | glm-5.3-flash | 65,400 | 1 | 65400 | 3 | 2/3/3/1 | 199,625 | ~49,906 | 64,064 | 2.8s |
| 5 | 80-150k | CodeBuddyIDE | hy3 | 144,630 | 3 | 49277 | 3 | 2/3/32/32 | 145,711 | ~36,427 | 48,000 | 1.8s |
| 6 | 80-150k | WorkBuddy | deepseek-v4-pro | 144,408 | 2 | 73949 | 5 | 2/5/16/12 | 219,647 | ~54,911 | 70,400 | 2.0s |
| 7 | 80-150k | CLI | gpt-5.6-luna | 143,594 | 1 | 143594 | 32 | 2/32/35/60 | 447,332 | ~111,833 | 140,863 | 5.9s |
| 8 | 80-150k | CLI | gpt-5.6-terra | 141,999 | 6 | 46280 | 2 | 2/2/5/12 | 176,757 | ~44,189 | 24,428 | 7.2s |
| 9 | 150-250k | CLI | gpt-5.6-luna | 243,275 | 1 | 243275 | 25 | 2/25/86/170 | 942,277 | ~235,569 | 0 | 9.6s |
| 10 | 150-250k | WorkBuddy | hy3 | 237,126 | 2 | 122174 | 2 | 2/2/20/20 | 230,257 | ~57,564 | 114,944 | 3.2s |
| 11 | 150-250k | WorkBuddy | hy4-preview | 243,014 | 6 | 57243 | 2 | 2/2/5/5 | 150,492 | ~37,623 | 44,352 | 2.5s |
| 12 | 150-250k | WorkBuddy | glm-5.3-flash | 241,327 | 3 | 80986 | 5 | 2/5/14/10 | 232,290 | ~58,072 | 80,640 | 2.5s |
| 13 | 250-500k | app_cloud | hy4-preview | 495,380 | 4 | 126775 | 7 | 2/7/45/40 | 268,397 | ~67,099 | 126,464 | 2.8s |
| 14 | 250-500k | CLI | gpt-5.6-terra | 467,147 | 10 | 64677 | 2 | 2/2/9/19 | 235,585 | ~58,896 | 63,641 | 11.4s |
| 15 | 250-500k | CLI | gpt-5.6-luna | 480,489 | 1 | 480489 | 30 | 2/30/312/389 | 1,538,474 | ~384,618 | 479,353 | 13.5s |
| 16 | 250-500k | CLI | gpt-5.6-terra | 263,429 | 8 | 61458 | 2 | 2/2/7/24 | 249,081 | ~62,270 | 48,896 | 5.9s |

¹ `sys` 含 1 条原始 system + 1 条工具定义 system；`tool` 为工具结果消息数。

> **两列 token 的区别**：`trace 总量` 是整条链路（一次用户交互，可能含 N 次 LLM 调用）的 input 累加；`本次 prompt` 是选中那次调用的实测 input tokens（服务端 usage）。`估算 tokens` 是本文件字符数 /4，只用于粗略分档——中英混排下真实 tokenizer 结果通常更高。

## 覆盖到的形状特征

- **多轮深度**：user 轮 2–32，消息数 8–733（含大量 assistant/tool 交替）
- **工具密集**：tool 消息占比 12%–56%，tool 定义 6–42 个/会话
- **前缀缓存**：缓存读占比 0%–99.8%（#9 为 0，是唯一完全冷 prefill 的样本，适合做冷启动基线）
- **TTFT 跨度**：1.8s–13.5s，与缓存命中强相关（prompt 越大不一定越慢）

## 用法

```yaml
dataset:
  mode: trace
  path: configs/fixtures/trace-real-16.json.gz   # 相对配置目录解析；支持 .json / .json.gz
  format: sharegpt        # 可省略（自动识别）
  replay_mode: full       # 注入 system/user/assistant/tool 原序；user_only 只回灌 user 轮
  min_turns: 2            # 默认 2；本样本全部 >=2 轮，不会被过滤
  max_sessions: 8         # 按需裁剪；0 = 不限
```

已验证的加载结果（`LoadTrace` 实跑）：

```
[user_only] 格式=sharegpt 会话=16 FullReplay=false  NoAssistantContent=false 缺tool_call_id=0
[full     ] 格式=sharegpt 会话=16 FullReplay=true   NoAssistantContent=false 缺tool_call_id=0
```

与 `trace-sessions.json`（只有 user 轮的精简格式）的区别：本样本 **含 assistant/tool 内容**，`replay_mode: full` 能真正生效，不会退化成 user_only 的深度。

## 脱敏规则

结构化模式 → 占位符（同一 ID 确定性映射，保持 `tool_call_id` 对应关系）：

| 类别 | 处理 |
|---|---|
| 用户/主机路径 | `/Users/<user>/<workspace>/…`，深层子路径与含中文的段统一 `<dir>`（保留扩展名） |
| 内网域名 | `<host>` |
| 邮箱 / IP / 手机号 | `<email>` / `<ip>` / `<phone>` |
| trace / span / call ID、32 位 hex、UUID | `<uuid_xxxxxx>` / `<id_xxxxxx>` / `<call_xxxxxx>` |
| 密钥（`sk-`/`ghp_`/`Bearer`/`api_key=` 等） | `<secret>` |
| 微信 ID | `<wxid>` |
| 客户机构名、人名、项目代号、内部平台名 | `某某机构` / `某某` / `某项目` / `某平台` |

工具定义同样走脱敏（原始 skill 清单里含绝对路径与内网域名，是残留风险最高的地方）。

**残留说明**：文中仍有 `/home/me`、`/Users/Administrator` 一类路径，来自工具文档的示例文案，非真实数据。

## 注意事项

- 体积：单条会话 prompt 20k–480k tokens，**按被测模型上下文上限挑选档位**（`max_sessions` 或自行切片），128k 上限的模型不要直接跑 #9/#15。
- 回放顺序：文件按 token 档位升序排列（1–4 / 5–8 / 9–12 / 13–16），`max_sessions: 4` 拿到的是最小的四档。
- 不可复现对比：真实数据的价值在形状，不在绝对值；跨模型横向对比仍应以 filler 受控档位为主。
- 本文件只增不改：后续补充样本请追加到文件末尾，保持既有编号稳定。

生成于 2026-09-10 · 字符总量 5,550,715 · 会话 16 条