# 场景选型指南：多轮 agent 数据采集

> 工具只负责采集原始数据，不做 HTML 报告、聚合或结论启发式。
> 公共执行入口只保留多轮 agent：单发多轮用于体验基线，多用户多轮用于 RPS/并发容量采集。
> 结果契约见 [data-contract.md](data-contract.md)，配置模板见 `configs/example.yaml`。

## 1. 产品边界

| 路径 | 用途 | 入口 |
|---|---|---|
| 单发多轮 | 观察 history 变深、prefix cache、每轮 TTFT/TPOT 变化 | `--concurrency 1` |
| 多用户多轮 RPS | 主容量采集；RPS 是新会话到达率，观察排队、失败、drain 和用户体验 | `--concurrency cfg` + `request_rate/rate_sweep` |
| 多用户多轮闭环 | 辅助诊断并发槽位和单流退化 | `--concurrency 2,4,8` |
| probe | 引擎、usage、thinking、tool-call、/metrics 能力检查 | `bench probe` |

**不再支持单发单轮。** 普通对话的单轮阶梯不能代表 agent 的长前缀、多轮 history 和缓存形状；需要短请求健康检查时使用 `probe`，不要恢复一个独立 benchmark 场景。

## 2. 推荐执行顺序

```text
probe（逐模型确认能力）
→ 单发多轮 off（确认 history/cache 基线）
→ 单发多轮 on 或 thinking.levels（确认思考成本）
→ RPS 多用户多轮 off（主容量曲线）
→ RPS 多用户多轮 on（线上确实有 thinking 流量时）
→ 闭环多用户多轮（需要定位槽位/并发退化时）
→ soak（需要长期稳定性证据时）
```

多个模型可以放在同一次测试配置中。工具会：

- 按模型顺序独立执行 warmup、benchmark、correctness；
- 每个请求携带 `phase`，模型数据按 `<output_dir>/<模型>/` 分区落盘；
- `probe` 一次接收 `models` 列表并逐模型采集能力结果；
- 不把一个模型的请求或 warmup 混入另一个模型的客户端数据；
- `server_metrics/source_check` 是端点级辅助数据，不应被解读成某个模型的精确归因，除非服务端指标自身带模型 label 且外部分析完成拆分。

## 3. 单发多轮

```bash
bench probe -c configs/customer.yaml
bench -c configs/customer.yaml --concurrency 1 --thinking off --seed-salt 1
bench -c configs/customer.yaml --concurrency 1 --thinking on --seed-salt 2
```

重点采集：

- 每轮实际 `prompt_tokens` 与 `new_tokens`；
- 每轮 `ttft_ms`、`tpot_ms`、`tokens_per_sec`；
- `cached_tokens`、服务端 cache counter（若可用）；
- usage 缺失、断流、超时和主动取消等异常原始记录。

## 4. RPS 多用户多轮（主路径）

配置示例：

```yaml
multiturn:
  sessions: 2
  turns: 8
  system_tokens: 18000
  tool_defs_tokens: 3000
  turn_tokens: 10000
  max_tokens: 256
  keep_assistant: true

concurrent:
  multiturn: true
  rate_sweep: [0.1, 0.25, 0.5, 1]
  num_prompts: 32       # 每档新会话数
  max_concurrency: 0
  burstiness: 1
```

语义：

- `request_rate`/`rate_sweep` 的单位是**新会话/s**，不是每轮请求/s；
- 新会话按到达时间启动，内部按多轮 history 继续执行；
- `num_prompts` 在多轮语义下表示会话数；
- 每档落盘 `request_rate`、实际会话/turn、`completed_requests`、`failed_requests`、`cancelled_requests`；
- 判断容量时不能只看最终完成率，要同时观察发射窗口内的完成速度、最后完成时间、waiting、TTFT P95/P99 和 drain 时长。

## 5. 思考模式

默认 `thinking.mode: both` 会对每个场景展开 off/on 两组数据。多个模型的 thinking 配置可以通过 `model_overrides.<model>.thinking` 字段级覆盖。

使用 `thinking.levels` 时：

- 档位名必须在不同模型配置中明确；
- `--thinking low` 按档位名过滤；
- `--thinking on/off` 不适用于 levels；
- 思考模式只改变请求变体，不改变多轮场景结构。

## 6. 数据与统计边界

采集和统计分离：

```text
所有请求完整落盘
  ├── benchmark：进入主场景原始数据
  ├── warmup：进入 auxiliary_requests，不进入 benchmark KPI
  ├── correctness：判定进 correctness，完整指标进 auxiliary_requests
  ├── failed：保留并进入失败计数
  └── cancelled：保留，但不进入完成数、主吞吐或 SLO 分母
```

`/metrics` 只做参考、诊断和必要的控制层观测，不替代客户端 TTFT、TPOT、吞吐和 goodput。

## 7. RPS 与闭环如何选择

- 需要回答“最多承载多少真实用户/请求到达”时，优先 RPS；
- 需要回答“并发槽位增长时单流速度如何退化”时，补闭环档；
- 不要把 RPS 和闭环 levels 的数字放到同一条横轴直接比较；它们的输入变量不同；
- `server_metrics` 缺失不影响客户端数据采集，外部分析标记 NA 即可。

## 8. 数据隔离纪律

- 每次测试递增 `seed_salt`，避免旧 prefix cache 污染冷启动；
- 多模型使用目录输出，不要用单个 `.json` 文件；
- warmup/correctness 也保留完整原始指标，但不要手工混入 benchmark 统计；
- 中断或熔断后的数据不要删除，利用 `aborted`、`cancelled`、`error` 和 `phase` 在外部分析时筛选；
- 任何外部分析都应先按 `schema_version`、`phase`、`error`、`cancelled` 过滤，再计算指标。
