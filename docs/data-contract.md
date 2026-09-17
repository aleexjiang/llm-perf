# 数据契约：YAML → JSON（工具的对外接口）

> 面向数据分析者与下游工具：本工具产出的 JSON/CSV 结构与口径。
> **2026-09-17 拍板：工具专注数据采集，报告层已整体剥离**（原 gen_html_report.py 移除）——
> 这份契约就是本工具的对外接口，分析（聚合/判级/呈现）由外部完成（pandas/notebook/客户 BI）。
> 模块与代码组织见 [architecture.md](architecture.md)；判级阈值的方法论依据见 [latency-baselines.md](latency-baselines.md)。
>
> **本文不写行号**——行号随每次提交漂移（历史教训），定位一律用函数/类型名 grep。

## 版本

每份场景 JSON 顶层带 `schema_version`（当前 **3**，常量 `report.SchemaVersionCurrent`）。
结构变更时递增，消费方据此做兼容判断；schema 不保向后兼容（拍板见 AGENTS.md），大版本升级可能直接改字段类型。

## 数据流与落盘组织

```
bench -c config.yaml --turns X --concurrency Y
  └─ <output_dir>/<场景>-<时间戳>.json          # 单模型：直接落顶层
     <output_dir>/<模型>/<场景>-<时间戳>.json   # 多模型：PartitionByModel 按模型分区
     <output_dir>/run.log                       # 测试级追加日志（跨场景共享）
     <output_dir>/raw/*.log                     # debug:true 时的原始请求/响应转储
     <output_dir>/*.stall.csv                   # stall_guard 降速采样序列（侧文件，见下）
  └─ bench probe -c config.yaml -o probe.json
     └─ probe 结果是独立结构（ProbeResult），不与场景 Report 同构
```

## Go 侧输出：Report 顶层结构（report.go `Report`）

```
Report
├── schema_version  # 数据契约版本（恒落盘，见上）
├── tool / scenario / generated_at / test / endpoint / note
│                  #   test = 测试类别（benchmark | performance | soak），恒有值不带 omitempty
├── slo            # 本次 goodput 约束（config 配置了才填）：{ttft_ms, tpot_ms}
├── slo_baseline   # 体验基线三档阈值（配置了才填）：外部判级直接消费这份阈值，
│                  #   不要内置自己的常量（阈值随部署走，单一来源在配置/JSON）
├── plan           # 测试画像：开跑前的场景×模型估算（展示口径=执行口径）
├── multiturn[]    # MultiturnRun: model, thinking, session, max_tokens,
│                  #   profile（混合档位名，仅 multiturn.profiles 生效时出现）,
│                  #   turns[]→TurnMetrics, batch/start_offset_s（5.7 爬坡发车）,
│                  #   last_prompt_tokens/nominal_last_prompt（12.3 深度实测对照）
├── concurrent[]   # ConcurrentLevel: model, thinking, level, request_rate(开环>0),
│                  #   sessions[]→MultiturnRun（多轮会话，逐 turn 计量）,
│                  #   wall_seconds, throughput_tps（完整成功请求）, completed/failed/cancelled_requests,
│                  #   slo_meet/slo_total/goodput_rps/goodput_tps,
│                  #   waiting_max/running_max（观测峰值）, aborted（提前终止原因）,
│                  #   shapes[]→ShapeStat（concurrent.mix 形状分解，中位数）
├── correctness[]  # 金丝雀判定：{model, number, reply, match, e2e_ms, error}
├── auxiliary_requests[] # 不进入 benchmark KPI 但完整留存：phase=warmup/correctness，
│                  #   metrics=完整 TurnMetrics（含失败、取消、warnings、usage、原始时序）
├── server_metrics # 可选第二数据源（客户端实测才是基线）。窗口差值/轮询聚合：
│                  #   available（**仅指窗口差值 counter/hist 是否取到**）,
│                  #   note（取不到时的原因；全仓只有"结束快照失败"会写它）,
│                  #   cache_hit/query, spec_drafts/accepted,
│                  #   preemptions（**恒出现，刻意不带 omitempty**：0 = 窗口内没有发生抢占,
│                  #     这本身是有意义的好结果；键一消失就会被读成"这项没采到"）,
│                  #   gauges{}（轮询独立于结束快照，available=false 时仍可能有效）,
│                  #   histograms{}, observation_degraded（观测失效须醒目标注）
│                  #   缺失/取不到不得导致分析端少档位或改变判定
├── kv_capacity    # KV 静态容量画像（12.12，vllm:cache_config_info；缺失静默省略）
├── source_check   # 两源一致性（仅 benchmark 并发/RPS 窗口）：client_tps/server_tps/deviation
│                  #   warmup/correctness 已排除；多模型时是端点级合计参考，不是单模型精确归因
│                  #   deviation 恒出现：0 = 两源完全一致，是最有意义的好结果
├── environment    # 引擎识别存档（ProbeResult 轻量版）
├── config_raw     # 配置原文
└── stall_trace    # .stall.csv 侧文件相对路径（--no-stall-trace 或未触发熔断时缺失）
```

