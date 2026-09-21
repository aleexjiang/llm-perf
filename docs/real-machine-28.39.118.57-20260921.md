# 真机全量测试记录（2026-09-21，28.39.118.57）

> 环境：vLLM `qwen3.8-27b`（27B-FP8，`max_model_len=262144`），`http://28.39.118.57:8080/v1`。
> 前置代理带 `AIO-Forward` 响应头；`/v1/models` 与 chat 均无认证；`/metrics` 可用。
> KV 画像：池 1.51M tokens（fp8，block=1600，`gpu_memory_utilization=0.90`），
> 满上下文口径并发上界约 5.7 路。
>
> 工具版本：`9ade5d8` 构建（source_check 修复发生在本轮测试之后，未影响已采集数据；
> 修复内容见文末）。配置：`llm-perf-test/configs/selfhost-qwen3.8-27b-20260920.yaml`。
> 原始产物：`llm-perf-test/output-20260920-28.39.118.57/`。

## 测试设计

三轮全量，每轮覆盖 `probe`、`user`、`concurrency`、`rps`；轮间通过 `seed_salt=20/21/22`
隔离。`sampling.temperature=0` 固定输出轨迹；`request_set.num_prompts=100`；
`rps.rates=[3,4.5,6,8]`；`concurrency.levels=[1,8,32]`；user 为 4 用户 profile 会话。

三轮 probe 均为：标准面 10/10 通过，扩展面 5 项可用。部署默认开启思考，显式
`enable_thinking=false` 有效；tool-call 非流式/required/流式均正常且无标记泄漏。

## 客户端结果

### user（生成式多轮）

| 轮 | cache 命中率 | preemptions | 会话轮数 | 最深 prompt | 会话失败 |
|---|---:|---:|---|---:|---:|
| r1 | 81.0% | 0 | 4/7/4/15 | 245,170 | 1（ctx limit） |
| r2 | 79.0% | 0 | 4/5/3/13 | 240,947 | 1（ctx limit） |
| r3 | 82.5% | 0 | 4/5/3/16 | 251,412 | 1（ctx limit） |

- light/medium 轮次 TTFT 中位数大致在 7.5s-22s；heavy 长会话随并发 prefill 竞争波动。
- user 的动态 prefix cache 明显生效；三轮均无 preemption。
- r3 首个会话出现一次低质量重复回复（同一标题重复 3 次），为模型采样输出问题；
  `temperature=0` 下该轮仍在 `max_tokens` 内自然结束，工具未误判失败。

### concurrency（固定在飞，100 prompts/档）

| 档位 | 吞吐 tok/s | TTFT p50 | TTFT p95 | TPOT p50 |
|---:|---:|---:|---:|---:|
| 1 | 144/145/144 | 452-456ms | 515-562ms | 5.2ms |
| 8 | 403/406/407 | 534-587ms | 2.19-2.47s | 16.7ms |
| 32 | 528/530/532 | 990-1,094ms | 8.57-9.08s | 47.4-49.5ms |

三轮 300 次请求全部成功。8→32 并发吞吐只增加约 30%，但 TTFT p95 放大一个数量级，
饱和趋势与上一环境一致；本部署饱和吞吐约 530 tok/s。

### rps（开环到达，100 prompts/档）

| 到达率 | 吞吐 tok/s | TTFT p50 | TTFT p95 | TPOT p50 | 失败 |
|---:|---:|---:|---:|---:|---:|
| 3 req/s | 511-528 | 2.44-3.79s | 5.17-6.98s | 51.3-51.5ms | 0/300 |
| 4.5 req/s | 519-533 | 6.80-8.31s | 13.84-13.94s | 48.9-51.1ms | 0/300 |
| 6 req/s | 529-531 | 9.27-10.76s | 13.84-13.86s | 48.4-49.1ms | r1: 1/100 |
| 8 req/s | 528-532 | 10.99-12.35s | 13.95-14.20s | 48.5-49.3ms | 0/300 |

吞吐在 3 req/s 附近已贴近服务端能力；继续加压主要变成排队，TTFT p95 稳定劣化到约 14s。
若以 TTFT p95 5s 为体验目标，本环境可承诺的到达率约 3 req/s；放宽到 14s 可到 6-8 req/s。
本轮未配置 SLO 阈值，goodput 结论留待下一轮显式配置。

