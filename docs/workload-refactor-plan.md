# 压测场景与负载模型改造计划

> 状态：讨论稿，尚未实施
>
> 日期：2026-09-18
>
> 目标：去掉正式压测中的 filler 负载，将用户会话、RPS 到达和原生并发对比拆成清晰、互不混淆的场景。

## 1. 背景与问题

当前实现把多个概念混在了一起：

- `dataset.mode` 同时承载 filler 和 trace 两种数据来源；
- `concurrent` 场景同时承载多轮会话、RPS 和固定并发；
- `multiturn`、`turn_tokens`、`system_tokens`、`tool_defs_tokens`、`profiles` 共同决定 filler 会话形状；
- `ConcurrentLevel` 同时包含 `Requests` 和 `Sessions`；
- `--concurrency cfg` 既可能执行开环 RPS，也可能执行闭环 levels；
- tool-call 属于能力检查，却与压测入口存在概念关联。

其中 filler 每轮按固定 `turn_tokens` 注入大段文本，例如默认配置每轮新增约 `10000` token。这适合做受控上下文增长实验，但不代表线上真实用户输入、assistant 回复、工具结果和会话轮次分布，容易导致结果被误读为真实线上容量。

## 2. 改造原则

1. **正式压测只使用 trace 或统一请求集，不再使用 filler。**
2. **数据来源、业务形态、负载模型三者分离。**
3. **用户会话模式与原生请求压测模式分离。**
4. **并发模式对齐 vLLM `bench serve`，用于原生压测横向对比。**
5. **RPS 的参数语义与 vLLM 保持一致：请求到达率、总请求数、最大并发、突发度。**
6. **`probe` 只做能力和健康检查，不进入 benchmark 数据。**
7. **保留完整原始请求指标，聚合和结论继续外移。**
8. **同一模式内部只保留一种明确的样本组织方式：用户模式以 sessions 为主，RPS/并发模式以 requests 为主。**

## 3. 目标场景

### 3.1 `probe`：能力与健康探针

入口：

```bash
bench probe -c customer.yaml
```

职责：

- chat、stream、usage、thinking 能力检查；
- tool-call 健康检查；
- `/models`、`/metrics`、context limit 探测；
- `decode_speed`、filler fidelity 等部署健康参考。

tool-call 只属于 `probe`，不进入用户、RPS、并发压测路径。

### 3.2 `user`：真实用户会话

入口示例：

```bash
bench user -c customer.yaml --users 1
bench user -c customer.yaml --users 8
```

语义：

- `--users 1`：单用户顺序执行完整 trace 会话；
- `--users N`：N 个独立用户并行执行各自 trace 会话；
- 每个用户保留自己的 history；
- `assistant`、`tool` 消息按 `replay_mode: full` 进入后续上下文；
- 重点观察真实用户会话中的 TTFT、decode、失败、取消和上下文增长；
- 不以 RPS 或固定并发容量对齐为主要目标。

数据结构以 `sessions[]` 为主。

### 3.3 `rps`：开环请求到达

入口示例：

```bash
bench rps -c customer.yaml \
  --request-rate 1,2,4,8 \
  --num-prompts 1000 \
  --max-concurrency 64
```

语义对齐 vLLM `bench serve`：

- `request_rate`：请求发起速率，单位 req/s；
- `num_prompts`：本轮总请求数；
- `max_concurrency`：同时允许执行的最大请求数；
- `burstiness: 1`：Poisson 到达；
- `burstiness < 1`：更突发；
- `burstiness > 1`：更均匀；
- 到达率由调度器控制，服务端排队和执行速度可能使实际完成速率低于目标到达率。

RPS 模式默认以**独立请求**为单位，输出数据以 `requests[]` 为主。它不自动把每个请求串成多轮用户会话；需要真实多轮会话时使用 `user` 模式。

### 3.4 `concurrency`：原生压测对比

入口示例：

```bash
bench concurrency -c customer.yaml \
  --max-concurrency 1,2,4,8,16 \
  --num-prompts 1000
```

或使用单档：

```bash
bench concurrency -c customer.yaml \
  --request-rate inf \
  --max-concurrency 8 \
  --num-prompts 1000
```

目标：与 vLLM `bench serve` 的原生压测方式做尽量同口径对比。

语义：

