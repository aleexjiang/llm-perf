# LLM 推理体验基线参考（对外解释依据）

用途：为压测报告的"体验基线评估"提供**有出处的判据**。对外解释时引用本文件所列原文链接，不凭印象报数。
整理方式：所有核心数字均于 2026-09-08 抓取原文逐字核实，关键句附英文原文引用。**补充参考**一节除外（未逐字核实，引用前需先查原文）。

---

## 1. MLPerf / MLCommons（行业基准，判据最硬）

MLPerf Inference 是推理性能的行业事实标准，其 server 场景延迟约束即"可接受的体验上限"。

### 1.1 Server 档（Llama2-70B，2024-03 定）

| 指标 | 约束（p99） |
|---|---|
| TTFT | ≤ 2 秒 |
| TPOT | ≤ 200 毫秒 |

原文依据与锚定逻辑：

> "Using the typical human reading speed as a logical anchor, these latency constraints were set as: **TTFT: <= 2 seconds, TPOT: <= 200 milliseconds**. A TPOT of 200 ms translates to a maximum allowed generation latency that maps to ~240 words per minute (depending on the tokenizer), which is often cited as the average human reading speed."

> "While reading speed is a good yardstick for generation tasks such as LLM-based chat or tech support, other use cases have tighter latency constraints. For example, code generation, real-time slide generation, and smart agents all require much faster response times."

（注意官方自己声明：**agent 场景需要比这更紧的约束**——这档只能当"及格线"。）

