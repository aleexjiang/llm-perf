# 报告指标口径

本文定义 schema v11 的外部分析口径。Go 负责采集单轮数据和总 TPS 时间轴；报告工具负责
分位、分层、可视化和容量判断。

## 单轮指标

基本性能样本是一轮模型请求，即 `TurnMetrics`：

```text
ttft_ms = 首个含 token 的 reasoning/content chunk - sent_at
e2e_ms  = end_at - sent_at
tpot_ms = (e2e_ms - ttft_ms) / (completion_tokens - 1)
tps     = (completion_tokens - 1) / ((e2e_ms - ttft_ms) / 1000)
think_ms = 首 content chunk - 首 reasoning chunk
```

- role-only、usage-only、空 delta 不计入 TTFT；TTFT 含排队和 prefill。
- TPOT 和 TPS 含 reasoning token；首 token 位于 decode 区间起点，因此两者严格互逆。
  `completion_tokens <= 1` 或 TTFT 不可测时不可用。
- `end_at` 取流式 `[DONE]` 到达时刻，不包含代理延迟关闭响应体的等待。
- 没有 reasoning 或 content 时没有 `think_ms`；异常负值钳为 0 并写 warning。
- 少于 8 个输出 token 且自然结束的流式样本，TPS/TPOT 置空，避免短样本噪声。

## 总 TPS

成功流式请求的 decode 区间为 `[sent_at + ttft_ms, end_at)`。所有场景统一使用一秒桶积分：

```text
首 token -> 计入 decode_start 所在桶
其余 token -> (completion_tokens - 1) × 请求与桶的重叠时长 / decode 总时长
bucket_tps -> 桶内 token 数 / 1 秒
avg_decode_requests -> 各请求与桶的重叠秒数之和 / 1 秒
```

输出结构：

```json
{
  "second": 0,
  "avg_decode_requests": 2.4,
  "tps": 612.4
}
```

`second` 从该档位第一条有效 decode 区间开始。末桶不足一秒的部分按零吞吐补齐，因此每点
都是完整一秒桶。桶积分满足 `sum(total_tps[].tps) == sum(completion_tokens)`（允许浮点误差）。
user 写入各 `user_levels[].throughput.total_tps[]`；rps/concurrency 写入各
`concurrent[].total_tps[]`。总 TPS 不使用中点抽样、session 平均 TPS 或请求数乘单流平均值。

## 聚合摘要

每个档位的 `throughput` 包含：

```text
wall_seconds / completed_requests / failed_requests / cancelled_requests / invalid_requests
completion_tokens / total_tps[]
streaming.all|stop|length
  count
  p5_tps / p50_tps / p95_tps / p99_tps
  p50_ttft_ms / p95_ttft_ms
  p50_tpot_ms / p95_tpot_ms
```

user 的摘要只在各 `user_levels[].throughput`；rps/concurrency 另有场景级 `throughput`。
`stop` 和 `length` 必须分层。延迟与 TPOT 以 P95 表示坏尾部；TPS 以 P5 表示低速尾部，
P50 描述典型值，P95/P99 只表示高速侧分布。每个分层必须显示样本数。

报告可从 `total_tps[]` 计算桶均值、P95 和峰值。时间轴覆盖第一条有效 decode 开始到最后一条
有效 decode 所在一秒桶结束；首个 decode 前的 prefill 不在该时间轴内。

## 样本过滤

进入成功聚合的样本必须满足：

```text
error == ""
cancelled != true
completion_tokens > 0
```

TTFT、TPOT、TPS 还要求对应流式指标可测。失败、取消、usage 缺失和不完整流保留原始记录。
usage 无效的正常响应计入 `invalid_requests`；这些样本都不进入成功指标、总 TPS 或 SLO 分母。
`warmup` / `correctness` 不进入 benchmark KPI。

## 服务端对账

`/metrics` 是独立数据源，不挂到单条请求。报告可使用：

```text
server_metrics.generation_tokens / prompt_tokens
server_metrics.cache_hit_tokens / cache_query_tokens
server_metrics.gauges.running / waiting / kv_usage
server_metrics.preemptions / histograms
```

`source_check` 用客户端成功请求的 completion token 与服务端 generation token 对账。
两者共享同一窗口结束快照。服务端数据只用于采集正确性和瓶颈归因，不改变客户端单轮指标
或总 TPS 主口径。

## 分层与诊断

总 TPS 和单轮指标应按以下维度切片：`finish_reason`、prompt/completion 长度、
thinking/reasoning、prefix cache，以及客户端 decode 请求数与服务端 running/waiting。
这些是解释维度，不是新的 TPS 定义。

以下旧字段不属于 schema v11，不回填：

```text
total_tokens
throughput_tps
active_decode_tokens / active_decode_seconds / active_decode_tps
weighted_tps
streaming.*.active_decode_*
ttft_reasoning_ms / ttft_content_ms
reasoning_field / reasoning_preview / new_tokens / server_counter_delta
chunk counts / character counts / ITL / raw chunk timestamps / reply previews
decode_requests
```