注意：`RequestRate>0` 即开环模式（Level=0）；`Level>0` 为闭环并发档位。
`.stall.csv` 列序 `t_s,agg_tps,med_tps,in_flight,emitting,phase`（phase ∈ emit/prefill/idle，
非 emit 相位速度留空——"测不出"≠0；`# tripped:` 注释行留熔断原因）。

## TurnMetrics 字段要点（client.go `TurnMetrics`）

所有场景行的叶子单元。四类字段语义不同，消费时必须区分：

| 类别 | 字段 | 消费规则 |
|---|---|---|
| 恒有 | model, stream, thinking, phase, sent_at, end_at, e2e_ms, prompt_tokens, completion_tokens, total_tokens, tokens_per_sec | `phase=benchmark` 为主压测；warmup/correctness 见 auxiliary_requests |
| 流式才有（omitempty） | ttft_ms, ttft_reasoning_ms, ttft_content_ms, think_ms, decode_ms, itl_*(p50/p90/p95/p99/max), tpot_ms, reasoning_tokens, cached_tokens, finish_reason, reasoning_field, new_tokens(多轮) | 非流式缺失；判级前先判存在 |
| 原始序列（raw_timings 开，默认开） | content_times_ms[]：每个 content chunk 相对 sent_at 的毫秒偏移 | **单调不减**；这是 chunk 到达时刻序列，可做 chunk 间隔抖动分析，不能无损重建逐 token 时间。体积随输出 token 数线性增长，超长 soak 可 `raw_timings: false` 关闭 |
| 质量标记 | thinking_no_content、stream_broken、cancelled、retry_count、warnings[] | 完整保留；分析主 KPI 前按 phase/error/cancelled 过滤 |
| 不进 JSON（json:"-"） | ToolCalls（probe 专用） | 压测数据永远看不到 |

口径提醒：TPOT = (E2E−TTFT)/(completion−1) **含思考 token**（GenAI-Perf 横评口径）；ITL 只算 content chunk 间隔——**ITL 是 chunk 间隔、不是 token 间隔**：投机解码（MTP）会把多个 token 合进同一个 SSE chunk，此时 chunk 间隔 ≈ N × token 间隔（vLLM+MTP 实测约 2.66×），拿 ITL 分位当 TPOT 会把延迟判高约 2.6 倍。**判级一律用 `tpot_ms`，不用 ITL 分位**；`new_tokens` 是本轮相对上一轮新增 prompt tokens，配合 TTFT 得增量 prefill 速率。

## 指标口径细则（对抗式审查沉淀，2026-09）

以下口径来自全链路审查后的拍板，消费数据前先读：

