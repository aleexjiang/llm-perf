# llm-perf 维护指南

## 项目边界

llm-perf 只负责采集自部署 LLM 服务的性能原始数据，Go 二进制输出 schema v8 JSON。
报告、分位统计、容量判定和可视化在工具外完成；`llm-perf-test/` 是本地真机测试工作区，
被 `.gitignore` 忽略，不把真实端点、密钥、业务数据和测试产物提交到仓库。

核心文档：

- [docs/data-contract.md](docs/data-contract.md)：JSON 数据契约。
- [docs/report-metrics.md](docs/report-metrics.md)：单轮指标和总 TPS 时间轴口径。
- [docs/architecture.md](docs/architecture.md)：模块边界。
- [docs/testing-architecture.md](docs/testing-architecture.md)：测试形态和控制变量。

## 命令

公共入口只有四个子命令：

```bash
./bench probe -c configs/example.yaml
./bench user -c configs/example.yaml
./bench rps -c configs/example.yaml
./bench concurrency -c configs/example.yaml
```

常用覆盖项：`-m`、`-o`、`--seed-salt`、`--thinking`、`--max-ctx`；`user` 还支持 `--users`。
场景参数放在 YAML，不为单次测试在 CLI 增加临时调度参数。

`user` 使用 `user.profile_path` 生成多轮动态会话；`rps` 和 `concurrency` 使用
`request_set.sharegpt_path` 的冻结请求集。两类负载的 cache 和时间行为不同，分析时不要混表。

真机运行顺序：先 `probe` 确认模型、认证、usage、思考能力和 `/metrics`，再跑目标场景。
重跑或切换思考模式时使用新的 `--seed-salt`。

## 当前数据模型

`TurnMetrics` 是一轮模型请求的核心样本，所有模式使用同一组单轮字段：

```text
sent_at / end_at
prompt_tokens / completion_tokens / reasoning_tokens / cached_tokens
ttft_ms / think_ms / tpot_ms / tokens_per_sec / e2e_ms
finish_reason / error / cancelled / warnings
```

单轮 TPS：

```text
tokens_per_sec = completion_tokens / ((e2e_ms - ttft_ms) / 1000)
```

总 TPS 是时间轴序列。每个成功请求的 decode 区间为
`[sent_at + ttft_ms, end_at)`；每个一秒点的 `tps` 是该秒中点正在 decode 的请求的
`tokens_per_sec` 之和，`decode_requests` 是请求数。输出字段为 `total_tps[]`。

服务端 `/metrics` 是独立的场景级观测源，只写入 `server_metrics`；客户端 completion token
与服务端 generation token 通过 `source_check` 对账，不把共享 counter 差值挂到单条请求。

## 数据删减原则

schema 不保留旧字段兼容逻辑。分析可以从单轮样本重算的派生值不进入 JSON：

- 不落 `total_tokens`、`throughput_tps`、`weighted_tps` 和旧 active-decode 聚合字段。
- 不落 `ttft_reasoning_ms`、`ttft_content_ms`、`reasoning_field`、`new_tokens`。
- 不落 chunk 计数、字符数、ITL、原始 chunk 时间和首帧时间；这些只在内存/debug 中使用。
- 不落逐请求 `/metrics` counter；服务端数据按场景窗口单独采集。

字段结构变化时直接更新 schema 和消费文档，递增 `SchemaVersionCurrent`，不同时落新旧字段。

## 负载与指标纪律

- 主指标是单轮 TTFT、TPOT、TPS、E2E、think_ms 和总 TPS 时间轴。
- 延迟和单轮 TPS 的主报告口径使用 P95，并同时报告样本数；P50 只作分布参考。
- `stop` 和 `length` 分层展示，不能混成一个结论。
- 失败、取消和 usage 缺失样本保留原始记录，但不进入成功样本聚合。
- `user` 的 session 只描述多轮上下文和 cache，不用 session TPS 代替单轮 TPS 或总 TPS。
- `running`、`waiting`、KV、cache 命中、preemption 和 source check 是诊断数据，不是新的性能指标。
- `probe` 的 tool-call 只验证协议能力，不进入压测样本。

## 修改纪律

- 先读现有实现和数据契约，再编辑；保持改动集中，不做无关重构。
- 手工编辑使用 `apply_patch`；默认 ASCII，新注释只解释非显然逻辑。
- 不回滚用户已有改动，不使用破坏性 Git 命令。
- 真机端点、密钥和输出只放 `llm-perf-test/` 或本地配置。

## 交付验证

代码或契约变化后执行：

```bash
gofmt -w internal/ cmd/
test -z "$(gofmt -l internal/ cmd/)"
go build ./...
go vet ./...
go test ./... -count=1
scripts/smoke.sh
```

提交前检查：

```bash
git diff --check
git status --short --branch
```
