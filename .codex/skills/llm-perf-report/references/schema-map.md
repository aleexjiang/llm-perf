# Field Map for Reports

Use schema v15 paths. `...` means every matching row.

## Common

| Meaning | Path |
|---|---|
| schema version | `schema_version` |
| model | `user_levels[].model` or `concurrent[].model` |
| thinking mode | `thinking` on user/concurrent rows |
| output budget | `max_tokens` |
| request count | `metrics.request_shape.count` |
| completed / failed / cancelled / invalid | `metrics.completed_requests` and siblings |
| wall time | `metrics.wall_seconds` |

## Request shape

| Meaning | Path |
|---|---|
| prompt total | `metrics.request_shape.prompt_tokens` |
| prompt mean | `metrics.request_shape.prompt_tokens_mean` |
| prompt P50 / P95 | `p50_prompt_tokens` / `p95_prompt_tokens` |
| prompt min / max | `prompt_tokens_min` / `prompt_tokens_max` |
| completion total / mean | `completion_tokens` / `completion_tokens_mean` |
| completion P50 / P95 | `p50_completion_tokens` / `p95_completion_tokens` |
| completion min / max | `completion_tokens_min` / `completion_tokens_max` |
| cached total | `metrics.request_shape.cached_tokens` |

Prompt is cumulative multi-turn context for user rows and full frozen request text for
rps/concurrency rows.

## Performance summary

| Meaning | Path |
|---|---|
| one-second throughput series | `metrics.bucket_tps[].tps` |
| average decode parallelism | `metrics.bucket_tps[].avg_decode_requests` |
| displayed as 每秒总吞吐均值 | `metrics.bucket_tps_mean` |
| displayed as 每秒总吞吐 P95 | `metrics.bucket_tps_p95` |
| displayed as 每秒总吞吐峰值 | `metrics.bucket_tps_peak` |
| single-request TPS P5/P50/P95/P99 | `metrics.streaming.all.p5_tps`, `p50_tps`, `p95_tps`, `p99_tps` |
| TTFT P50/P95 | `p50_ttft_ms` / `p95_ttft_ms` |
| TPOT P50/P95 | `p50_tpot_ms` / `p95_tpot_ms` |
| E2E P95 | `p95_e2e_ms` |
| think time | `metrics.thinking.p50_think_ms`, `p95_think_ms`, `max_think_ms` |
| stop/length layers | `metrics.streaming.stop` / `metrics.streaming.length` |

## User workload

| Meaning | Path |
|---|---|
| profile name | `workload.profile` |
| first-turn prompt range | `workload.first_turn_tokens` |
| shared system base tokens | `workload.shared_base_tokens` |
| tier weight | `workload.tiers.<tier>.weight` |
| tier turns / user input / regular context | `workload.tiers.<tier>.turns_range`, `user_input_tokens`, `context_tokens` |
| tier context-burst probability | `workload.tiers.<tier>.context_burst_probability` |
| tier context-burst token range | `workload.tiers.<tier>.context_burst_tokens` |
| context budget | `workload.context_budget_tokens` |
| per-turn planned input | `sessions[].input_plan[]`, aligned with `sessions[].turns[]` |

## Server observation and integrity

| Meaning | Path |
|---|---|
| cache hit tokens | `server_metrics.cache_hit_tokens` |
| cache query tokens | `server_metrics.cache_query_tokens` |
| preemptions | `server_metrics.preemptions` |
| running peak | `running_max` on rps/concurrency; `server_metrics.gauges.running.max` for user |
| waiting peak | `waiting_max` on rps/concurrency; `server_metrics.gauges.waiting.max` for user |
| client/server token deviation | `source_check.deviation` (rps/concurrency only; valid only when `server_tokens > 0` and `note` is empty) |

## Raw samples

Use these for distributions not pre-summarized:

- user: `user_levels[].sessions[].turns[]`
- rps/concurrency: `concurrent[].requests[]`

Valid samples require `error == ""`, `cancelled != true`, and `completion_tokens > 0`.
