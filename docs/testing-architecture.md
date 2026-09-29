# 测试架构

本文记录测试形态、负载梯度、变量隔离和验证命令。字段与公式分别见
[data-contract.md](data-contract.md) 和 [report-metrics.md](report-metrics.md)。

## 测量目标

工具采集单轮 TTFT、TPOT、TPS、E2E、think_ms，以及按时间轴计算的总 TPS。
正确性和数据对账用于确认采集可信。`running`、`waiting`、KV、prefix cache、
preemption 和 `/metrics` histogram 是诊断数据，不单独定义性能结论。

## 场景

| 场景 | 请求形态 | 主要问题 |
|---|---|---|
| `probe` | 最小能力请求 | 模型、usage、thinking、tool-call、`/metrics` 是否可用 |
| `user` | profile 驱动的动态多轮 | 长上下文、prefix cache、单轮体验 |
| `rps` | 冻结请求集的 Poisson 到达 | 到达率提升时何时排队、体验何时恶化 |
| `concurrency` | 冻结请求集的固定在飞 | 总 TPS 何时进入平台 |

三种压测场景的调度不同，但单轮字段和总 TPS 计算相同。

## 负载与变量隔离

- user profile 的 workload 必须单调：`light` 少轮次且每轮新增少，`medium` 居中，
  `heavy` 多轮次且每轮新增多。heavy 启用 `fill_context` 时持续到统一上下文预算的安全
  边界；light / medium 按自身轮次结束，不为填满模型上下文强行加轮。
- user 文本来自内置 corpus，assistant 回复使用被测模型真实输出并进入下一轮 history。
- rps/concurrency 使用 ShareGPT 冻结请求集，保证输入形状和样本顺序可复现。
- 比较服务端能力时固定模型、thinking、采样、输入形状和输出预算，只改变一个压力维度。
- user 的动态 cache 与冻结请求集的 cache 分开解释。
- `seed_salt` 用于重跑隔离 prefix cache；重跑或切换 thinking 变体时递增。
- thinking on/off 是独立变体，不把两种输出形状混成一个结论。
- 性能容量测试与长时间稳定性测试分开；稳定性测试关注失败、正确性和指标漂移。
- tool-call 只做 probe 能力检查，不进入吞吐压测。

## 阶梯停止

先用较粗档位定位拐点，再在拐点附近加密。总 TPS 连续两档不再增长或下降，且 P95 延迟、
TPOT 或服务端 `waiting` 明显恶化时，停止更高档位。已完成数据保留；报告记录饱和档位、
判定依据和未执行档位。

## 统计纪律

- 每个请求都保留 `TurnMetrics`，包括失败、取消和 warning；统计时按状态过滤。
- `stop` 与 `length` 分层。
- 延迟和 TPOT 以 P95 为主，单轮 TPS 低尾以 P5 为主，同时显示样本量；P50 描述典型值。
- 总 TPS 只用一秒桶积分并保持 token 守恒，不用中点抽样、session 平均 TPS 或请求数乘平均 TPS。
- `/metrics` 只按窗口采集，和客户端 completion token 做 `source_check`。
- 服务端观测缺失不得改变客户端主口径。

## 报告要求

外部报告至少展示：TTFT、TPOT、E2E 的 P95、TPS 的 P5/P50 和样本数；`total_tps[]` 与
平均 decode request count；成功/失败/取消/无效数量；prompt、completion、reasoning、cache
构成；以及 running/waiting、KV、cache、preemption 和 source check 诊断。

## 验证

```bash
gofmt -w internal/ cmd/
go build ./...
go vet ./...
go test ./... -count=1
scripts/smoke.sh
```