1. **tokens_per_sec 是双口径字段**：流式 = completion/(E2E−TTFT)——首 token 后的全部生成时段，**含思考段**，与 TPOT 同窗互逆（≈1000/TPOT）；非流式 = completion/e2e_ms（含 prefill+排队，天然偏低）。**两者不可横向比较**；非流式行的 ttft/think/itl 缺失即提示口径。
2. **失败判定唯一依据 `error` 字段**：断流（stream_broken）也写 error（"stream broken: …"）；`stream_broken` 只作补充标记。只看 stream_broken 会漏、只看 err 返回值会漏（attempt 返回 err=nil + 指标里的 Error）。
3. **吞吐是完整成功请求口径**：ThroughputTPS = `error==""` 且未主动取消请求的 completion_tokens 之和 ÷ 墙钟。失败/取消请求的完整原始指标仍落盘，但失败响应可能只有部分 usage，不能把不完整 token 混入主吞吐；档位同时落盘 completed/failed/cancelled 计数。
4. **SLOTotal 含服务端失败、不含主动取消**：已发出但服务端/网络失败的请求计入 SLO 分母（失败=不达标）；工具主动取消属于控制行为，不计入完成数或 SLO 分母。非流式模式下 TPOT 不可测 → 配置 goodput 时非流式全不达标。
5. **分位统一为线性插值**（2026-09-10 起）：ITL 分位（Go 侧 `percentile`）由最近秩 floor 取值改为线性插值，P50 偶数样本等于两中值平均——与分析侧 `median`、scenario 层 `aggregateShapes` 口径一致。此前同一报告内两者都叫 p99 但口径不同，现可比；与改动前的历史数据对比时 ITL 分位数会略升。
6. **think_ms 保证 ≥ 0**：reasoning 首包晚于 content 首包（引擎时序异常）时钳 0 并记 `think_ms_negative` 告警，原始时序在 first_*_at 时间戳可核查。
7. **usage 缺失的连锁**：服务端不回 usage 时 prompt/completion=0 + `usage_missing` 告警 → tokens_per_sec=0、TPOT 缺失、new_tokens 不更新（下轮会显示完整 prompt 而非增量）。有 usage_missing 告警的行，token 类指标全部不可信。
8. **服务端 counter 差分保证 ≥ 0**：负增量（服务端重启归零）钳 0——该窗口的命中率等指标可信度下降，应结合 preemptions/重启时间解读。NaN/±Inf 指标行在解析层直接丢弃（防 JSON 序列化失败）。
9. **直方图多 label 合并近似**：同 family 多 label（如按模型拆分）的 bucket 会合并计数，多模型共署引擎的直方图分位是粗估。
10. **TTFT 是"首个含 token chunk"口径**（2026-09 对齐主流）：role-only 空 content 首 chunk（OpenAI 兼容服务标配）不计入 TTFT，取 reasoning/content 首包较早者；原始首 chunk 时刻保留在 `first_chunk_at` 供核查。此版本前的落盘数据是"任意首 chunk"口径，数值略偏小（差 1 个空帧）。
11. **goodput 只判定已配置的 SLO 子集**（vLLM 语义）：阈值为 0 的维度不参与判定；配置了 TTFT 阈值时要求 TTFT 可测（>0，非流式不白拿达标）。
12. **content_chars/reasoning_chars 是字符数（rune）**：2026-09 起按字符计（此前是 UTF-8 字节数，中文单字被计为 3）。
13. **think_ms / decode_ms 缺失 ≠ 0 秒**（2026-09-10 起）：两者都带 omitempty。`thinking=on` 且 `thinking_no_content=true`（思考吃光输出预算）时思考段终点无从界定，缺失是**"测不出"而不是 0**，消费方不得当 0 参与中位数——否则一个档位里只要有部分 run 测不出，中位数就塌成 0（实测曾把 100k 档算成"思考 0.0s"，该档真实思考 142s）。`decode_ms` 在该情形下由采集端清 0（键消失），同一口径。
14. **`server_counter_delta` 是逐请求排障证据**：仅单发 / 串行多轮启用（并发恒缺——避免把共享计数器增量错记到单请求头上）；值 = 该请求抓取窗口内的服务端 counter 增量，窗口可能含周期性抓取或其他流量（近似对账，非精确归属）。属设计内原始存档。

## 开环到达（burstiness 与重整）

## 请求阶段与完整采集

所有场景请求都必须保留完整 `TurnMetrics`：主压测请求位于 `multiturn[]/concurrent[]`，预热与正确性金丝雀位于 `auxiliary_requests[]`。公共执行入口不再产生 `single[]`。
`phase` 只决定请求用途，不决定是否落盘；失败、主动取消、usage 缺失和 stream broken 都是可分析的原始数据。

- `warmup`：连接/首包预热，只不进入 benchmark KPI；
- `benchmark`：主压测数据，按档位统计；
- `correctness`：正确性判定请求，保留完整计时与 usage，但不进入性能 KPI；
- `cancelled=true`：客户端主动取消，保留原始记录，不计入完成数、主吞吐和 SLO 分母。

## 开环到达（burstiness 与重整）