- 每个请求独立，不运行多轮 session；
- 不使用 `turn_tokens`、`system_tokens`、`tool_defs_tokens`；
- 不使用当前多轮 session barrier；
- 不使用 `profiles`/SWRR 混合用户档位；
- `request_rate=inf` 表示尽快发起，`max_concurrency` 控制在途请求上限；
- 有限 `request_rate` 时，同时具备到达节奏和并发上限；
- 输出数据以 `requests[]` 为主。

## 4. 数据来源设计

### 4.1 正式压测数据

正式压测统一使用脱敏 trace 或由 trace 提取的请求集：

```yaml
trace:
  path: "/data/customer-sessions.jsonl.gz"
  format: "sharegpt"
  replay_mode: "full"
```

用户会话模式使用完整 session；RPS 和 concurrency 模式使用独立 request sample。

### 4.2 统一请求集

为了与 vLLM 原生压测做可复核对比，建议增加统一请求集格式：

```json
{
  "id": "req-0001",
  "model": "model-a",
  "messages": [
    {"role": "user", "content": "..."}
  ],
  "expected_output_tokens": 256
}
```

同一份请求集应能够：

- 被 llm-perf 的 `rps`/`concurrency` 消费；
- 转换成 vLLM `bench serve --dataset-name custom` 可消费的格式；
- 记录输入消息、目标输出长度和样本顺序；
- 在需要时关闭 shuffle，保证顺序可复现。

如果两边一边使用 customer trace，另一边使用 vLLM `random` 数据集，只能比较趋势，不能宣称为严格数字对比。

### 4.3 filler 的处理

正式 benchmark 删除 filler 数据源和 filler 配置。

保留最小的合成请求能力，仅用于：

- Go 单元测试；
- mock server；
- smoke 测试；
- probe 的最小连通性请求。

不再把 filler 作为客户环境正式压测数据源，也不再把 `turn_tokens` 解释为线上真实每轮增量。

## 5. 配置目标形态

建议把数据来源与负载模型分开：

```yaml
source:
  type: trace
  path: "/data/customer-sessions.jsonl.gz"
  format: sharegpt
  replay_mode: full

scenario:
  mode: user
  users: 1
```

RPS：

```yaml
scenario:
  mode: rps
  request_rate: 4
  num_prompts: 1000
  max_concurrency: 64
  burstiness: 1
```

固定并发：

```yaml
scenario:
  mode: concurrency
  request_rate: inf
  max_concurrency: 8
  num_prompts: 1000
```

最终字段命名仍需讨论。当前 `dataset`、`concurrent`、`multiturn` 配置是否整体迁移，暂不在本稿中定死。

## 6. CLI 目标形态

建议从当前组合式入口：

```bash
bench --turns multi --concurrency cfg
```

迁移到显式子命令：

```bash
bench probe -c config.yaml
bench user -c config.yaml --users 1
bench user -c config.yaml --users 8
bench rps -c config.yaml --request-rate 1,2,4,8
bench concurrency -c config.yaml --max-concurrency 1,2,4,8
```

显式子命令的好处：

- 不再通过 `--turns` 与 `--concurrency` 组合推断场景；
- 不会把 RPS 和闭环并发静默混在一起；
- 用户模式、请求模式和探针模式边界清晰；
- 命令行参数可以直接映射 vLLM `bench serve` 参数。

是否保留旧入口作为短期迁移别名，待后续讨论决定。项目当前不承诺配置 schema 向后兼容，因此可以直接切换，但需要同步文档和 smoke 测试。

## 7. 报告与数据契约目标

场景 JSON 的 `scenario` 应明确表达负载模型：

### 用户模式

```json
{
  "scenario": "user",
  "source": "trace",
  "sessions": []
}
```

### RPS 模式

```json
{
  "scenario": "rps",
  "source": "trace_request_set",
  "request_rate": 4,
  "max_concurrency": 64,
  "num_prompts": 1000,
  "requests": []
}
```

### 并发模式

```json
{
  "scenario": "concurrency",
  "source": "trace_request_set",
  "request_rate": "inf",
  "max_concurrency": 8,
  "num_prompts": 1000,
  "requests": []
}
```

需要注意：`request_rate` 当前 JSON 类型是浮点数，`inf` 是否落盘为字符串、零值是否代表无限速，需在数据契约设计阶段明确，不能依赖 Go JSON 对 `Inf` 的默认行为。

## 8. 代码改造范围

### CLI 与场景派发

