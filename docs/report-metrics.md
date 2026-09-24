# 报告口径计算

> 本文只定义报告侧指标的计算口径，修改报告脚本或解释 `user` 多会话结果时以本文为准。
> JSON 字段结构见 [data-contract.md](data-contract.md)；方法论背景见
> [testing-architecture.md](testing-architecture.md)；阈值依据见
> [latency-baselines.md](latency-baselines.md)。

## 1. 总吞吐与单流速度

单流速度和总吞吐必须分开定义，不能把会话间隙计入单流 TPS。

### 1.1 会话单流 TPS

一个会话的单流 TPS = 该会话各成功轮 decode 速度的算术平均。

```text
session_tps = mean(turn_tokens_per_sec for 成功轮)
```

例如一个会话有 2 轮，就是这两轮 `tokens_per_sec` 的平均值；有 N 轮就是 N 轮的成功轮平均值。
这里不引入“会话总输出 / 会话墙钟”的定义。

### 1.2 总吞吐：同一时刻活跃会话单流 TPS 之和

总吞吐不是单会话 TPS，也不是 `总输出 token / 全局墙钟`。它是对时间轴上的每个时刻，
把当时仍处于对话过程的活跃会话的单流 TPS 相加；再按会话覆盖窗口做时间平均。

```text
total_throughput_tps(t) = Σ active_session_tps(t)
mean_total_throughput = ∫ total_throughput_tps(t) dt / session_coverage_window
```

这个分母不加会话结束后的空闲时间，也不是把 4 个会话的 TPS 简单求平均。

### 1.3 单轮 decode 速度

单轮 decode 速度（TPOT 的倒数或 `tokens_per_sec`）只描述首 token 之后的出字速度；
它是诊断层指标，不是会话单流 TPS，也不直接等于总吞吐。总吞吐的主定义是上面的
“活跃单流 TPS 之和”。

### 1.4 与行业口径的关系

vLLM 的在线 benchmark 通常用 `output tokens / benchmark window` 报 output throughput；
AIPerf 用 `total output tokens / (last response - first request)`。这些是 batch 级平均值，
会把会话间隙计入窗口。本工具的“会话单流 TPS / 活跃单流之和”是另一套口径：
单流按轮次 decode 速度平均，总吞吐按活跃会话单流 TPS 的时间积分计算；两者都应保留并明确命名。

## 2. 样本过滤

场景总吞吐和单流速度只统计**完整成功请求**：

```text
成功 = error 缺失且 cancelled != true
```

以下样本不进入总吞吐和单流加权：

- `error` 非空的失败请求；
- `cancelled=true` 的主动取消请求；
- usage 缺失导致 `completion_tokens` 不可信的请求。

失败和取消样本仍保留在原始 JSON 中，并在报告里单列计数。

单流速度还必须满足：

```text
stream = true
ttft_ms > 0
e2e_ms > ttft_ms
completion_tokens > 0
```

极短输出（`completion_tokens < 8` 且 `finish_reason=stop`）在采集端已置空
decode 指标，不得混入单流加权。

## 3. 分层口径

单流速度必须按 `finish_reason` 分层：

| 层 | 含义 | 用途 |
|---|---|---|
| `all` | 全部成功请求 | 总览，保留但不要单独作为主结论 |
| `stop` | 模型自然结束 | 正常完成的单流速度 |
| `length` | 输出预算打满 | 截断样本，workload 形状与 stop 不同 |

`stop` 和 `length` 不能混成一个中位数。主结论使用各层的 `weighted_tps`；

## 3.1 P95 优先

性能指标一律以 P95 为报告主口径，P50 只用于描述样本分布，不作为判级依据：

- TTFT：报告 P95，不把 P50 当主结论；
- TPOT：报告 P95，不把 P50 当主结论；
- E2E：报告 P95，不把 P50 当主结论；
- 单流速度：报告全部成功轮的 P95；加权单流值作为总览和吞吐分解用；
- 输入/输出 token 的 P50/中位数只用于描述负载构成，不是性能指标。

当样本量很小（例如某分层只有 1-3 条）时，P95 的统计稳定性不足，报告必须同时显示样本量，
不能用该层的 P95 直接外推容量。

## 4. JSON 字段映射

场景级结果位于顶层 `throughput`：

```text
throughput.wall_seconds
throughput.completed_requests
throughput.failed_requests
throughput.cancelled_requests
throughput.completion_tokens
throughput.throughput_tps

throughput.streaming.all
throughput.streaming.stop
throughput.streaming.length
  count
  completion_tokens
  decode_seconds
  weighted_tps
  p50_tps
  p95_tps
  p99_tps
  p50_ttft_ms
  p95_ttft_ms
  p50_tpot_ms
  p95_tpot_ms
```

`throughput` 对 `user`、`rps`、`concurrency` 都适用：

- `user` 的场景墙钟是整个多会话执行的起止时间；
- `rps` / `concurrency` 的场景墙钟是各档位 `wall_seconds` 之和。

## 5. 多会话解释

“4 个会话”只表示有 4 条业务会话。总吞吐按会话单流 TPS 的时间积分计算：

```text
session_tps = mean(该会话各成功轮 tokens_per_sec)
total_throughput_tps(t) = Σ 当前活跃会话的 session_tps
mean_total_throughput = ∫ total_throughput_tps(t) dt / 会话覆盖窗口
```

会话之间的空档不计入分母；会话重叠时，多个会话的单流 TPS 相加。

报告必须同时展示：

1. 每个会话的单流 TPS（各成功轮 TPS 的平均值）；
2. 活跃会话单流 TPS 之和得到的总吞吐；
3. batch 平均 tok/s 作为“总输出 / 首请求到末响应”的交叉参考；
4. TTFT、单请求 decode P95、E2E P95 作为诊断层指标。

## 6. 示例

以单卡三轮 `user` 数据为例（r1 各会话单流 TPS）：

| 会话 | 档位 | 成功轮数 | 各轮 TPS 平均 |
|---|---:|---:|---:|
| 1 | light | 2 | 14.33 |
| 2 | medium | 6 | 52.45 |
| 3 | light | 3 | 21.78 |
| 4 | heavy | 10 | 84.78 |

总吞吐再按这四条会话在时间轴上的重叠情况，对各自的单流 TPS 求和并积分。

## 7. 常见误用

- 用所有轮次的 `tokens_per_sec` p50 作为单流结论：会被 `stop` / `length` 样本占比支配。
- 用总吞吐除以会话数推算单流速度：会话数不是 decode 并行度。
- 用 `加权单流速度 × 会话数` 预测总吞吐：缺少 `decode 时间占墙钟比例`。
- 把未配置 SLO 的 `goodput=0` 解释为吞吐为 0：0 表示未统计。
- 把失败请求或主动取消请求计入总吞吐：违反完整成功请求口径。