开环模式（`request_rate`/`rate_sweep`）的到达调度（scenario.go `poissonDelays`）：

- 间隔 ~ Gamma(shape=`concurrent.burstiness`, scale=1/(rate·burstiness))：默认 1 = 标准泊松；<1 更突发；>1 趋向恒定间隔。
- **延迟重整**：采样后按理论总量 (n−1)/rate 整体缩放——不同 seed 的到达总量严格一致，吞吐数据跨 run 可比的前提（对齐 vLLM bench serve 的 normalize_factor）。
- 发射按**预生成的绝对时刻线**逐请求睡到点（补偿发射循环自身滞后），非"睡随机数再发"。
- 消费 side：开环档位的负载口径 = 到达率 × 墙钟内的请求数；分析容量拐点时先核对 `completed/failed/cancelled` 与应到请求数（积压时完成数可能在 drain 后才追平，不能只看最终完成率）。

## 主流口径对照（2026-09，对齐 GenAI-Perf/AIPerf、vLLM bench serve、LLMPerf、Inference-Perf）

| 指标 | 本工具 | 主流口径 | 结论 |
|---|---|---|---|
| TTFT | 首个含 token chunk（首 reasoning/content 较早者） | GenAI-Perf/LLMPerf/AIPerf/Inference-Perf 均"忽略空首响应"；vLLM 为首个流式输出 | ✅ 已对齐（空首 chunk 不算）；`first_chunk_at` 保留任意首帧 |
| TPOT | (E2E−TTFT)/(completion−1)，含思考 token | GenAI-Perf/vLLM/AIPerf/Inference-Perf 同式；LLMPerf 原生含 TTFT（历史差异） | ✅ 一致 |
| ITL | 相邻 content chunk 间隔，per-request 分位 | vLLM 池化所有请求 gap 后取分位；GenAI-Perf 为 per-response 值再聚合 | ⚠️ 有意差异：本工具是"单用户体验"视角（median-of-p99），与 vLLM 池化数值不可直接互比 |
| E2E | 发出 → 流读完（含 [DONE]/usage 尾帧到达） | GenAI-Perf 剔除末尾 [DONE] | ⚠️ 偏差 ≤1 个尾帧（毫秒级），本工具略偏保守，不改 |
| tokens_per_sec（per 请求） | 流式 = completion/(E2E−TTFT)，含思考段、不含 prefill | 行业 per-user TPS = output_tokens/e2e_latency（含 prefill） | ✅ 口径已贴近；与行业差一段 prefill，横比时行业值 ≈ completion/e2e_ms×1000 |
| 吞吐 ThroughputTPS | benchmark 档位内完整成功请求 completion 之和 ÷ 墙钟；warmup/correctness 不进入该档位 | vLLM/LLMPerf 同；GenAI-Perf 用首请求→末响应（略窄） | ✅ 客户端主口径；失败/取消计数另行落盘 |
| goodput | 达标请求数/墙钟 + 达标 token/墙钟 | vLLM：满足已配置 SLO 的成功请求/时长（req/s） | ✅ 对齐（子集语义见细则 11）；token 口径是本工具扩展 |
| cached_tokens | usage.prompt_tokens_details.cached_tokens | OpenAI 口径，vLLM 同名透传 | ✅ 一致 |
| 思考模型 TTFT | 首 reasoning chunk（= TTFTReasoning） | Neuron 的 llmperf_reasoning.patch 同口径 | ✅ 一致 |

参考：NVIDIA NIM Metrics / GenAI-Perf docs、vLLM `bench serve` 文档（"Metric terminology is not standardized across benchmarking tools…use the measurement points and formulas rather than the metric names alone"）、ray-project/llmperf、awslabs Inference-Perf 关键指标页。

## 演进规则（防漂移三条）

1. **schema 不保向后兼容**（决策见 AGENTS.md）：改字段直接改类型；大版本升级消费方按 `schema_version` 分流，不写兼容双轨。
2. **分析侧能算的不碰 Go**：聚合、分位、判级、呈现全部在工具之外；Go 只保证原始 per-request/per-turn 数据完整落盘（含 raw_timings 原始序列）。新增派生指标优先在外部做。
3. **契约文档单一来源**：字段/口径变更必须同步本文并递增 `SchemaVersionCurrent`——契约漂移比代码 bug 更伤（消费方不知道自己读错了什么）。
