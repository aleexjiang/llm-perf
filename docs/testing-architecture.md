# 测试架构

本文记录测试形态、负载梯度、变量隔离和验证命令。字段与公式分别见
[data-contract.md](data-contract.md) 和 [metrics-semantics.md](metrics-semantics.md)。

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

- user profile 使用上下文突增语义：首轮由 `first_turn_tokens` 控制总 prompt（YAML 的
  `user.first_turn_tokens` 可覆盖 profile；`user.shared_base_tokens` 控制其中的 system 基座，
  默认 27000）；后续普通轮由 `context_tokens` 控制常规新增，每轮按 `context_burst_probability` 独立判定是否成为突增轮，
  命中时在常规新增之外追加 `context_burst_tokens`，模拟 `@文件`、粘贴日志等少数轮。
  profile 拒绝未知字段；所有档位由 `context_budget_tokens` 统一止损，
  不需要 `fill_context` 特殊逻辑。
- trace 只做离线形状分析：用 `scripts/trace_shape.py` 统计轮次、首轮 prompt、用户输入、
  后续增量和突增概率，再人工设定受控 profile 与 YAML 参数。工具不回放 trace 消息，也不从 trace
  自动生成 profile。
- 首轮大小对比只改 `user.first_turn_tokens` 和 `user.shared_base_tokens`，固定 profile、
  users、输出预算和 thinking。报告必须写明实际首轮区间和基座大小。
- `sessions[].input_plan[]` 记录每轮计划输入。分析上下文突增时，按 `context_burst=true/false`
  拆分 TTFT，而不是只看 prompt 跳变。
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
- `source_check.deviation` 只有在 `server_tokens > 0` 且 `note` 为空时才可作为一致性结论。
- 服务端观测缺失不得改变客户端主口径。

## 报告要求

外部分析至少展示：请求 token 形状分布；user 的 `workload` 与突增轮数；「每秒总吞吐」的
桶均值/P95/峰值；TTFT、TPOT、
E2E 的 P95、TPS 的 P5/P50 和样本数；`bucket_tps[]` 与平均 decode request count；
成功/失败/取消/无效数量；prompt、completion、reasoning、cache 构成；以及
running/waiting、KV、cache、preemption 和 source check 诊断。

## 验证

```bash
gofmt -w internal/ cmd/
go build ./...
go vet ./...
go test ./... -count=1
scripts/smoke.sh
```