- 来源：[Llama 2 70B: An MLPerf Inference Benchmark for Large Language Models](https://mlcommons.org/2024/03/mlperf-llama2-70b/)（MLCommons 官方博客，2024-03-27）

### 1.2 Interactive 档（Llama2-Chat-70B，v5.0 新增，2025-04 定）

| 指标 | 约束（p99） |
|---|---|
| TTFT | ≤ 450 毫秒 |
| TPOT | ≤ 40 毫秒（25 tok/s） |

原文依据（2024 年底基于 ChatGPT / Perplexity 实测数据与用户调研修订）：

> "Our findings indicated that a 50th percentile token generation rate of **20–50 tokens per second (TPOT of 20-50ms) is critical for seamless user experience**. To prioritize reliability under peak demand, MLPerf adopts a stricter 99th percentile threshold of **25 tokens/second (TPOT of 40ms)**, ensuring consistent responsiveness even during high-load scenarios. Additionally, we set a **99th percentile TTFT limit of 450ms** to minimize initial latency, aligning with user expectations for near-instantaneous query response."

- 来源：[MLPerf Inference v5.0 Advances Language Model Capabilities for GenAI](https://mlcommons.org/2025/04/llm-inference-v5/)（2025-04-02）

### 1.3 大模型长上下文档（Llama3.1-405B，128K 上下文）

| 指标 | 约束（p99） |
|---|---|
| TTFT | ≤ 6 秒 |
| TPOT | ≤ 175 毫秒 |

> "To balance the demands of long-context processing with real-world usability, we set a 99th percentile TTFT of **6 seconds** and a 99th percentile TPOT of **175ms**. These thresholds reflect the computational challenges of deploying large models with long context, while maintaining reasonable responsiveness."

含义：**基线随模型规模与上下文长度浮动**——大模型长上下文的 TTFT 预算是小模型的 13 倍，跨档比较 TTFT 绝对值无意义。

- 来源：同 v5.0 博客（见 1.2）

---

## 2. 场景化目标（particula.tech 实测 + 人因研究推导，2026）

该文为公开实测报告（云端区域内 ~10K token 输入，72 小时滚动采样），数字前需声明其前提：

> "These are our recommendations, derived from the measurements above plus human-perception research. **They are not a published standard and no provider commits to them.**"
> "Treat them as a floor, not a forecast."（所有数字不含用户到端点的网络往返）

### 2.1 场景目标表（Table A 摘要）

| 场景 | TTFT P50 | TTFT P95 | 输出速度下限 | 端到端 P95 |
|---|---|---|---|---|
| Chat 流式 UI | 800ms | 2s | 30 tok/s | 10s |
| Voice agent（仅 LLM 环节） | 300ms | 500ms | 50 tok/s | — |
| Voice agent（完整对话轮次） | 600ms | 1s | — | 1.2s |
| **Agentic（每步）** | **2s** | **5s** | 50 tok/s | 单步预算 × 步数 |
| 深度推理 | 不设 TTFT 目标，显示进度 | — | 50 tok/s | 30s–3min |

> "For agentic multi-step work, **budget 2s per step at P50 and multiply by step count**."

### 2.2 关键实测结论（对外解释可直接引用）

- 快速非推理配置 TTFT 0.75–1.6s（10K 输入，云区域内）；"Sub-second first token is achievable. It is not the norm."
- 专业推理服务商输出速度差异巨大（35–578 tok/s），但 **TTFT 收敛在 0.7–1.0s**："Buying a specialist serving stack buys output speed. It does not buy TTFT."
- 推理模型的 reasoning effort 是延迟旋钮：同一家族 low→max effort，TTFT 2.97s→129.47s（44 倍），输出速度不变
- 面向人的流式输出 30–50 tok/s 已到感知上限，多余预算应投给 TTFT："For human-facing streamed output, above roughly **30 to 50 tok/s the extra speed is invisible**, and every remaining perceived-latency gain has to come from TTFT."
- 例外（输出速度仍重要）：机器消费者（agent 循环、工具链）与超长生成——"machine consumers (agent loops, tool-call chains, extraction pipelines where nobody reads the tokens)"

- 来源：[LLM Latency Targets: TTFT & Tokens Per Second](https://particula.tech/blog/llm-latency-targets-ttft-tokens-per-second-2026)

---

## 3. 人因研究（阈值的物理依据）

### 3.1 阅读速度（decode 速度下限的锚）

> 2019 年对 190 项阅读速率研究、18,573 名被试的元分析：成人默读非虚构文本约 **238 wpm**。按英文 ~0.75 词/token 折算约 **5.3 token/秒**。

- 来源：Brysbaert, M. (2019). *How many words do we read per minute?* Journal of Memory and Language, 109, 104047. [https://doi.org/10.1016/j.jml.2019.104047](https://doi.org/10.1016/j.jml.2019.104047)
- 注：以上折算引用自 particula.tech（见 2）；MLPerf Server 档用的是 ~240 wpm 同源口径（见 1.1）。

### 3.2 响应时间感知阈值（TTFT 上限的锚）

> "0.1 seconds is about the limit for having the user feel that the system is reacting instantaneously, 1 second is about the limit for having the user's flow of thought stay uninterrupted, 10 seconds is about the limit for keeping the user's attention on the dialogue."

- 来源：Nielsen, J. (1993). *Usability Engineering*；网络版 [Response Times: The 3 Important Limits](https://www.nngroup.com/articles/response-times-3-important-limits/)（Nielsen Norman Group）
- particula 对此的落点："For a streaming chat UI, target 800ms at P50 and 2s at P95, which keeps you inside the classic one-second limit for uninterrupted flow of thought."

### 3.3 推论：decode 速度存在"天花板效应"

输出 > 20–30 tok/s 已快过所有读者（5.3 tok/s 的 4–6 倍），再快无感；**TTFT 每一毫秒用户都在盯着等，没有天花板**。这是"优先优化 TTFT 而非 tok/s"的人因依据。

---

## 4. 方法论标准（怎么测，而非测多少）

### 4.1 Azure OpenAI 官方延迟文档（测量与归因框架）

**注意：该文档不含基准数字**（无 TTFT 毫秒范围、无 tok/s 参考值、无吞吐承诺），定位是"如何测量和归因"，对外引用时不能拿它当基线出处。可用内容：

- 延迟分解公式：`TTLT = TTFT + (TBT × Tokens Generated)`
- 四大归因因素："Latency of a completion request can vary based on four primary factors: (1) the model, (2) the number of tokens in the prompt, (3) the number of tokens generated, and (4) the overall load on the deployment and system."
- 对报告口径最重要的一句："A 5-second TTLT that generates 2,000 tokens is very different from a 5-second TTLT that generates 50 tokens. **Latency without token context isn't actionable.**"（报延迟必须连带 token 上下文，否则不可解读）
- 混合负载损伤缓存："Mixing different workloads on the same endpoint can negatively affect latency… mixing the calls can reduce your cache hit rate"（我们 5.6 混合负载的场景依据）

- 来源：[Azure OpenAI latency](https://learn.microsoft.com/en-us/azure/ai-services/openai/how-to/latency)（Microsoft Learn）

### 4.2 OpenTelemetry GenAI 语义约定（指标命名标准化）

TTFT（`gen_ai.server.time_to_first_token`）与 TPOT（`gen_ai.server.time_per_output_token`）已被 OTel GenAI 语义约定定为标准 histogram 指标——我们 server_metrics 采集口径与之一致。

- 来源：[OpenTelemetry GenAI Semantic Conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/)

---

## 5. 补充参考（未逐字核实，引用前必须先查原文）

| 来源 | 内容要点 | 链接 |
|---|---|---|
| ecitis 推理 SLO 分析 | agent 工具步（非流式）只设 TTFT <1s；TPOT 对非流式不是体验指标 | https://blog.ecitis.org/inference-slos |
| CSDN「Agent SLO 设计」 | 中文实践口径：chat agent TTFT≤1s/P95≤3s；办公 1.5s/5s；代码 2s/10s；长文档 3s/30s | https://agi-way.blog.csdn.net/article/details/161825785 |
| MLPerf Inference 官方文档 | 基准套件全貌、场景定义（server/offline/interactive） | https://docs.mlcommons.org/inference/ |
| Azure OpenAI 监控指标参考 | `AzureOpenAITimeToResponse`、`AzureOpenAINormalizedTBTInMS` 等指标定义 | https://learn.microsoft.com/en-us/azure/foundry/openai/monitor-openai-reference |

---

## 6. 方法论共识（多来源一致，构成报告判据设计原则）

1. **判 tail 不判均值**：一律用 p95/p99 对基线（MLPerf 用 p99；particula 表分 P50/P95）。均值会被长短请求混合抹平。
2. **TTFT 绝对基线只适用短输入**：TTFT 随输入长度线性涨（Azure 四因素之（2）），32K 输入对 450ms 基线必然全红；长输入应看 **prefill 吞吐斜率**而非绝对 TTFT。
3. **goodput 优于平均延迟**：达标率（满足 SLO 的请求占比）才是容量口径。
4. **思考模型单列**：reasoning effort 是延迟旋钮（2.2 节，44 倍差异），thinking=on 不能与 off 共用基线。
5. **延迟必须带 token 上下文**："Latency without token context isn't actionable"（4.1）。
6. **基线随模型规模/上下文浮动**：70B 与 405B 的官方预算差 13 倍（1.1 vs 1.3），报告基线应按模型档位分列。

---

## 7. 与 llm-perf 报告的映射（3 档制，报告侧已实现）

**为什么 agent 场景重点看 30–40K 输入档**：现代 agent 产品基线上下文即 ~35K（系统提示 + 工具定义 + RAG 注入，用户发一句"你好"请求就已带 35K 上下文），短输入档的徽章代表不了 agent 体验。TTFT 判据必须显式覆盖 30–40K 输入段。

### 7.1 三档阈值

| 档位 | 适用输入 | TTFT p99 | TPOT p99 | 出处 |
|---|---|---|---|---|
| 档1 优（短输入） | ≤ 4K | ≤ 450ms | ≤ 40ms | MLPerf Interactive（1.2） |
| 档2 及格（短输入） | ≤ 4K | ≤ 2s | ≤ 200ms | MLPerf Server（1.1） |
| 档3 agent 大上下文 | 30–40K | 优 ≤ 3s / 及格 ≤ 6s | 沿用 40ms / 200ms | **推导值**，见 7.2 |

档3 的 TPOT 沿用短输入档：decode 速度与输入长度基本无关，输入变长只影响 prefill（TTFT），不影响逐 token 生成。

### 7.2 档3 阈值的推导过程（无权威直引，须标注"推导值"）

没有任何权威来源直接给出 35K 输入的 TTFT 基线，档3 数字由两条独立证据夹出：

1. **行业现状外推**：particula 实测云端产品 10K 输入 TTFT 0.75–1.6s（2.2 节）；冷 prefill 是 compute-bound，TTFT 随输入长度近似线性涨（Azure 四因素之（2），4.1 节）→ 35K ≈ 3.5×，得 **2.6–5.6s** 行业现状区间。取优 ≤ 3s（对应端到端 prefill ≥ ~12K tok/s）、及格 ≤ 6s。
2. **MLPerf 405B 档佐证上限**：官方 6s p99 TTFT @ 数据集均值 9.4K 输入（1.3）——那是 405B 大模型的口径，同输入下小模型的 prefill 应显著更快，故 35K 输入 6s 作为"及格上限"是宽松且安全的。

暖路径（prefix cache 命中）下 35K 输入的 TTFT 只由新增 token 决定，会远好于档3——**档3 判的是冷 prefill 最坏角落，暖路径另行解读**（见冷/暖分测原则）。

### 7.3 档位归组规则

- `prompt_tokens ≤ 4K` → 打档1/档2 徽章（✅ 优 / ⚠️ 及格 / ❌ 差）
- `prompt_tokens ≥ 24K`（默认边界，可配）→ 打档3 徽章
- 4K–24K 之间 → 不打徽章，只报数值与 prefill 斜率（两档之间无权威锚点，不硬造）

### 7.4 配置形态（与 goodput 合流）

```yaml
slo:                      # 体验基线评估（3 档制徽章 + 结论段）
  baseline: true          # 默认开
  goodput:                # 原有达标口径迁入，与徽章共用一套阈值
    ttft_ms: ...
    tpot_ms: ...
  # 以下可选覆盖内置默认（均为 p99）：
  # tiers:
  #   short_max_tokens: 4000     # 短输入档上界
  #   long_min_tokens: 24000     # 大上下文档下界
  #   ttft_ms: {short_good: 450, short_pass: 2000, long_good: 3000, long_pass: 6000}
```

落地形态：HTML 报告"体验基线评估"区（✅/⚠️/❌ 徽章 + 结论段，逐档位标注出处链接指向本文件）；基线不进退出码、不污染原始 JSON；thinking=on 单独分组不打短输入档徽章（共识 4）。
