# 数据契约：YAML → JSON → 报告

> 面向报告侧开发者与数据分析者：Go 产出的 JSON 结构、报告侧聚合口径、两边如何对齐不漂移。
> 模块与代码组织见 [architecture.md](architecture.md)；判级阈值的来源依据见 [latency-baselines.md](latency-baselines.md)。

## 数据流与落盘组织

```
bench -c config.yaml --turns X --concurrency Y
  └─ <output_dir>/<场景>-<时间戳>.json          # 单模型：直接落顶层
     <output_dir>/<模型>/<场景>-<时间戳>.json   # 多模型：PartitionByModel 按模型分区
     <output_dir>/run.log                       # 测试级追加日志（跨场景共享）
     <output_dir>/raw/*.log                     # debug:true 时的原始请求/响应转储
  ▼
gen_html_report.py f1.json f2.json ... [标题]
  ├─ <output_dir>/report.html                   # 自包含 HTML（图表内嵌）
  └─ <output_dir>/perf-summary.json             # 聚合数据 + 结论，供 AI/下游消费
```

- 一次调用可传多份 JSON（merge 逻辑按模型×场景×思考模式分桶合并 runs）；同名键冲突时合并而非覆盖。
- probe 结果是独立结构（ProbeResult），不与场景 Report 同构，落 `<模型>/probe-*.json`。

## Go 侧输出：Report 顶层结构（report.go:118）

```
Report
├── tool / scenario / generated_at / endpoint / note
├── slo            # 本次 goodput 约束（config 配置了才填）：{ttft_ms, tpot_ms}
├── single[]       # SingleRow: model, thinking, prompt_tokens, max_tokens, runs[]→TurnMetrics
├── multiturn[]    # MultiturnRun: model, thinking, session, max_tokens, turns[]→TurnMetrics
├── concurrent[]   # ConcurrentLevel: model, thinking, level, request_rate(开环>0),
│                  #   requests[]→TurnMetrics（单轮）或 sessions[]→MultiturnRun（多轮会话，逐 turn 计量）,
│                  #   wall_seconds, throughput_tps, slo_meet/slo_total/goodput_rps/goodput_tps,
│                  #   shapes[]→ShapeStat（concurrent.mix 形状分解，中位数）
├── correctness[]  # 金丝雀：{model, number, reply, match, e2e_ms, error}
├── server_metrics # 可选第二数据源（客户端实测才是基线）。窗口差值/轮询聚合：
│                  #   available（**仅指窗口差值 counter/hist 是否取到**）,
│                  #   note（取不到时的原因；全仓只有"结束快照失败"会写它）,
│                  #   cache_hit/query, preemptions, spec_drafts/accepted,
│                  #   gauges{}（轮询独立于结束快照，available=false 时仍可能有效）,
│                  #   histograms{}, observation_degraded（观测失效须醒目标注）
│                  #   缺失/取不到不得导致少结论、漏档位或改变判定
├── environment    # 引擎识别存档（ProbeResult 轻量版）
└── config_raw     # 配置原文
```

注意：`RequestRate>0` 即开环模式（Level=0）；`Level>0` 为闭环并发档位。

## TurnMetrics 字段要点（client.go:102）

所有场景行的叶子单元。三类字段语义不同，消费时必须区分：

| 类别 | 字段 | 消费规则 |
|---|---|---|
| 恒有 | model, stream, thinking, sent_at, end_at, e2e_ms, prompt_tokens, completion_tokens, total_tokens, tokens_per_sec | 直接用 |
| 流式才有（omitempty） | ttft_ms, ttft_reasoning_ms, ttft_content_ms, think_ms, decode_ms, itl_*(p50/p90/p95/p99/max), tpot_ms, reasoning_tokens, cached_tokens, finish_reason, reasoning_field, new_tokens(多轮) | 非流式缺失；TTFT 徽章判级前先判存在 |
| 不进 JSON（json:"-"） | ToolCalls（probe 专用） | 报告侧永远看不到，别指望 |
| 质量标记 | thinking_no_content（思考吃光预算，剔除或调 max_tokens）、stream_broken（响应不完整）、retry_count、warnings[] | 分析前先过滤 |

口径提醒：TPOT = (E2E−TTFT)/(completion−1) **含思考 token**（GenAI-Perf 横评口径）；ITL 只算 content chunk 间隔；`new_tokens` 是本轮相对上一轮新增 prompt tokens，配合 TTFT 得增量 prefill 速率。

## 指标口径细则（对抗式审查沉淀，2026-09）

以下口径来自全链路审查后的拍板，消费数据前先读：