- `cmd/bench/main.go`：新增 `user`、`rps`、`concurrency` 子命令；
- 移除通过 `--turns × --concurrency` 隐式组合场景的逻辑；
- `probe` 继续独立输出 ProbeResult。

### scenario 层

- 当前 `Concurrent` 拆为 `User`、`RPS`、`Concurrency`；
- `collectSessionTurns` 收敛为 user session runner；
- RPS 和 concurrency 使用统一 request runner；
- `runOpenRound` 改为 vLLM 风格的 arrival scheduler + max concurrency gate；
- 固定并发不再执行 filler 多轮逻辑；
- 移除正式路径中的 `profiles`/SWRR/turn_tokens 依赖。

### engine 层

保留：

- `Client.Chat`；
- SSE 解析和逐 chunk 计时；
- retry、cancel、drain；
- trace 加载和消息回放；
- `/metrics` 观测。

调整：

- 将 trace session 转换为可复用的 request sample；
- 明确 user session runner 和独立 request runner 的输入契约；
- filler 生成器降级为测试内部能力，必要时移出正式运行包。

### report 层

- 将当前 `ConcurrentLevel` 拆为请求负载档位结构；
- `user` 使用 sessions；
- `rps`/`concurrency` 使用 requests；
- 同步 `docs/data-contract.md`；
- `SchemaVersionCurrent` 递增。

### 测试与文档

- 重写 `scripts/smoke.sh`，覆盖四个子命令；
- 增加与 vLLM 参数语义一致的请求到达和并发上限测试；
- 增加请求集顺序、shuffle、复现性测试；
- 更新 `README.md`、`CODEBUDDY.md`、`docs/architecture.md`、`docs/scenario-guide.md`。

## 9. 推荐实施顺序

1. 先冻结新场景和数据契约草案；
2. 抽出统一 trace request sample 和 user session runner；
3. 将当前 RPS 调度改造成 vLLM 风格的 request arrival + max concurrency；
4. 把固定并发改造成独立请求模式；
5. 新增 `user` 子命令承载单用户/多用户 trace 会话；
6. 将 probe 与所有正式压测路径完全隔离；
7. 移除正式路径 filler 和多轮 filler 配置；
8. 更新报告结构、schema version、smoke 和文档；
9. 用同一请求集与 vLLM `bench serve` 做端到端对照验证。

## 10. 待讨论细节

以下问题在实现前需要确定：

1. `user` 多用户模式是固定用户数跑完指定 sessions，还是支持持续时长；
2. RPS 模式是否使用独立 request sample，还是允许 trace session 的每个 turn 展开成请求；
3. `concurrency` 是否只支持 `request_rate=inf`，还是同时保留有限 RPS + `max_concurrency`；
4. vLLM 对比使用 customer trace request set，还是严格复刻 vLLM `random` 数据集参数；
5. trace 中含多轮消息时，如何抽取 RPS/并发模式的独立请求；
6. 输入长度、输出长度是否从 trace usage 读取，还是从配置指定目标范围；
7. 多模型是否使用同一请求集，还是每个模型单独绑定 request set；
8. 是否删除旧 `single`、`multiturn`、`concurrent` 场景名，还是保留一段时间的迁移别名；
9. `request_rate=inf` 在配置和 JSON 中采用什么表示；
10. 是否需要导出 vLLM benchmark 所需的 custom dataset 文件。

## 11. 暂不做的内容

- 不在 llm-perf 内生成 HTML 报告；
- 不在 Go 内做聚合、判级和结论；
- 不把 filler 重新包装成“真实用户模拟”；
- 不把 tool-call probe 混入性能请求；
- 不为了兼容旧 schema 保留两套长期执行管线；
- 不在没有统一请求集的情况下宣称与 vLLM 数字严格可比。

## 12. `trace-real-128.json` 会话形状分析

分析样本：

```text
/Users/aleexjiang/Desktop/CodeBuddy/llm-perf/llm-perf-test/trace-real-128.json
```

该文件是 WorkBuddy/Codex 类型的编程 Agent 会话，不是普通聊天数据。文件包含 128 个会话、15,141 条消息和约 4,091 万字符；角色分布为 system 252 条、user 1,021 条、assistant 6,187 条、tool 7,681 条。125/128 个会话包含工具消息，说明它是强工具驱动型 workload。

### 12.1 会话轮数

以 `user` 消息数作为用户请求轮次：

