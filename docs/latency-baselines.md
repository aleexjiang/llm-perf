# LLM 推理体验基线参考

本文为报告中的“体验基线评估”提供有出处的判据。核心数字于 2026-09-08 从原始页面核实；
补充参考一节未逐字核实，引用前先查原文。

## 权威锚点

### MLPerf / MLCommons

| 档位 | 输入/模型 | TTFT p99 | TPOT p99 |
|---|---|---|---|
| Server | Llama2-70B | ≤ 2s | ≤ 200ms |
| Interactive | Llama2-Chat-70B | ≤ 450ms | ≤ 40ms |
| 长上下文 | Llama3.1-405B，128K | ≤ 6s | ≤ 175ms |

MLPerf Server 的约束以阅读速度为锚：

> "Using the typical human reading speed as a logical anchor, these latency constraints were set as:
> **TTFT: <= 2 seconds, TPOT: <= 200 milliseconds**."

Interactive 档来自 ChatGPT / Perplexity 实测和用户调研：

> "a 50th percentile token generation rate of **20–50 tokens per second (TPOT of 20-50ms)
> is critical for seamless user experience**."

长上下文档说明基线会随模型和上下文变化：

> "we set a 99th percentile TTFT of **6 seconds** and a 99th percentile TPOT of **175ms**."

来源：

- [Llama 2 70B MLPerf](https://mlcommons.org/2024/03/mlperf-llama2-70b/)
- [MLPerf Inference v5.0](https://mlcommons.org/2025/04/llm-inference-v5/)

### 场景目标

particula.tech 基于云端约 10K token 输入、72 小时滚动采样给出以下参考。该文明确说明
数字是建议而非标准，且不含用户到端点的网络往返：

| 场景 | TTFT P50 | TTFT P95 | 输出速度下限 | E2E P95 |
|---|---:|---:|---:|---:|
| Chat 流式 UI | 800ms | 2s | 30 tok/s | 10s |
| Voice agent（LLM 环节） | 300ms | 500ms | 50 tok/s | — |
| Voice agent（完整轮次） | 600ms | 1s | — | 1.2s |
| Agentic（每步） | 2s | 5s | 50 tok/s | 步数 × 单步预算 |
| 深度推理 | 不设 TTFT，显示进度 | — | 50 tok/s | 30s–3min |

关键结论：

- 10K 输入下快速非推理配置 TTFT 约 0.75–1.6s；亚秒可做到但非常态。
- 专业推理服务商输出速度差异很大，TTFT 仍收敛在约 0.7–1.0s。
- reasoning effort 是延迟旋钮；同一家族 low → max 可使 TTFT 放大 44 倍。
- 面向人的流式输出超过约 30–50 tok/s 后感知趋平，剩余优化应优先给 TTFT。
- 机器消费者和超长生成仍可从更高输出速度获益。

来源：[LLM Latency Targets: TTFT & Tokens Per Second](https://particula.tech/blog/llm-latency-targets-ttft-tokens-per-second-2026)

### 人因与方法论

- Brysbaert 2019 对 18,573 名被试的元分析：成人默读非虚构文本约 238 wpm，
  按英文约 0.75 词/token 折算约 **5.3 token/s**。
  [DOI](https://doi.org/10.1016/j.jml.2019.104047)
- Nielsen 响应时间阈值：0.1s 近似即时，1s 保持思路不断，10s 保持注意力。
  [NN/g](https://www.nngroup.com/articles/response-times-3-important-limits/)
- Azure 延迟分解：`TTLT = TTFT + (TBT × Tokens Generated)`；影响延迟的四类因素是模型、
  prompt token、生成 token、部署负载。“Latency without token context isn't actionable.”
  [Microsoft Learn](https://learn.microsoft.com/en-us/azure/ai-services/openai/how-to/latency)
- OTel GenAI 语义约定采用 `gen_ai.server.time_to_first_token` 和
  `gen_ai.server.time_per_output_token`。
  [OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/gen-ai/)

## llm-perf 三档判据

现代 agent 产品常见输入约 30–40K（system、工具定义、RAG 注入）。短输入徽章不代表
agent 体验，因此基线显式覆盖长输入，但 long 档用的是冷 prefill 最坏角落；prefix cache
命中后的暖路径另行解释。

| 档位 | 适用输入 | TTFT p99 | TPOT p99 | 性质 |
|---|---:|---:|---:|---|
| 优（短输入） | ≤ 4K | ≤ 450ms | ≤ 40ms | MLPerf Interactive |
| 及格（短输入） | ≤ 4K | ≤ 2s | ≤ 200ms | MLPerf Server |
| agent 大上下文 | 30–40K | 优 ≤ 3s / 及格 ≤ 6s | 40ms / 200ms | 推导值 |

long 档 TTFT 的推导：

1. 10K 输入实测 TTFT 0.75–1.6s；冷 prefill 近似随输入线性增长，35K 外推约 2.6–5.6s。
   因此取优 ≤ 3s、及格 ≤ 6s。
2. MLPerf 405B 档在约 9.4K 输入下为 6s p99；小模型在 35K 输入下取 6s 作为及格上限
   是宽松且安全的。

TPOT 沿用短输入档：输入长度主要影响 prefill/TTFT，不改变逐 token 生成速度。

归组规则：

- `prompt_tokens ≤ 4K`：短输入优/及格档；
- `prompt_tokens ≥ 24K`：agent 大上下文档；
- 4K–24K：不打徽章，只报数值和 prefill 斜率。

配置通过 `slo.baseline` 透出，判级由分析侧完成，不进入退出码，不污染原始 JSON。
thinking=on 单独分组，不与 off 共用基线。

## Decode 速度参考

英语无声阅读约 4 tok/s，中文约 5–8 字/s。低于阅读速度时用户会明显“等输出”：

| 单流输出速度 | 体验 |
|---:|---|
| < 5 tok/s | 差，低于阅读速度 |
| 5–10 tok/s | 勉强，可感知偏慢 |
| 10–25 tok/s | 够用到良好 |
| 25–80 tok/s | 良好，主流托管模型区间 |
| > 80 tok/s | 优秀；面向人时感知已趋平 |

报告 `slo.baseline.pass_tps=10 / good_tps=25` 来自本表：10 是阅读速度约 2 倍的安全线，
25 是舒适区上沿。`bench probe` 的 `decode_speed` 只作部署健康参考，不自动熔断或改变后续
采集。

## 引用共识

1. 判断 tail，不判均值；MLPerf 用 p99，场景表用 P50/P95。
2. TTFT 绝对基线只适用于短输入；长输入看 prefill 吞吐斜率。
3. goodput 优于平均延迟；达标率才是容量口径。
4. thinking on/off 必须分开，reasoning effort 对延迟影响巨大。
5. 延迟必须带 prompt/completion token 上下文。
6. 基线随模型规模和上下文浮动，跨档比较绝对值无意义。

## 补充参考

以下来源未逐字核实，引用前先查原文：

| 来源 | 内容 |
|---|---|
| [ecitis 推理 SLO](https://blog.ecitis.org/inference-slos) | agent 工具步 TTFT <1s；TPOT 对非流式不是体验指标 |
| [CSDN Agent SLO](https://agi-way.blog.csdn.net/article/details/161825785) | 中文实践口径：chat、办公、代码、长文档分档 |
| [MLPerf Inference](https://docs.mlcommons.org/inference/) | server / offline / interactive 场景 |
| [Azure OpenAI monitoring](https://learn.microsoft.com/en-us/azure/foundry/openai/monitor-openai-reference) | Azure TTFT、TBT 等指标定义 |
