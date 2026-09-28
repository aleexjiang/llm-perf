# llm-perf

客户自部署 LLM 推理服务性能采集工具。Go 编译为单二进制，无运行时依赖；输出 schema v9 JSON，
报告和容量分析由外部工具完成。

详细契约见 [docs/data-contract.md](docs/data-contract.md)，指标口径见
[docs/report-metrics.md](docs/report-metrics.md)。

## 场景

```bash
./bench probe -c configs/example.yaml
./bench user -c configs/example.yaml
./bench rps -c configs/example.yaml
./bench concurrency -c configs/example.yaml
```

| 场景 | 负载 | 用途 |
|---|---|---|
| `probe` | 最小能力请求 | 模型、认证、usage、thinking、tool-call、`/metrics` 探测 |
| `user` | profile 驱动的动态多轮会话 | 长上下文、prefix cache 和单轮体验 |
| `rps` | ShareGPT 冻结请求集、Poisson 到达 | 到达率、排队和体验拐点 |
| `concurrency` | ShareGPT 冻结请求集、固定在飞 | 并发、总 TPS 和饱和拐点 |

`user` 需要 `user.profile_path`；`rps` 和 `concurrency` 需要
`request_set.sharegpt_path`。它们的请求构成和 cache 行为不同，不混合解释。

`user.levels` 配置用户数阶梯；一次 `bench user` 会按配置顺序串行执行全部 user 档位，
每个档位独立落盘会话、总 TPS 和服务端观测。

常用 CLI 覆盖项：`-m`、`-o`、`--seed-salt`、`--thinking`、`--max-ctx`。

## 单轮指标

每条 `TurnMetrics` 是一轮模型请求的核心样本：

```text
sent_at / end_at
prompt_tokens / completion_tokens / reasoning_tokens / cached_tokens
ttft_ms / think_ms / tpot_ms / tokens_per_sec / e2e_ms
finish_reason / error / cancelled / warnings
```

核心公式：

```text
tpot_ms = (e2e_ms - ttft_ms) / (completion_tokens - 1)
tokens_per_sec = completion_tokens / ((e2e_ms - ttft_ms) / 1000)
```

`tokens_per_sec` 是首 token 之后的单轮平均输出速度；TTFT 包含排队和 prefill；E2E 包含完整请求时间。

## 总 TPS

成功请求的 decode 区间为：

```text
[sent_at + ttft_ms, end_at)
```

任意时刻的总 TPS 是该时刻正在 decode 的请求的 `tokens_per_sec` 之和。
JSON 以一秒时间轴落盘：

```json
{
  "second": 0,
  "decode_requests": 3,
  "tps": 612.4
}
```

user 放在 `throughput.total_tps[]`；rps/concurrency 放在各档位的 `total_tps[]`。
不使用 session 平均 TPS 推导总 TPS。

## 服务端观测

启用配置：

```yaml
server_metrics: true
```

`/metrics` 只按场景窗口独立采集到 `server_metrics`，用于 running/waiting、KV、cache、
preemption、服务端 token 和 histogram 归因。`source_check` 用服务端 generation token 与客户端
completion token 对账，不把共享 counter 差值挂到单条请求。

`/metrics` 不可用时，客户端单轮指标和总 TPS 仍然是主口径，不改变采集流程。

## 数据规则

- schema 结构变化直接更新版本，不保留旧字段兼容逻辑，不同时落新旧字段。
- `total_tokens`、全场景 `throughput_tps`、旧 active-decode 聚合、`weighted_tps` 不属于 schema v9。
- chunk 计数、字符数、ITL、原始 chunk 时间、首帧时间、思考协议字段只在内存或 debug 中使用。
- 失败、取消、usage 缺失和不完整流保留原始记录，但不进入成功聚合。
- `warmup` 和 `correctness` 进入 `auxiliary_requests[]`，不进入 benchmark KPI。
- 长场景运行中会打印请求进度；每完成一个 user level、RPS rate 或 concurrency level，
  会先写一个累计 checkpoint JSON，场景全部完成后再写最终 JSON。
- `stop` 和 `length` 分层；P95 是主报告口径，P50 只作分布参考。

## 构建与验证

```bash
make build
make test
gofmt -w internal/ cmd/
go build ./...
go vet ./...
go test ./... -count=1
scripts/smoke.sh
```

真机配置、二进制、原始 JSON、日志和分析记录统一放在 gitignored 的 `llm-perf-test/`。
客户端点和 API key 不提交到仓库。