1. **tokens_per_sec 是双口径字段**：流式 = completion/(E2E−TTFT)——首 token 后的全部生成时段，**含思考段**，与 TPOT 同窗互逆（≈1000/TPOT；2026-09 修正：此前分母是 decode_ms 仅覆盖 content 时段，而 completion 含 reasoning token，思考模型 tok/s 被显著虚高）；非流式 = completion/e2e_ms（含 prefill+排队，天然偏低）。**两者不可横向比较**；非流式行的 ttft/think/itl 缺失即提示口径。
2. **失败判定唯一依据 `error` 字段**：断流（stream_broken）也写 error（"stream broken: …"）；`stream_broken` 只作补充标记。只看 stream_broken 会漏、只看 err 返回值会漏（attempt 返回 err=nil + 指标里的 Error）。
3. **吞吐是物理口径**：ThroughputTPS = 全部请求（含失败）的 completion_tokens 之和 ÷ 墙钟。失败请求 0 产出但占墙钟——吞吐低可能是失败拖累而非 decode 慢，解读时先看失败数。Goodput ≤ Throughput 恒成立。
4. **SLOTotal 含失败请求**：达标率分母 = 全部请求（失败=不达标）。非流式模式下 TPOT 不可测 → 配置 goodput 时非流式全不达标（设计如此，别用非流式测 goodput）。
5. **分位统一为线性插值**（2026-09-10 起）：ITL 分位（Go 侧 `percentile`）由最近秩 floor 取值改为线性插值，P50 偶数样本等于两中值平均——与报告侧 `st.median`、scenario 层 `aggregateShapes` 口径一致。此前同一报告内两者都叫 p99 但口径不同，现可比；与改动前的历史报告对比时 ITL 分位数会略升。
6. **think_ms 保证 ≥ 0**：reasoning 首包晚于 content 首包（引擎时序异常）时钳 0 并记 `think_ms_negative` 告警，原始时序在 first_*_at 时间戳可核查。
7. **usage 缺失的连锁**：服务端不回 usage 时 prompt/completion=0 + `usage_missing` 告警 → tokens_per_sec=0、TPOT 缺失、new_tokens 不更新（下轮会显示完整 prompt 而非增量）。有 usage_missing 告警的行，token 类指标全部不可信。
8. **服务端 counter 差分保证 ≥ 0**：负增量（服务端重启归零）钳 0——该窗口的命中率等指标可信度下降，应结合 preemptions/重启时间解读。NaN/±Inf 指标行在解析层直接丢弃（防 JSON 序列化失败）。
9. **直方图多 label 合并近似**：同 family 多 label（如按模型拆分）的 bucket 会合并计数，多模型共署引擎的直方图分位是粗估。
10. **TTFT 是"首个含 token chunk"口径**（2026-09 对齐主流）：role-only 空 content 首 chunk（OpenAI 兼容服务标配）不计入 TTFT，取 reasoning/content 首包较早者；原始首 chunk 时刻保留在 `first_chunk_at` 供核查。此版本前的落盘数据是"任意首 chunk"口径，数值略偏小（差 1 个空帧）。
11. **goodput 只判定已配置的 SLO 子集**（vLLM 语义）：阈值为 0 的维度不参与判定；配置了 TTFT 阈值时要求 TTFT 可测（>0，非流式不白拿达标）。
12. **content_chars/reasoning_chars 是字符数（rune）**：2026-09 起按字符计（此前是 UTF-8 字节数，中文单字被计为 3）；报告"思考字符"列、日志"N 字"同步。

## 主流口径对照（2026-09，对齐 GenAI-Perf/AIPerf、vLLM bench serve、LLMPerf、Inference-Perf）

