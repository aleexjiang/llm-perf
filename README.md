# llm-perf

自部署 OpenAI 兼容 LLM 服务的性能采集器。Go 单二进制，输出 schema v15 JSON；
报告、分位统计和容量判断由外部工具完成。

权威文档：

- [数据契约](docs/data-contract.md)：JSON 结构和字段语义。
- [指标口径](docs/metrics-semantics.md)：TTFT、TPOT、TPS 和总 TPS。
- [架构](docs/architecture.md)：模块边界和数据流。
- [测试架构](docs/testing-architecture.md)：负载、变量隔离和报告要求。

## 快速开始

```bash
make build
./bench probe       -c configs/example.yaml
./bench user        -c configs/example.yaml
./bench rps         -c configs/example.yaml
./bench concurrency -c configs/example.yaml
```

| 场景 | 负载 | 用途 |
|---|---|---|
| `probe` | 最小能力请求 | 模型、认证、usage、thinking、tool-call、`/metrics` |
| `user` | profile 驱动的动态多轮 | 长上下文、prefix cache、单轮体验 |
| `rps` | 冻结请求集、Poisson 到达 | 到达率、排队和体验拐点 |
| `concurrency` | 冻结请求集、固定在飞 | 总 TPS 和饱和拐点 |

`user` 需要 `user.profile_path`；`rps` / `concurrency` 需要
`request_set.sharegpt_path`。user 的动态 cache 与冻结请求集的 cache 行为必须分开解释。
profile 用 `scripts/profile_build.py --out <profile.json>` 生成受控的单调
light/medium/heavy 梯度。trace 只用于 `scripts/trace_shape.py --trace <trace>` 离线分析首轮、
后续增量和上下文突增分布，不支持回放。

首轮大小可在 YAML 中覆盖：

```yaml
user:
  first_turn_tokens: [16000, 16000]  # 首轮总 prompt
  shared_base_tokens: 8000           # 其中的共享 system 基座，默认 27000
```

首轮下限必须不小于 `shared_base_tokens + 最大用户输入`。首轮不参与上下文突增；后续轮按
profile 的 `context_burst_probability` 独立判定，并在常规 `context_tokens` 之外追加
`context_burst_tokens`。

常用覆盖项：`-m`、`-o`、`--seed-salt`、`--thinking`、`--max-ctx`。
重跑或切换 thinking 变体时使用新的 `--seed-salt`。

## 关键口径

每条 `TurnMetrics` 是一轮请求：

```text
ttft_ms = 首个含 token 的 reasoning/content chunk - sent_at
tpot_ms = (e2e_ms - ttft_ms) / (completion_tokens - 1)
tokens_per_sec = (completion_tokens - 1) / ((e2e_ms - ttft_ms) / 1000)
```

成功流式请求的 decode 区间为 `[sent_at + ttft_ms, end_at)`。总 TPS 使用统一的一秒桶积分：
首 token 计入所在桶，其余 token 按 decode 区间与桶的重叠时长分配。

`user.levels` 按配置顺序串行执行，每个档位独立保存 `metrics` 和 `server_metrics`。
每个 user 档位还记录实际生效的 `workload`，每个会话的 `input_plan[]` 与 `turns[]` 一一对应，
可直接统计突增轮。
`rps` / `concurrency` 的档位数据保存在 `concurrent[]`。

## 数据规则

- schema 变更直接递增 `schema_version`，不保留旧字段兼容逻辑。
- 失败、取消、usage 缺失和不完整流保留原始记录，但不进入成功聚合。
- `warmup` / `correctness` 只进入 `auxiliary_requests[]`，不进入 benchmark KPI。
- 长场景每完成一个档位写出累计 `*.checkpoint-NNN.json`，全部结束后写最终 JSON。
- `stop` 与 `length` 分层；延迟/TPOT 看 P95，TPS 低尾看 P5，P50 只作分布参考。

## 验证

```bash
gofmt -w internal/ cmd/
go build ./...
go vet ./...
go test ./... -count=1
scripts/smoke.sh
```

真机配置、二进制、原始 JSON、日志和分析记录统一放在 gitignored 的
`llm-perf-test/`。客户端点和 API key 不提交到仓库。
