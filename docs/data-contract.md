# 数据契约：YAML 到 JSON

工具采集 OpenAI 兼容 LLM 服务的原始性能数据。报告、分位统计和容量判断由外部完成。
场景 JSON 当前为 **schema v14**，由 `contract.SchemaVersionCurrent` 写入；结构变化直接递增版本，
不保留旧字段兼容逻辑。

## 落盘结构

```text
<output_dir>/<scenario>-<timestamp>.json
<output_dir>/<model>/<scenario>-<timestamp>.json  # 多模型分区
<output_dir>/run.log
<output_dir>/raw/                                  # 仅 debug 或失败证据
```

`probe` 输出独立的 `probe.json`。长场景每完成一个 user level、RPS rate 或 concurrency
level，在最终 JSON 旁写一份累计 `*.checkpoint-NNN.json`；checkpoint 与最终 JSON 使用同一
schema，最终文件是完整结果。

## 顶层结构

```text
Report
├── schema_version / tool / scenario / generated_at / test / endpoint / note
├── slo / slo_baseline / plan
├── user_levels[]               # user：用户数档位、会话及该档位摘要
├── concurrent[]                # rps/concurrency：档位及 requests[]
├── correctness[]
├── auxiliary_requests[]        # warmup/correctness 完整样本
├── server_metrics              # 场景级 /metrics 观测
├── kv_capacity                 # 可选 KV 画像
├── source_check                # rps/concurrency 的客户端/服务端 token 对账
├── metrics                     # rps/concurrency 场景级摘要；user 不写顶级字段
├── environment                 # 可选 probe 环境快照
└── config_raw
```

### `user_levels[]`

```text
model / thinking / users / max_tokens
sessions[] -> MultiturnRun
metrics -> bucket_tps[] 和单轮分布
server_metrics -> 该档位的服务端窗口观测
```

`workload` 记录该档位实际生效的 profile、比例、附件概率和上下文预算；报告从这里读取
负载形状，不依赖 `config_raw` 反推。

`user.levels` 按配置顺序串行执行。`sessions[]` 只关联多轮上下文；主性能样本是其中的
`turns[]`。user 的吞吐摘要只从各档位 `metrics` 读取，不再复制到顶层。

### `concurrent[]`

```text
model / thinking / max_tokens
level / request_rate
requests[] 或 sessions[]
wall_seconds
bucket_tps[]
completed_requests / failed_requests / cancelled_requests / invalid_requests
slo_meet / slo_total / goodput_rps / goodput_tps
waiting_max / running_max / aborted
```

`request_rate > 0` 表示 RPS 开环档位；`level > 0` 表示固定并发档位。

## `TurnMetrics`

每条 turn 是一轮模型请求：

| 字段 | 含义 |
|---|---|
| `model` / `stream` / `thinking` / `phase` | 请求上下文；phase 为 `benchmark`、`warmup` 或 `correctness` |
| `sent_at` / `end_at` | 客户端请求起止；流式 `end_at` 取 `[DONE]` 到达时刻 |
| `prompt_tokens` / `completion_tokens` | usage 输入/输出 token；completion 含 reasoning token |
| `reasoning_tokens` / `cached_tokens` | 服务端提供时存在 |
| `ttft_ms` / `think_ms` | 首个 token 延迟；首 reasoning 到首 content 的时长 |
| `tpot_ms` / `tokens_per_sec` | 首 token 后的平均 token 间隔/速度；两者严格互逆 |
| `e2e_ms` | 完整请求耗时 |
| `finish_reason` | `stop`、`length` 等 |
| `error` / `cancelled` / `stream_broken` | 失败、主动取消、流中断 |
| `retry_count` / `warnings` | 连接层重试次数和兼容性告警 |

以下数据只在内存、日志或显式 debug 中使用：chunk 数、字符数、首帧时间、ITL、原始 chunk
时间、回复预览和 tool-call 聚合。

## 总 TPS 时间轴

每条成功流式 turn 的 decode 区间为：

```text
decode_start = sent_at + ttft_ms
decode_end   = end_at
```

时间轴从该档位第一条有效 decode 区间开始，按一秒桶积分：首 token 计入所在桶，剩余
`completion_tokens - 1` 按请求 decode 区间与桶的重叠时长分配：

```json
{
  "second": 0,
  "avg_decode_requests": 2.4,
  "tps": 612.4
}
```

`avg_decode_requests` 是请求 decode 区间在该桶内的重叠秒数之和；`tps` 是桶内估算 token
数除以一秒。末桶按完整一秒处理，未覆盖部分为零。所有桶的 token 总量与参与请求的
`completion_tokens` 守恒。user 写入各 `user_levels[].metrics.bucket_tps[]`；
rps/concurrency 写入各档位 `concurrent[].bucket_tps[]`。失败、取消和 usage 缺失的 turn 不进入
时间轴；单轮 TPS 因短输出不可测时，可信 completion tokens 仍进入桶积分。

## `metrics`

```text
wall_seconds / completed_requests / failed_requests / cancelled_requests / invalid_requests
completion_tokens / bucket_tps_mean / bucket_tps_p95 / bucket_tps_peak / bucket_tps[]
request_shape
thinking
streaming.all|stop|length
  count
  p5_tps / p50_tps / p95_tps / p99_tps
  p50_ttft_ms / p95_ttft_ms
  p50_tpot_ms / p95_tpot_ms
  p95_e2e_ms
```

`bucket_tps_mean/p95/peak` 是 `bucket_tps[]` 的桶摘要。user 将该摘要放在每个
`user_levels[].metrics`；rps/concurrency 同时保留场景级摘要和档位级 `metrics`。
`stop` 与 `length` 不能混算；延迟/TPOT 的坏尾部看 P95，TPS 的低速尾部看 P5，
P50 描述典型值。

`request_shape` 记录有效请求的 prompt/completion/cached token 分布，用于外部分析报告的
请求形状概述。

`thinking` 记录思考时间分布；`thinking=off` 时 count 与分位也为 0，报告必须显式写出。

## 服务端观测

`server_metrics` 在场景或档位窗口内采集，不挂到单条请求：

```text
available / note
cache_hit_tokens / cache_query_tokens
prompt_tokens / generation_tokens
preemptions / spec_drafts / spec_accepted_tokens
window_seconds
gauges{} / histograms{}
observation_degraded / observation_note
```

`source_check` 比较窗口内客户端成功请求的 completion token 与服务端 generation token；
两者共享同一结束快照，不改变客户端单轮指标或总 TPS 主口径。

## 样本规则

- `error != ""` 和 `cancelled=true` 的样本完整保留，但不进入成功聚合、总 TPS 或 SLO 分母。
- `usage_missing` 或 token 不可信的正常响应计入 `invalid_requests`，不参与成功计数、token、
  TPS、TPOT 或 SLO 聚合。
- 少于 8 个输出 token 且自然结束的流式样本，decode 指标在采集端置空。
- `warmup` / `correctness` 只进入 `auxiliary_requests[]`，不进入 benchmark KPI。

## 已移除字段

以下字段不属于 schema v14，不回填、不兼容：

```text
total_tokens
metrics_tps_legacy_removed
active_decode_tokens / active_decode_seconds / active_decode_tps
weighted_tps
streaming.*.active_decode_*
ttft_reasoning_ms / ttft_content_ms
reasoning_field / reasoning_preview
new_tokens
server_counter_delta
chunk counts / character counts / ITL / raw chunk timestamps / reply previews
decode_requests
```