| 指标 | 本工具 | 主流口径 | 结论 |
|---|---|---|---|
| TTFT | 首个含 token chunk（首 reasoning/content 较早者） | GenAI-Perf/LLMPerf/AIPerf/Inference-Perf 均"忽略空首响应"；vLLM 为首个流式输出 | ✅ 已对齐（空首 chunk 不算）；`first_chunk_at` 保留任意首帧 |
| TPOT | (E2E−TTFT)/(completion−1)，含思考 token | GenAI-Perf/vLLM/AIPerf/Inference-Perf 同式；LLMPerf 原生含 TTFT（历史差异，AWS 也要打 patch 修掉） | ✅ 一致 |
| ITL | 相邻 content chunk 间隔，per-request 分位，报告层对请求取中位 | vLLM 池化所有请求 gap 后取分位；GenAI-Perf 为 per-response 值再聚合 | ⚠️ 有意差异：本工具是"单用户体验"视角（median-of-p99），与 vLLM 池化数值不可直接互比 |
| E2E | 发出 → 流读完（含 [DONE]/usage 尾帧到达） | GenAI-Perf 剔除末尾 [DONE] | ⚠️ 偏差 ≤1 个尾帧（毫秒级），本工具略偏保守，不改 |
| tokens_per_sec（per 请求） | 流式 = completion/(E2E−TTFT)，含思考段、不含 prefill；非流式 = completion/e2e | 行业 per-user TPS = output_tokens/e2e_latency（含 prefill） | ✅ 口径已贴近（2026-09 修正思考模型虚高问题）；与行业差一段 prefill，横评时行业值 ≈ completion/e2e_ms×1000 |
| 吞吐 ThroughputTPS | 全部请求 completion 之和 ÷ 墙钟（首请求发射前 → 全部完成；warmup 不计入） | vLLM/LLMPerf 同；GenAI-Perf 用 Ty−Tx（首请求→末响应，略窄）；Inference-Perf 滑窗剔除 warmup/cooldown | ✅ 一致（物理口径，失败请求占墙钟见细则 3） |
| goodput | 达标请求数/墙钟 + 达标 token/墙钟 | vLLM：满足已配置 SLO 的成功请求/时长（req/s） | ✅ 对齐（子集语义见细则 11）；token 口径是本工具扩展 |
| cached_tokens | usage.prompt_tokens_details.cached_tokens | OpenAI 口径，vLLM 同名透传 | ✅ 一致 |
| 思考模型 TTFT | 首 reasoning chunk（= TTFTReasoning） | Neuron 的 llmperf_reasoning.patch 同口径 | ✅ 一致 |

参考：NVIDIA NIM Metrics / GenAI-Perf docs、vLLM `bench serve` 文档（"Metric terminology is not standardized across benchmarking tools…use the measurement points and formulas rather than the metric names alone"）、ray-project/llmperf、awslabs Inference-Perf 关键指标页。

## 报告侧聚合口径（gen_html_report.py）

| 口径 | 规则 | 位置 |
|---|---|---|
| 并发表中位/min/max | mmm()，中位数；偶数个样本取两中值平均 | mmm(135) |
| p95/p99 | 线性插值（numpy 默认口径）；**样本 <20（MIN_PCT_SAMPLE=145 行常量）不输出分位**，退回中位 | pct9599(165) |
| 前缀缓存判级 | 按 prompt_tokens 与 cached_tokens 关系分类 | classify_cache(219) |
| 增量 prefill 斜率 | TTFT 对 new_tokens 的线性拟合，ms/千新 token | slope_ms_per_token(204) |
| 混跑形状聚合 | 按 label×weight 分桶取中位；撞 key 合并 runs | shapes_table(751) |

### 体验基线判级（第 8 节，SLO_TIERS=150 行）

- **3 档制归组**：输入 ≤4K 打档1/2；≥24K 打档3（agent 大上下文）；4–24K 不判级。
- **判级方向**：TTFT/TPOT 是低优口径（`badge`：≤good ✅ / ≤pass ⚠️ / 超 ❌）；tok/s 是**高优口径（`badge_hi`）**——加新指标时先想清楚方向，别混用。
- TTFT 优先取 p99（样本 <20 退中位）；**thinking=on 的 TTFT 是 TTFAT 口径（含思考时长），不套 TTFT 徽章**（TPOT/tok/s 仍判级）。
- 阈值内置在 SLO_TIERS 常量：档1 优 0.45s/档2 及格 2s/档3 优 3s 及格 6s；TPOT 40/200ms；tok/s 25/10。未来由 `slo:` 配置段覆盖（ROADMAP 5.8 待做）。
- 基线评估结果写进 perf-summary 的 `baseline` 数组（评估单元含场景/模型/档位/判级/证据），**不进退出码**——判级是参考结论不是门禁。

## perf-summary.json 结构（summary_json=1227 行）

```
{ about, endpoint, tool, generated_at,
  coverage,          # 请求数等覆盖度
  baseline[],        # 5.8 基线评估单元
  events{},          # 告警事件计数
  per_model{},       # 按模型的聚合明细（浮点保留 3 位）
  conclusions[], recommendations[{priority,text}], limitations[] }
```

HTML 里内嵌同内容 `<script type="application/json" id="perf-summary">` 块，AI/下游工具可直接从 HTML 提取，不必单独传文件。

## 演进规则（防漂移三条）

1. **schema 不保向后兼容**（决策见 AGENTS.md）：改字段直接改类型，报告侧同步改；不写兼容双轨。
2. **报告侧能算的不碰 Go**：聚合、分位、判级全部在 Python 侧；Go 只保证原始 per-request/per-turn 数据完整落盘。新增派生指标优先在报告侧做。
3. **判级阈值单一来源**：所有阈值收敛在 SLO_TIERS；改基准值同步更新 latency-baselines.md 的依据引用。
