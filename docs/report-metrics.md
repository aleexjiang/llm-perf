# 报告指标口径

本文定义 schema v9 JSON 的外部分析口径。Go 负责采集单轮数据和总 TPS 时间轴，
报告工具负责分位统计、分层、可视化和容量判断。

## 1. 单轮指标

当前工具的基本性能样本是一轮模型请求，即一条 `TurnMetrics`。user 场景的 `user_levels[]`
保存不同用户数档位，每个档位内部再保存 `sessions[]`。
`user` 的 session 只负责关联多轮上下文，不产生另一套 TPS 定义。

### TTFT

```text
ttft_ms = 首个含 token 的 reasoning/content chunk - sent_at
```

role-only、usage-only 和空 delta 不计入 TTFT。TTFT 包含排队和 prefill。

### E2E

```text
e2e_ms = end_at - sent_at
```

E2E 包含排队、prefill、首 token 等待、思考和输出阶段。

### TPOT

```text
tpot_ms = (e2e_ms - ttft_ms) / (completion_tokens - 1)
```

TPOT 包含 reasoning token。`completion_tokens <= 1` 或 TTFT 不可测时，TPOT 不可测。

### 思考时间

```text
think_ms = 首 content chunk - 首 reasoning chunk
```

没有 reasoning 或没有 content 时不落 `think_ms`。异常负值钳为 0 并写入 warning。

### 单轮 TPS

```text
tokens_per_sec = completion_tokens / ((e2e_ms - ttft_ms) / 1000)
```

它表示一轮请求从首 token 之后开始的平均输出速度，不包含 TTFT。
短于 8 个输出 token 且自然结束的样本，TPS 和 TPOT 在采集端置空，避免短样本噪声。

## 2. 总 TPS 时间轴

对每条成功流式请求，decode 区间为 `[sent_at + ttft_ms, end_at)`。
在时间轴任意时刻画一条竖线，找出当前正在 decode 的请求；这一时刻的总 TPS 是这些请求各自
`tokens_per_sec` 的总和：

```text
total_tps(t) = Σ tokens_per_sec(request_i)
```

输出按 1 秒时间桶落盘：

```json
{
  "second": 0,
  "decode_requests": 3,
  "tps": 612.4
}
```

`second` 从场景第一条有效 decode 区间开始计；`decode_requests` 和 `tps` 取该秒中点状态。
user 场景放在 `throughput.total_tps[]`，rps/concurrency 放在各自的 `concurrent[].total_tps[]`。

总 TPS 不使用 session 平均 TPS、请求数乘单流平均值或其他二次估算。

## 3. 聚合摘要

场景级 `throughput` 保留：

```text
wall_seconds
completed_requests
failed_requests
cancelled_requests
completion_tokens
total_tps[]
streaming.all|stop|length
  count
  p50_tps / p95_tps / p99_tps
  p50_ttft_ms / p95_ttft_ms
  p50_tpot_ms / p95_tpot_ms
```

`stop` 和 `length` 必须分层。P95 是主报告口径，P50 只描述分布；每个分层必须同时显示样本数。

报告可以从 `total_tps[]` 计算均值、P95、峰值，但必须标明统计窗口：

- 有 decode 的时间窗口：只统计至少有一个请求 decode 的秒；
- 整个场景窗口：从场景开始到结束的每一秒都统计，包括总 TPS 为 0 的秒。

两者含义不同，不能都简称为“总 TPS”。

## 4. 样本过滤

成功样本必须满足：

```text
error == ""
cancelled != true
completion_tokens > 0
```

TTFT、TPOT、TPS 还要求流式指标可测。失败、取消、usage 缺失和不完整流仍保留原始记录，
但不进入成功指标聚合。

`warmup` 和 `correctness` 进入 `auxiliary_requests[]`，不进入 benchmark KPI。

## 5. 服务端对账

`/metrics` 是独立的场景级数据源，不挂到单条请求。报告可使用：

```text
server_metrics.generation_tokens
server_metrics.prompt_tokens
server_metrics.cache_hit_tokens / cache_query_tokens
server_metrics.gauges.running / waiting / kv_usage
server_metrics.preemptions
server_metrics.histograms
```

`source_check` 用客户端成功请求的 completion token 与服务端 generation token 做对账。
服务端数据只用于采集正确性和瓶颈归因，不改变客户端单轮指标或总 TPS 主口径。

## 6. 分层与诊断

总 TPS 和单轮指标应按以下维度切分，用于解释性能变化：

- `finish_reason=stop` 或 `length`；
- prompt token 长度区间；
- completion token 长度区间；
- thinking 开关和 reasoning token；
- prefix cache 命中状态；
- 客户端 decode 请求数与服务端 running/waiting。

这些是解释维度，不是新的 TPS 定义。

## 7. 不进入 schema v9 的内容

分析可以从单轮数据重算的派生值不进入 JSON，旧字段不回填：

```text
total_tokens
throughput_tps
active_decode_tokens / active_decode_seconds / active_decode_tps
weighted_tps
streaming.*.active_decode_*
ttft_reasoning_ms / ttft_content_ms
reasoning_field / reasoning_preview
new_tokens
server_counter_delta
chunk counts / character counts / ITL / raw chunk timestamps / reply previews
```

客户端仍可在内存和 debug 日志中使用 chunk、ITL、首帧时间和回复文本，
但这些不是性能 JSON 契约。