| 指标 | 结果 |
|---|---:|
| 最少 | 2 轮 |
| P50 | 5 轮 |
| P75 | 9 轮 |
| P90 | 20 轮 |
| P95 | 26 轮 |
| 最大 | 32 轮 |
| 平均 | 8 轮 |

因此正式用户模式不应固定 `turns: 8`；应保留完整 session 的真实轮次分布，并支持短会话、典型会话、长会话分层分析。

### 12.2 消息形状

典型会话不是简单的：

```text
user → assistant → user → assistant
```

而是：

```text
system
user
assistant（可能只是工具调用）
tool × N
assistant
tool × N
assistant
user
...
```

第一个会话示例为：

```text
system(41,018)
user(378)
user(9,309)
assistant(776)
tool(1,001)
tool(3,140)
tool(18,801)
assistant(293)
tool(141)
assistant(190)
tool(162)
system(12,085)
```

它体现出三个重要特征：

1. system 基座本身很大；
2. 存在连续 user 消息；
3. assistant/tool 交互远多于 user 轮次。

`replay_mode: full` 应保留完整 role 顺序和 `tool_call_id`；当前样本的 7,681 条 tool 消息均带有工具调用标识，适合 full replay。

### 12.3 工具交互规模

按一个 user 到下一个 user 之间的消息块统计：

| 指标 | 结果 |
|---|---:|
| 工具消息数 P50 | 2 条 |
| 工具消息数 P90 | 23 条 |
| 最大工具消息数 | 111 条 |
| 回合块文本量 P50 | 6.3K 字符 |
| 回合块文本量 P90 | 103K 字符 |
| 最大回合块文本量 | 647K 字符 |

因此线上多轮增量不能用单一 `turn_tokens` 表示，应保留：

```text
当前 user 输入
+ assistant 工具调用
+ tool 结果
+ assistant 中间回复
+ 后续工具调用链
```

### 12.4 实际请求 prompt 规模

以每个 user 位置之前的完整消息前缀作为一次真实请求快照，共得到 1,021 个请求快照：

| 指标 | 字符数 | 按 4 字符/token 粗估 |
|---|---:|---:|
| P50 | 85.6K | 21.4K token |
| P75 | 145.8K | 36.4K token |
| P90 | 300.2K | 75.1K token |
| P95 | 824.3K | 206K token |
| 最大 | 1.93M | 483K token |

字符除以 4 仅用于形状估算，代码、中文和工具输出必须以实际 tokenizer 或服务端 usage 校准。

这说明该样本的真实 workload 是：

```text
大 system 基座
+ 不定长 user 输入
+ 大量 assistant/tool 历史
+ 逐轮增长的完整 prompt 前缀
```

不是固定每轮注入若干 token 的 filler workload。

### 12.5 对三种压测模式的落地结论

- `user`：直接使用 128 个完整 trace session；每个 user 消息是一次请求点，前面的 system/assistant/tool 历史按 full replay 保留；
- `rps`：从每个 user 位置提取完整历史前缀，形成 request snapshot，再按 `request_rate`、`num_prompts`、`max_concurrency` 调度；
- `concurrency`：使用固定 request set，所有请求独立，作为与 vLLM `bench serve` 的对比输入；
- filler：正式 benchmark 不再使用，仅保留给单元测试、mock、smoke 和 probe 最小连通性请求。

RPS/concurrency 不能直接把整个 session 当成一个请求。应从 session 提取 user-turn snapshot，并明确抽样策略：

```text
turn_uniform：所有 user 回合等概率
session_uniform：先等概率选 session，再选该 session 回合
```

默认推荐 `session_uniform`，避免 32 轮长会话对请求集产生过高权重；如果目标是复现完整历史请求总量，则使用 `turn_uniform`，但报告中必须记录该采样口径。

### 12.6 数据清洗与对比约束

1. 末尾 system 消息如果没有后续 user，不进入任何测量请求；
2. 连续 user 消息不应被静默合并，应保留为连续请求并记录 burst 关系；
3. assistant 文本长度不能直接当作 `max_tokens`，因为其中可能只是工具调用或中间回复；
4. trace 中没有完整的服务端生成参数时，vLLM 对比需要另行指定输出预算或按历史输出长度分桶；
5. llm-perf 与 vLLM 必须使用同一份 request set、同一输入消息、同一输出预算、同一请求数和同一调度参数，才能做严格数字比较；
6. 仅一边使用 customer trace、另一边使用 vLLM random dataset 时，只能比较趋势，不能宣称数字等价。
