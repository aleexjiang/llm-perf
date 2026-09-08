# 数据契约：YAML → JSON → 报告

> 面向报告侧开发者与数据分析者：Go 产出的 JSON 结构、报告侧聚合口径、两边如何对齐不漂移。
> 模块与代码组织见 [architecture.md](architecture.md)；判级阈值的来源依据见 [latency-baselines.md](latency-baselines.md)。

## 数据流与落盘组织

```
bench -c config.yaml --turns X --concurrency Y
  └─ <output_dir>/<场景>-<时间戳>.json          # 单模型：直接落顶层
     <output_dir>/<模型>/<场景>-<时间戳>.json   # 多模型：PartitionByModel 按模型分区
     <output_dir>/run.log                       # 战役级追加日志（跨场景共享）
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
│                  #   requests[]→TurnMetrics（单轮）或 sessions[]→MultiturnRun（多轮重放）,
│                  #   wall_seconds, throughput_tps, slo_meet/slo_total/goodput_rps/goodput_tps,
│                  #   shapes[]→ShapeStat（concurrent.mix 形状分解，中位数）
├── correctness[]  # 金丝雀：{model, number, reply, match, e2e_ms, error}
├── server_metrics # 窗口差值/轮询聚合：cache_hit/query, preemptions, spec_drafts/accepted,
│                  #   gauges{}, histograms{}, observation_degraded（观测失效须醒目标注）
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
