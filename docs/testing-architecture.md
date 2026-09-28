# 测试架构

本文只记录当前实现的测试形态和测量原则。数据字段与公式见
[data-contract.md](data-contract.md) 和 [report-metrics.md](report-metrics.md)。

## 测量目标

工具围绕模型输出速度和服务吞吐采集数据：

1. 单轮 TTFT：多久开始输出。
2. 单轮 TPOT：输出 token 的平均间隔。
3. 单轮 TPS：首 token 后的平均输出速度。
4. 总 TPS：时间轴上同时 decode 的请求 TPS 之和。
5. 正确性和数据对账：确认采集结果可信。

`running`、`waiting`、KV、prefix cache、preemption 和 `/metrics` histogram 都是诊断数据，
用于解释主指标变化，不单独定义新的性能结论。

## 场景

| 场景 | 请求形态 | 主要问题 |
|---|---|---|
| `probe` | 最小能力请求 | 模型、usage、thinking、tool-call、`/metrics` 是否可用 |
| `user` | profile 驱动的动态多轮 | 长上下文、prefix cache 和单轮体验如何变化 |
| `rps` | 冻结请求集的 Poisson 到达 | 到达率提升时何时开始排队、体验何时恶化 |
| `concurrency` | 冻结请求集的固定在飞 | 请求数提升时总 TPS 何时进入平台 |

`user`、`rps`、`concurrency` 的请求调度不同，但单轮字段和总 TPS 计算完全相同。

## 负载

- user：profile 提供 light/medium/heavy 的轮次和上下文形状，文本来自内置 corpus；assistant 回复使用被测模型真实输出，继续进入下一轮 history。
- rps/concurrency：使用 ShareGPT 冻结请求集，保证输入形状和样本顺序可复现。
- `seed_salt` 用于重跑隔离 prefix cache；重跑或切换 thinking 模式时递增。
- thinking on/off 是独立测试变体，不把两种输出形状混成一个结论。

## 统计纪律

- 每个请求都保留 `TurnMetrics`，包括失败、取消和 warning；统计时按状态过滤。
- `stop` 与 `length` 分层报告。
- 延迟和单轮 TPS 以 P95 为主，必须同时显示样本量；P50 只描述分布。
- 总 TPS 使用每秒时间轴，不用 session 平均 TPS、不用请求数乘平均 TPS。
- `/metrics` 只按场景窗口采集，和客户端 completion token 做 source check 对账。
- 不能用服务端观测缺失改变客户端主口径。

## 变量隔离

- 需要比较服务端能力时固定模型、thinking、采样、输入形状和输出预算，只改变一个压力维度。
- user 的动态 cache 和 rps/concurrency 的冻结 cache 分开解释。
- 性能容量测试与长时间稳定性测试分开运行；稳定性测试关注失败、正确性和指标漂移。
- tool-call 只作为 probe 能力检查，不进入当前模型输出速度压测。

## 报告输出

外部报告至少展示：

- 单轮 TTFT、TPOT、TPS、E2E 的 P95 和样本数；
- `total_tps[]` 时间序列及 decode request count；
- 成功/失败/取消数量；
- prompt、completion、reasoning、cache 负载构成；
- 服务端 running/waiting、KV、cache、preemption 和 source check 诊断。

## 验证

代码和契约变更后执行：

```bash
gofmt -w internal/ cmd/
go build ./...
go vet ./...
go test ./... -count=1
scripts/smoke.sh
```