## 服务端观测

| 场景 | prefix cache 命中率 | preemptions | 投机解码 accepted/drafts |
|---|---:|---:|---:|
| user | 79.0%-82.5% | 0 | 约 2.0-2.3 token/draft |
| concurrency | 0.8%-1.2% | 0 | 约 2.0 token/draft |
| rps | 1.2% | 0 | 约 1.9-2.0 token/draft |

冻结 ShareGPT 快照间共享前缀很少，缓存命中率低是预期行为；user 真实生成链的
动态缓存命中率约 80%，两者不能混表。三轮 gauge 轮询均未降级；rps/concurrency 高压档位
的 `running` 峰值触到 32，`waiting` 峰值 32，说明服务端调度上限参与排队。

## 发现与处理

### 1. `rps/concurrency` 缺少 `source_check`（工具缺陷，已修复）

现象：三轮 6 份 rps/concurrency JSON 均无 `source_check` 字段，与
`docs/data-contract.md` 的承诺不一致。

根因：`applySourceCheck` 已实现，但 `RPSScenario` 和 `ConcurrencyScenario` 没有接线。
该函数此前没有任何调用方，现有测试无法暴露。

处理：为两个场景在场景窗口启动后挂载 `applySourceCheck`，并新增
`TestRequestScenariosSourceCheck` 回归测试；测试桩提供 `vllm:generation_tokens_total`。

### 2. `metrics_interval_ms=0` 会让 gauge 轮询 panic（工具缺陷，已修复）

现象：回归测试中未设置 `metrics_interval_ms`，`GaugePoller` 收到零间隔后调用
`time.NewTicker(0)` 直接 panic。真实配置显式写了 500ms，所以三轮测试未触发。

处理：`StartGaugePoller` 对非正值兜底为 500ms。未配置与非法配置不应让整个采集进程崩溃。

### 3. 网关瞬时 502（环境问题，已留痕）

r1/rps `rate=6` 有一请求返回 HTML 502，body 为代理页面而非 vLLM 错误结构。
该请求计入 `failed_requests=1`，其余 99 条正常；r2/r3 同档位 0 失败。
判断为网关/代理瞬时故障。JSON 中已保留完整错误与计数，后续分析应按 `error` 过滤。

### 4. user 长会话上下文边界未预留输出预算（工具设计问题，已修复）

三轮 heavy 会话最后都在同一位置失败：服务端返回
`prompt contains at least 261889 input tokens`，请求还要求 256 个输出 token，
总数超过 `max_model_len=262144`。工具能识别“模型上下文上限”并安全终止会话，但
构造下一轮 prompt 时只对齐了 prompt 上限，没有为 `max_tokens` 预留空间。

处理：`max_prompt_tokens` 现在按“单请求 prompt + output 总预算”执行。user 在构造每轮
请求前用上一轮实测历史加 assistant 回复、再叠加本轮计划 user/context 增量估算 prompt，
并预留本变体的 `max_tokens`；估算超过预算时直接记 `token_budget_exhausted` 并提前止损，
不再发出确定性 400。`token_budget` 随 `MultiturnRun` 落盘，数据契约版本升到 5。
该修复通过 `TestUserScenarioStopsWithinTokenBudget` 回归测试。

### 5. 配置路径相对语义需注意（使用问题，已修正本次配置）

`user.profile_path` 与 `request_set.sharegpt_path` 相对配置文件目录解析。
本次初版配置按仓库根目录写路径，user 在发出任何请求前失败一次；改为
`../profiles/...`、`../ShareGPT...` 后正常。该行为本身一致，但容易踩错；
后续可考虑在配置错误提示里输出“当前解析基准目录”。

## 结论

1. 新环境功能健康：probe、user、concurrency、rps 三轮可重复执行，除一次代理 502 外无失败。
2. 容量画像稳定：饱和吞吐约 530 tok/s；rps 3 req/s 时 TTFT p95 5-7s，
   4.5 req/s 以上 TTFT p95 达到 14s；建议 3-4.5 req/s 作为进一步 goodput 承诺评估区间。
3. user 动态缓存命中率约 80%，rps/concurrency 冻结快照约 1%，两者口径应继续分开。
4. 本轮发现并修复了 `source_check` 未接线和 gauge 零间隔 panic；context 输出预算问题已记录待修。
