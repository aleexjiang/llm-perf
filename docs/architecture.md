# 内部架构

llm-perf 的边界是：配置加载、请求调度、客户端计时、单轮数据采集、总 TPS 时间轴生成和 JSON 落盘。
报告呈现、分位统计、容量判断由外部工具完成。

## 数据流

```text
YAML
  -> cmd/bench
  -> scenario
       probe: engine.Probe -> probe JSON
       user: user.levels + profile + corpus -> 串行用户阶梯
       rps/concurrency: frozen request set -> 调度请求
  -> engine.Client.Chat -> TurnMetrics
  -> report.Report -> schema v9 JSON
       + total_tps[] 时间轴
       + server_metrics 场景观测
       + source_check token 对账
```

## 包职责

| 包 | 职责 |
|---|---|
| `cmd/bench` | CLI 子命令、配置加载、模型过滤、输出路径和中断处理 |
| `internal/config` | YAML、环境变量、模型差异和 thinking 变体配置 |
| `internal/auth` | chat 与 `/metrics` 共用的认证方案 |
| `internal/corpus` | user 模式的内置文本和确定性窗口 |
| `internal/engine` | OpenAI 兼容客户端、SSE 解析、单轮计时和 probe |
| `internal/scenario` | user.levels/rps/concurrency 调度、失败处理、SLO、场景观测 |
| `internal/smetrics` | 场景级 `/metrics` counter、gauge、histogram 和 KV 画像 |
| `internal/report` | schema v9 结构、总 TPS 时间轴和 JSON 落盘 |

## 单轮采集

`engine.TurnMetrics` 是所有场景的核心样本。外部性能字段只有：

```text
sent_at / end_at
prompt_tokens / completion_tokens / reasoning_tokens / cached_tokens
ttft_ms / think_ms / tpot_ms / tokens_per_sec / e2e_ms
finish_reason / error / cancelled / stream_broken / retry_count / warnings
```

chunk 数、字符数、ITL、原始时间、回复文本和 reasoning 协议细节只保留在内存或 debug 中。
`total_tokens`、`new_tokens`、逐请求 `/metrics` counter 和旧聚合字段不属于 schema v9。

## 总 TPS

`report.BuildTotalTPS` 将每条成功流式请求投影为：

```text
[sent_at + ttft_ms, end_at)
```

每秒时间点的 `tps` 是该秒中点正在 decode 的请求的 `tokens_per_sec` 之和，
`decode_requests` 是请求数。user 结果写在 `throughput.total_tps[]`，
rps/concurrency 结果写在各档位 `total_tps[]`。

场景每完成一个档位就通过 `RunOptions.Checkpoint` 通知 CLI，CLI 写累计 checkpoint JSON；
请求完成时 rps/concurrency 同时输出 progress 日志。最终场景返回后再写正式 JSON。

## 服务端观测

`setupServerMetrics` 装配 `/metrics`，`startWindow/finishWindow` 负责场景窗口采集。
服务端 counter、gauge、histogram 只写场景级 `server_metrics`；`source_check` 比较服务端 generation
token 和客户端 completion token。服务端观测不可用时不改变客户端主口径。

## 场景

- `user.go`：profile 分配、确定性 corpus、真实 assistant 回复进 history。
- `requests.go`：rps 的开环到达和 concurrency 的固定在飞调度，共享 request set。
- `probe.go`：模型、usage、thinking、tool-call 和扩展 `/metrics` 能力探测。

## 扩展规则

- 新单轮字段必须属于核心性能数据或明确的 debug 数据；同步更新 `docs/data-contract.md`。
- 数据结构变化直接递增 `SchemaVersionCurrent`，不添加旧字段兼容分支。
- 新场景通过 scenario 注册表接入；通用观测和中断逻辑复用 `scenario.go`。
- 新服务端指标加入 `smetrics` 的规范化映射，未知命名明确告警，不静默套用错误语义。
