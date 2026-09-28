# 数据契约：YAML 到 JSON

本工具负责采集 OpenAI 兼容 LLM 服务的原始性能数据。Go 输出 schema v8 JSON；报告、
分位统计、容量判定和可视化由外部分析工具完成。

## 版本

场景 JSON 的 `schema_version` 当前为 **9**，由 `report.SchemaVersionCurrent` 写入。
schema 变更直接更新结构和版本，不保留旧字段兼容逻辑，不同时落新旧字段。

## 落盘结构

```text
<output_dir>/<scenario>-<timestamp>.json
<output_dir>/<model>/<scenario>-<timestamp>.json  # 多模型分区
<output_dir>/run.log
<output_dir>/raw/                                  # 仅 debug 或失败证据
```

`probe` 输出独立的 `probe.json`，不与场景 JSON 共用顶层结构。

## 顶层结构

```text
Report
├── schema_version / tool / scenario / generated_at / test / endpoint / note
├── slo / slo_baseline / plan
├── user_levels[]               # user：用户数档位、会话及该档位摘要
├── concurrent[]                # rps/concurrency：档位及 requests[]
├── correctness[]               # 正确性金丝雀结果
├── auxiliary_requests[]        # warmup/correctness 的完整样本
├── server_metrics              # 场景级 /metrics 观测
├── kv_capacity                 # 环境级 KV 画像
├── source_check                # 客户端与服务端 token 对账
├── throughput                  # 总 TPS 时间轴和单轮分布摘要
├── environment                 # 可选 probe 环境快照
└── config_raw
```

### `user_levels[]`

```text
model / thinking / users / max_tokens
sessions[] -> MultiturnRun
throughput -> total_tps[] 和单轮分布
server_metrics -> 该档位的服务端窗口观测
```

`user.levels` 按配置顺序串行执行；`session` 只用于关联多轮上下文，主性能指标来自每个 `turn`。

### `concurrent[]`

```text
model / thinking / max_tokens
level / request_rate
requests[] 或 sessions[]
wall_seconds
total_tps[]
completed_requests / failed_requests / cancelled_requests
slo_meet / slo_total / goodput_rps / goodput_tps
waiting_max / running_max / aborted
```

`request_rate > 0` 表示 RPS 开环档位；`level > 0` 表示固定并发档位。

## `TurnMetrics` 核心字段

每条 turn 是一轮模型请求的核心样本：

| 字段 | 含义 |
|---|---|
| `model` | 服务端模型名 |
| `stream` | 是否流式 |
| `thinking` | 是否开启思考 |
| `phase` | `benchmark`、`warmup` 或 `correctness` |
| `sent_at` / `end_at` | 客户端请求开始和结束时间 |
| `prompt_tokens` | usage 返回的输入 token |
| `completion_tokens` | usage 返回的输出 token，包含 reasoning token |
| `reasoning_tokens` | usage 返回的思考 token，服务端未提供时缺失 |
| `cached_tokens` | usage 返回的 prefix cache token，服务端未提供时缺失 |
| `ttft_ms` | 首个含 token 的 reasoning/content chunk 延迟 |
| `think_ms` | 首 reasoning chunk 到首 content chunk 的时间 |
| `tpot_ms` | `(e2e_ms - ttft_ms) / (completion_tokens - 1)` |
| `tokens_per_sec` | `completion_tokens / ((e2e_ms - ttft_ms) / 1000)` |
| `e2e_ms` | 请求发出到响应结束的时间 |
| `finish_reason` | `stop`、`length` 等结束原因 |
| `error` | 失败原因；非空表示样本失败 |
| `cancelled` | 客户端主动取消 |
| `stream_broken` | 流式读取中断 |
| `retry_count` | 实际发生的连接层重试次数 |
| `warnings` | usage、协议或响应完整性告警 |

以下字段不进入性能 JSON，只在客户端内存、日志或显式 debug 中使用：chunk 数量、字符数、
首帧时间、ITL、原始 chunk 时间、回复预览和工具调用聚合。

## 总 TPS 时间轴

`throughput.total_tps[]` 和每个并发档位的 `concurrent[].total_tps[]` 结构为：

```json
{
  "second": 0,
  "decode_requests": 3,
  "tps": 612.4
}
```

对每条成功流式 turn：

```text
decode_start = sent_at + ttft_ms
decode_end   = end_at
```

以场景第一条有效 decode 区间为时间轴起点。每个一秒点取该秒中点状态：

```text
decode_requests = 中点时刻正在 decode 的请求数
tps = 这些请求各自 tokens_per_sec 的总和
```

失败、取消、usage 缺失和 `tokens_per_sec` 不可测的 turn 不进入总 TPS。

## `throughput`

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

`stop` 和 `length` 分层，避免自然结束和输出预算截断混成一个性能结论。P95 是主报告口径，
P50 只描述分布；样本量必须同时展示。

## 服务端观测

`server_metrics` 只在场景级采集 `/metrics`，不挂到单条 `TurnMetrics`：

```text
available / note
cache_hit_tokens / cache_query_tokens
prompt_tokens / generation_tokens
preemptions
spec_drafts / spec_accepted_tokens
window_seconds
gauges{} / histograms{}
observation_degraded / observation_note
```

`generation_tokens` 与客户端 `completion_tokens` 通过 `source_check` 做对账。服务端数据是归因和
采集正确性校验，不改变客户端单轮指标或总 TPS 的主口径。

## 样本规则

- `error != ""` 的失败样本完整保留，但不进入成功统计。
- `cancelled=true` 的样本完整保留，但不进入完成数、总 TPS 或 SLO 分母。
- `usage_missing` 或 token 不可信的样本不得参与 token、TPS、TPOT 聚合。
- 短于 8 个输出 token 且自然结束的流式样本，decode 指标在采集端置空。
- `warmup` 和 `correctness` 进入 `auxiliary_requests[]`，不进入 benchmark KPI。

## 已移除字段

以下字段不属于 schema v9，不回填、不兼容：

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
```
