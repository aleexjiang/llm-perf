# 内部架构

llm-perf 的边界是配置加载、请求调度、客户端计时、单轮采集、总 TPS 时间轴生成和 JSON
落盘。报告呈现、分位统计和容量判断由外部工具完成。

## 数据流

```text
YAML
  -> cmd/bench
  -> scenario
       probe: engine.Probe -> probe JSON
       user: user.levels + profile + corpus -> 串行用户阶梯
       rps/concurrency: frozen request set -> 调度请求
  -> engine.Client.Chat -> TurnMetrics
  -> report.Report -> schema v11 JSON
       + user_levels[].throughput / concurrent[].total_tps
       + server_metrics 场景或档位观测
       + source_check token 对账
```

## 包职责

| 包 | 职责 |
|---|---|
| `cmd/bench` | CLI、配置加载、模型过滤、输出路径和中断处理 |
| `internal/config` | YAML、环境变量、模型差异和 thinking 变体 |
| `internal/auth` | chat 与 `/metrics` 共用的认证方案 |
| `internal/corpus` | user 模式内置文本和确定性窗口 |
| `internal/engine` | OpenAI 兼容客户端、SSE 解析、单轮计时和 probe |
| `internal/scenario` | user/rps/concurrency 调度、SLO 和场景观测 |
| `internal/smetrics` | `/metrics` counter、gauge、histogram 和 KV 画像 |
| `internal/report` | schema 结构、总 TPS 时间轴和 JSON 落盘 |

## 单轮采集

`engine.TurnMetrics` 是所有场景的核心样本。字段以 `docs/data-contract.md` 为准；
chunk 数、字符数、ITL、原始时间、回复文本和 reasoning 协议细节只保留在内存或 debug 中。

## 总 TPS 与进度

`report.BuildTotalTPS` 把每条成功流式请求投影为 `[sent_at + ttft_ms, end_at)`，
按一秒桶积分输出 token，并计算 `avg_decode_requests`。user 写入各档位
`user_levels[].throughput`，rps/concurrency 写入各档位 `concurrent[].total_tps[]`。

每完成一个档位，场景通过 `RunOptions.Checkpoint` 通知 CLI 写累计 checkpoint JSON；
rps/concurrency 同时打印请求进度，最终场景结束后再写正式 JSON。

## 服务端观测

`setupServerMetrics` 装配 `/metrics`，`startWindow` / `finishWindow` 负责窗口采集。
counter、gauge、histogram 只写场景或档位级 `server_metrics`。rps/concurrency 的
`source_check` 与 `server_metrics` 复用同一份结束快照；观测不可用时不改变客户端主口径。

## 场景与扩展

- `user.go`：profile 分派、确定性 corpus、真实 assistant 回复进入 history。
- `requests.go`：rps 开环到达与 concurrency 固定在飞，共享冻结请求集。
- `probe.go`：模型、usage、thinking、tool-call 和扩展 `/metrics` 能力探测。

新增单轮字段时同步 `docs/data-contract.md` 并递增 schema；新场景通过 scenario 注册表接入；
新服务端指标加入 `smetrics` 映射，未知命名明确告警，不静默套用错误语义。
