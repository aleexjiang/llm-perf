# llm-perf 维护指南

## 项目边界

llm-perf 只负责采集自部署 LLM 服务的性能原始数据，Go 二进制输出 schema v15 JSON。
报告、分位统计、容量判定和可视化在工具外完成。真机配置、端点、密钥、原始产物和
分析记录只放 gitignored 的 `llm-perf-test/`。

权威文档：

- [docs/data-contract.md](docs/data-contract.md)：JSON 结构和字段语义。
- [docs/metrics-semantics.md](docs/metrics-semantics.md)：TTFT、TPOT、TPS 和总 TPS。
- [docs/architecture.md](docs/architecture.md)：模块边界。
- [docs/testing-architecture.md](docs/testing-architecture.md)：测试形态和控制变量。

## 命令与场景

公共入口只有四个子命令：

```bash
./bench probe       -c configs/example.yaml
./bench user        -c configs/example.yaml
./bench rps         -c configs/example.yaml
./bench concurrency -c configs/example.yaml
```

常用覆盖项：`-m`、`-o`、`--seed-salt`、`--thinking`、`--max-ctx`。
场景参数放在 YAML，不为单次测试在 CLI 增加临时调度参数。

`user` 使用 profile 和 `user.levels` 生成多轮动态会话阶梯；`rps` / `concurrency`
使用 `request_set.sharegpt_path` 的冻结请求集。两类负载的 cache 和时间行为不同，
不要混表。真机运行先 `probe`，再跑目标场景；重跑或切换 thinking 变体时换
`--seed-salt`。

user profile 用 `scripts/profile_build.py` 生成受控梯度；trace 只用
`scripts/trace_shape.py` 离线分析请求形状，不回放、不直接生成 profile。首轮大小梯度用
YAML 的 `user.first_turn_tokens` 和 `user.shared_base_tokens`，不要复制多份只改首轮的 profile。

## 阶梯与负载纪律

`user.levels`、`rps.rates`、`concurrency.levels` 都是 YAML 驱动的阶梯。先用较粗档位
定位拐点，再在拐点附近加密。若总 TPS 连续两档不再增长或下降，同时 P95 延迟、
TPOT 或服务端 `waiting` 明显恶化，则停止更高档位；已完成数据保留，报告记录饱和档位、
判定依据和未执行档位。

user profile 的 workload 必须单调：`light` 少轮次且每轮新增少，`medium` 居中，
`heavy` 多轮次且每轮新增多。普通轮通常只有短输入；`@文件`、粘贴日志等大增量用
`context_burst_probability` 和 `context_burst_tokens` 表达。所有档位按自身轮次结束，并由
`context_budget_tokens` 统一止损，不为填满上下文强行加轮。

## 数据规则

- schema 变更直接递增 `SchemaVersionCurrent`，不保留旧字段兼容逻辑。
- 失败、取消、usage 缺失和不完整流保留原始记录，但不进入成功聚合。
- `stop` 和 `length` 分层；延迟/TPOT 看 P95，TPS 低尾看 P5，P50 只作分布参考。
- 总 TPS 使用一秒桶积分并保持 token 守恒；不使用中点抽样、session 平均 TPS 或请求数乘单轮平均值。
- `/metrics`、`running`、`waiting`、KV、cache 和 source check 是诊断数据，
  不改变客户端单轮指标和总 TPS 主口径。
- `probe` 的 tool-call 只验证协议能力，不进入压测样本。
- 长场景必须支持进度日志和档位 checkpoint。

字段细节以 `docs/data-contract.md` 为准；不要在入口文档复制字段清单。

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

## 修改纪律

- 先读实现和数据契约再编辑；保持改动集中，不做无关重构。
- 手工编辑用 `apply_patch`；默认 ASCII，新注释只解释非显然逻辑。
- 不回滚用户已有改动，不使用破坏性 Git 命令。
- 真机端点、密钥和输出只放 `llm-perf-test/` 或本地配置。
