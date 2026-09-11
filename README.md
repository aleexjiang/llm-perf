# llm-perf

客户自部署 LLM 推理服务性能评测工具（Go，单二进制，无运行时依赖）。

**契约：输入 YAML 配置，输出 JSON 原始数据。** Go 工具本身不做报告渲染——JSON 拿回来
用 `scripts/gen_html_report.py`（仓库自带，离线自包含 HTML，见[报告](#输出)）或 WorkBuddy
等工具二次加工。

面向堡垒机/内网交付场景：本机交叉编译出 linux/amd64 二进制，连同配置文件
（`bench` + `config.yaml`，端点与认证直接写进配置）拷贝到客户环境执行，跑完把 JSON 拉回来分析。

## 场景矩阵

模式 = `--turns` × `--concurrency` 两个参数的组合（**并发=1 即单发串行**）：

```
                --concurrency 1（单发）          --concurrency 2,4,...（并发）
--turns single  档位矩阵 ladder × runs          闭环并发（固定 prompt）
--turns multi   多轮会话逐轮滚动                 闭环并发（每用户独立会话）
```

```bash
./bench -c example.yaml --turns single --concurrency 1    # 单发单轮
./bench -c example.yaml --turns multi  --concurrency 1    # 单发多轮
./bench -c example.yaml --turns single --concurrency 1,2,4 # 并发爬坡（列表含 1 时先跑单发再跑 >1 档）
./bench -c example.yaml                                    # 默认 turns=both concurrency=1
```

| 场景 | 回答的问题 |
|---|---|
| `single`（conc=1, turns=single） | 单请求 TTFT / decode 速度随上下文长度如何增长？前缀缓存有没有命中？ |
| `multiturn`（conc=1, turns=multi） | 多轮对话**滚**到 40k 时每轮 TTFT 如何？（模拟 agent：system + tool defs + 逐轮增长 history） |
| `concurrent`（conc>1） | 并发 1→2→4→8→16 时 TTFT 衰减多少？整体吞吐峰值在哪？（turns=multi 时每个虚拟用户各自跑完整多轮会话；filler=模拟对话，trace 数据源=真实会话重放） |

两个正交开关贯穿全部场景：

- **思考模式**（`thinking.mode: both/on/off`，默认 `both`）：off/on 两套 `extra_body` JSON
  透传进请求体（对齐 vLLM `--extra-body`，适配任意网关的思考参数）。`both` 自动跑 A/B 对照；
  思考开启时 `max_tokens` 自动抬到 `max_tokens_floor`（默认 2048），防止思考吃光输出预算
- **流式**（`stream: true/false`，默认 `true`）：非流式只能测端到端延迟与 usage，
  TTFT/ITL/思考拆分不可测（JSON 中相应字段缺省）；用于 E2E 对照与网关缓冲问题排查

并发场景支持两种负载模型（`concurrent` 段，互斥）：

- **闭环并发**（默认，`levels: [1,2,4,...]`）：N 个虚拟用户同时发车，测容量上限下的衰减
- **开环到达率**（`request_rate: 4` 或 `rate_sweep: [1,2,4,8]`）：请求按 Poisson 过程到达
  （对齐 vLLM bench serve / inference-perf），测排队-延迟曲线；`rate_sweep` 多档扫描找饱和点，
  `max_concurrency` 防止到达率超容量时无限堆积

## 数据源：filler vs trace

- **filler**（默认）：token 精确的合成/语料填充（`filler_lang` + `filler_corpus`），
  用于前缀缓存对照、上下文深度阶梯等**变量控制实验**
- **trace**（`dataset.mode: trace`）：真实会话回放，多轮长度来自真实分布（贴近客户实际流量）。
  支持 ShareGPT 格式与自定义 `sessions` 格式（`[{"turns": ["...", ...]}]`，`.json`/`.json.gz`），
  token 以服务端 usage 为准；trace 模式下 system_tokens/turn_tokens 不生效（会话形状由回放决定）。
  `dataset.replay_mode` 控制回放保真度：`full`（默认，按原序注入全部 role——assistant/tool 消息进上下文，
  测真实 history 深度。真实 agent 会话里工具结果往往占上下文大头，user_only 的回放深度系统性偏小）；
  `user_only`（只回放 user 轮，需对比历史口径时显式配置）；
  full 模式下 `role: tool` 消息缺 `tool_call_id` 会被跳过并计数告警（不静默丢弃）

另有 `bench probe` 兼容性探针（换引擎先跑）、`debug` 原始流量留存与 `/metrics` 服务端观测层，
见下文[兼容性](#兼容性多推理引擎支持)与[服务端观测层](#服务端观测层metrics)。

## 指标口径

每个流式请求逐 chunk 记录时间戳，拆分为：

- **TTFT**：首个任意 chunk（含排队 + prefill）
- **TTFT reasoning**：首个思考增量 chunk（`reasoning` / `reasoning_content` 双字段兼容）≈ prefill 完成时刻
- **TTFT content**：首个可见内容 chunk = prefill + 思考
- **思考时长**（`think_ms`）= TTFT content − TTFT reasoning；**每次对话（含多轮每一 turn）都有**
- **decode 时长 / ITL 分位数**（GenAI-Perf 口径，不含 TTFT；P50/P90/P95/P99/max）/ tokens per second
- **TPOT**：每 output token 时间 =（E2E−TTFT）/(completion−1)，含思考 token（GenAI-Perf 横评口径）
- token 数取自响应 `usage` 字段（服务端精确值，非本地估算），含 `reasoning_tokens` 与
  `cached_tokens`（部分引擎不回传，置信 /metrics 观测层）
- **new_tokens**（多轮）：本轮相对上一轮新增的 prompt tokens；配合 TTFT 得**增量 prefill 速率**
  （ms/千新 token），直接量化"上下文越滚越贵"

单发版关键对照：`fixed_seed: true` 时各 run 复用同一 prompt——Run2+ 的 TTFT 显著低于 Run1 即前缀缓存命中。
多轮版关键判定：turn N 的 TTFT ≈ turn N−1 TTFT + 新增 token 的 prefill ⇒ 缓存命中；接近全量 prefill ⇒ 未命中。

## 填充语料：真实文本，支持到 1M 上下文

默认合成词表低信息量、可复现，但 tokenization 与语义分布和真实负载有差距。配置 `filler_corpus` 后
改用**内置公版书语料**（go:embed 打进二进制，堡垒机无需额外文件）：

| 配置值 | 语料 | 规模 |
|---|---|---|
| `filler_corpus: "en"` | 战争与和平 + 白鲸记（Gutenberg #2600/#2700） | ≈ 99 万 token |
| `filler_corpus: "zh"` | 红楼梦（120 回全文） | ≈ 62 万 token（超出部分循环填充） |
| `filler_corpus: "path/to/x.txt(.gz)"` | 自定义语料（UTF-8） | 不限 |

字符/token 换算比已在真实 Qwen 服务上校准（en 4.0 chars/token 实测偏差 <2%，zh 1.4 实测偏差 ≈3%）；
`filler_lang` 决定语料语言与换算比；同 seed 仍产出相同文本（`fixed_seed` 缓存实验不受影响）。

**上下文截止**：`max_prompt_tokens`（或 CLI `--max-ctx`）设定压测的上下文上限——

- single 档位超限自动截到该值并去重（如 `[50k, 300k, 1M] --max-ctx 262000` → `[50k, 262000]`）
- 多轮会话逐轮逼近上限，最后不足一轮的空间压缩填充、到顶即停（日志注明提前结束）
- `bench probe` 会读取服务端 `max_model_len` 并对比计划压测的最大档位，超限直接告警
- 大上下文注意 `timeout_seconds`（1M 级 prefill 可能需要 5 分钟以上，工具在 ≥100k 档位时自动提示）

## 兼容性：多推理引擎支持

核心协议是 OpenAI 兼容 `/v1/chat/completions`，vLLM / SGLang / TGI / MindIE（华为魔改 vLLM）/ llama.cpp 等均可用，
但各引擎在**细节字段**上差异很大（`reasoning` vs `reasoning_content`、usage 是否回传、`stream_options` 支持、
非标 delta 字段）。工具内置三层排查能力：

**1. `bench probe` —— 换引擎先跑探针**

```bash
./bench probe -c example.yaml          # 对 /models 列表第一个模型探测
./bench probe -c example.yaml <模型ID>  # 指定模型
```

输出：引擎猜测（Server 头 + 响应特征）、模型列表、逐项检查（非流式/流式/usage/[DONE]/**部署默认思考状态**、
思考开关有效性、思考等级可控性）、**tool-call 健康检查**（默认开启，`--no-toolcall` 关闭）、
结论提示（哪些配置要改、哪些魔改需要适配）、**可粘贴回配置文件的 YAML 片段**，落盘 `probe-<时间戳>.json`。

### 证据分级：标准面才是配置基线

检查项分两级，**只有标准面（`/chat/completions`）的结论才用来定配置**：

| 分级 | 来源 | 缺失时 | 记号 |
|---|---|---|---|
| **标准面** `core` | OpenAI 兼容契约内的 `chat` 接口 | 是缺陷，必须处理 | ✅ / ❌ |
| **扩展面** `ext` | 引擎扩展：`/metrics`、`/models` 的 `max_model_len`、`Server` 头 | **只记 NA，不判失败、不计入通过率** | ✅ / ➖ |

这条分级是本工具的一等原则：很多推理服务、尤其经网关代理之后，`/metrics` 根本不转发、
`/models` 只返回 `id/object/created/owned_by`（`max_model_len` 是 vLLM/SGLang 扩展字段，会被剥掉）。
把这些缺失当故障，会让探针在正常服务上到处误报 ❌。扩展面探到了是白捡的增强（可用它优化采集与报告），
探不到只记 `➖ … [扩展面未提供，不计入结论]`。跑完会打一行汇总，例如
`📊 标准面 12/12 通过；扩展面 2 项可用、1 项服务端未提供（非标准端点，不影响结论）`。

### 自举：认证格式与挂载路径不用再猜

`bench probe` 先用一个 `max_tokens=1` 的最小请求确认 chat 面，命中以下情况自动试探并给出可用值：

- **认证被拒（401/403）** → 依次试 `Bearer/Authorization`、裸 key/`Authorization`、裸 key/`X-API-Key`、
  `Bearer/X-API-Key`，命中即写进结论与配置片段。当前方案可用时**不会**额外发送 key，不把密钥撒到更多 header。
- **挂载点落空（404/405）** → chat、models、metrics 各自扫常见候选
  （如 `/v1/chat/completions`、`/openai/v1/chat/completions`、`/actuator/prometheus`），命中即提示改配置。
- **端点不可达** → 直接 `❌ chat_endpoint` 并停止：其余探测项无从谈起，避免每项各超时一次。

配置片段里**只有标准面确认过的值写成生效项**；由扩展面推导来的（如 `max_prompt_tokens` 由
`max_model_len` 换算、`server_metrics: true`）一律注释掉并标明来源——换个端点就可能失效。

`thinking_default` 是**无参数基线**，独立于配置：部署侧可能已经把思考关掉了（模板硬编码
`enable_thinking=false`、没挂 `--reasoning-parser`、启动参数就是非思考模式），此时"本次压测跑的是
思考态"是错误前提，整份性能结论会整体错位。探针把默认态显式打出来，默认关思考时给 ⚠️ 提示；
若配置声明了开启参数却拿不到思考增量，会进一步指出"优先怀疑部署侧关思考，而非参数名写错"。
配置用 `thinking.levels` 时（`extra_body_on/off` 为空），探针自动从 levels 里取开启态/关闭态兜底。

tool-call 健康检查是**前置门禁**：检出引擎能否正常调工具（parser 是否启用、流式调用是否丢失、
内容是否泄漏 `<tool_call>` 标记），失败时给出可行动结论（可直接贴给客户/厂商）；
不测性能、不进主压测路径。判据分级：硬特征（标记泄漏/裸 JSON/参数非法）→ ❌；
软特征（网关 4xx/finish_reason 错位）→ ⚠️；检查自身超时/网关错 → "检查未完成"，不计入结论。
`--probe-capture <目录>` 把检查的原始响应落盘（厂商排障证据 + 判据回归 fixture；含业务数据，外发前脱敏）。

**认证格式**：默认 `Authorization: Bearer <key>`；客户网关用裸 key 时配 `auth_scheme: raw`，
免认证端点配 `auth_scheme: none`，自定义 header 名配 `auth_header`（如 `X-API-Key`）——配错也不怕，
probe 会自举出可用值。接口路径与指标路径同样可配：`chat_path`（默认 `/chat/completions`）、
`metrics_path`（默认 `/metrics`）、`models_path`（默认 `/models`）；
probe 的请求超时直接读 `timeout_seconds`（此前硬编码 120s 不受配置影响）。

**2. 请求级兼容性告警（自动）**

每个请求的 JSON 里带 `warnings` 字段，日志同步打印 `⚠️ 兼容性告警`：
`usage_missing`（token 数不可信）/ `stream_ended_without_done` / `unknown_delta_fields: xxx`（魔改字段名）/
`unparseable_stream_line` / `thinking_no_content`（思考吃光 max_tokens）。
分析报告时先按 warnings 过滤脏数据。

**3. `debug: true` —— 原始流量留存**

```yaml
debug: true   # 原始响应 → <output_dir>/raw/*.log；日志同步 → <output_dir>/run.log
```

每个请求一份转储：请求体摘要 + 状态码 + 原始 SSE 行（头部 256KB）。
魔改引擎行为看一眼 raw 文件就清楚；**请求失败时即使不开 debug 也会自动转储**。

**4. 交叉验证建议（probe 自动输出）——工具结果存疑时用引擎原生工具复核**

`bench probe` 识别出引擎后，输出对应的原生 perf 工具与按当前配置映射的等价命令：

| 引擎 | 原生工具 |
|---|---|
| vLLM | `vllm bench serve`（CLI 内置，原 benchmark_serving.py） |
| SGLang | `python3 -m sglang.bench_serving`（`--backend openai` 可打任意兼容端点） |
| TGI | `text-generation-benchmark` |
| TensorRT-LLM / Triton | `trtllm-bench` / GenAI-Perf（AIPerf） |
| MindIE（华为） | MindIE-Service 自带 benchmark（版本间差异大） |

注意：交叉验证是**对数量级与分位趋势**（数据集/计时口径不同），不是逐数对齐。
结果对不上时的排查顺序：数据集差异 → 网络路径 → 客户端计时方法。

## 服务端观测层（/metrics）——可选第二数据源

**定位先说清楚**：`/metrics` 是引擎实现细节，不是标准端点（网关、反向代理、SaaS 托管普遍不提供）。
**客户端实测是本工具唯一的标准口径**；服务端观测是**可选的第二数据源**——有就多采一份做交叉验证
与归因，没有就只跑客户端口径，**结论一条都不少、口径一处不改**。这与 `probe` 的证据分级
（`core` / `ext`）是同一条原则在压测链路上的落地。

```yaml
server_metrics: true   # 有 /metrics 就多采一份（vLLM 默认暴露）；不可达不影响结论
```

不可达或抓取失败时只打一行说明（`ℹ️ 未提供 /metrics …… 全部结论按客户端实测口径给出`），
不中断、不降级措辞：报告里「数据来源」会如实写成「客户端实测（基线）；未启用或端点未提供 /metrics」。

指标命名经 `MetricsProvider` 抽象，**按抓取样本的指标名前缀自动识别引擎**（`vllm:` → vLLM、
`sglang:` → SGLang；无法识别时日志显式告警"按 vLLM 命名尝试，服务端指标大概率拿不到数"，
不静默套错——自研网关属预期）；SGLang 的缓存 counter 命名待真机校准。
**/metrics 抓取带认证头**（`auth_scheme`/`auth_header` 对探测、场景快照与 gauge 轮询同样生效）——
网关把 metrics 端点与业务接口用同一套认证保护时不会误判不可用。
gauge 轮询自带健康度：从未成功或连续失败 ≥5 时 JSON 标记 `observation_degraded`，报告出红色警示。

对标 NVIDIA AIPerf / inference-perf 的 server metrics 层，给客户端计时补上服务端视角：

- **counter 请求前后差值**：前缀缓存命中 tokens（**逐 turn 命中率**）、preemptions（KV 淘汰重算）、
  MTP 投机解码 draft/accepted（接受率）；串行时精确归因到单请求
- **gauge 高频轮询**：running/waiting 排队深度、KV 池占用的峰值/均值
- **histogram 场景窗口差值**：服务端口径的 queue/prefill/decode/TTFT/ITL 延迟分解（P50/P99 桶估算）
- 结果进 JSON（`server_metrics` 汇总 + 逐请求 `server_counter_delta`）与报告「服务端观测」章节

典型用法：TTFT 高时看命中率（低=缓存没生效）与 queue 时间（高=排队）、preemptions>0（KV 压力）。

## 其他

- **测试盐值**：`seed_salt: N`（CLI `--seed-salt`）给所有 prompt 种子叠加盐值——服务端 prefix cache
  是内存态且不会被挤出，同一配置重跑时"冷缓存"测量会被上次测试污染；**每次测试递增盐值**，
  或重启服务端清缓存（二选一）
- **预热**：`warmup_requests: N` 每场景开始前发 N 条小请求暖连接（不计入统计，唯一内容不污染缓存对照）
- **连接层重试**：`retry: {max_attempts: 2, backoff_ms: 300}` 对瞬时失败（reset/5xx/429）重试，默认关闭；重试留痕 warnings/retry_count
- **降速熔断**：`stall_guard: {min_tps: 20, window_seconds: 600, cooldown_seconds: 300}` 按**聚合输出速度**
  （窗口内在飞请求输出 token 之和 / 时长）判定服务端退化，持续低于阈值即中止**当前场景**（不是整轮），
  冷却后继续下一个场景；已完成数据照常落盘，报告 note 标注熔断原因与现场速度。空闲与纯 prefill 不参与
  判定。CLI `--stall-tps/--stall-window/--stall-cooldown`、`--no-stall-guard`
- **goodput**：`goodput: {ttft_ms: 2000, tpot_ms: 100}` 定义 SLO，concurrent 结果输出达标数与有效吞吐
- **正确性抽查**：`correctness: {samples: 8}` 数字转写金丝雀，防"HTTP 200 但内容异常"的假成功

## 快速开始

```bash
# 本机
make build-linux                    # 产出 bin/bench-linux-amd64
scp bin/bench-linux-amd64 configs/example.yaml 堡垒机:~/llm-perf/

# 堡垒机
mv bench-linux-amd64 bench && chmod +x bench
# 端点/key 直接写在配置文件里（endpoint + api_key 字面量；该配置勿入库）
./bench probe -c example.yaml       # ① 先探针：确认引擎兼容性与思考开关参数
./bench -c example.yaml --turns single --concurrency 1   # ② 小档位验证解析正确性（改小 prompt_tokens/max_tokens）
./bench -c example.yaml                                  # ③ 默认 turns=both concurrency=1（单发全矩阵）
```

## 输出

每个场景落一个 JSON 文件（含全部原始数据：逐 run 计时、逐 chunk 派生指标、usage token）：

```bash
./bench -c example.yaml                                  # → output/single-<ts>.json multiturn-<ts>.json
./bench -c example.yaml --turns single --concurrency 1 -o r1.json   # → 指定输出文件名
./bench -c example.yaml --turns both --concurrency 1,2,4 -o results/  # → 指定输出目录
```

**按模型分区**：配置了多个模型时，数据按模型分区落 `<output_dir>/<模型>/<场景>-<ts>.json`
（模型名取 `/` 后末段），重测/作废单模型数据不纠缠；`run.log` 与 `raw/` 仍在 output_dir
顶层（测试级共享）。单模型配置（或 `-m` 过滤后只剩一个）保持原布局直接落 output_dir：

```bash
output/
├── run.log
├── DeepSeek-V4-Flash-0731/
│   ├── single-<ts>.json
│   └── multiturn-<ts>.json
└── Qwen3.8-27B/
    ├── single-<ts>.json
    └── multiturn-<ts>.json
```

多模型 + `-o xxx.json` 会报错（一个文件装不下多个分区），请给目录。

JSON 结构见 `internal/report/report.go`：`single` / `multiturn` / `concurrent` 三个数组，
元素分别为档位 / 会话 / 并发档位，每条请求是 `engine.TurnMetrics`；观测层开启时附
`server_metrics` 汇总（缓存命中率/排队/prefill-decode 分解）与逐请求 `server_counter_delta`。

JSON → HTML 分析报告（自包含、Chart.js 内嵌离线可用）。自动合并多份 JSON（含 thinking off/on 分离的测试）、
数据驱动生成结论与建议（prefill 斜率、前缀缓存判定、decode 吞吐、思考行为分类），报告末尾内嵌
`perf-summary` JSON 数据块——把整份 HTML 交给 AI 即可让它追加通俗解读备注：

```bash
python3 scripts/gen_html_report.py output/                    # 目录模式：递归合并目录下全部场景 JSON
python3 scripts/gen_html_report.py a.json b.json [标题]        # 文件模式：显式指定一份或多份报告
# 四象限报告：--scenarios 选场景，缺省 = 四象限合并报告
#   single=单发·单轮  multiturn=单发·多轮  conc-single=多发·单轮  conc-multi=多发·多轮
#   支持中文别名与 + 分隔：--scenarios 单发单轮+多发多轮
python3 scripts/gen_html_report.py output/ 标题 --scenarios conc-multi      # 只出并发多轮报告
python3 scripts/gen_html_report.py output/ 标题 --scenarios 单发单轮,多发单轮  # 任意组合
node scripts/validate_report.js <报告.html>   # JS 端校验（占位符/图表可执行）
```

## 配置

见 `configs/example.yaml`，含详细注释。端点与认证优先写配置文件（`endpoint` + `api_key` 字面量）；
环境变量 `LLM_PERF_ENDPOINT`、`LLM_PERF_API_KEY` 优先级最高（应急通道；另有 `api_key_env` 指定从哪个变量读 key）。

**配置组织：顶部通用 + 底部 `model_overrides`**。一份文件写所有模型共享的通用配置，
底部按模型只写差异项（场景参数/思考/max_prompt_tokens/stream 等，未写的键继承顶层）。
每个模型可带 `enabled` 开关控制本次是否测试（分批重测时临时关掉，`enabled: false` 的模型
不进场景循环、probe 也不选；全部禁用直接报错）。

## 开发

```bash
make build          # 本机二进制（-ldflags 注入 git describe 版本号到 JSON 的 tool 字段）
make test
scripts/smoke.sh    # 自动化冒烟：mock 服务 + 多组合运行 + 输出数据形状断言
                    # （模型×变体分布/CLI 过滤回归/全能力 trace/开环/levels/-m/--max-ctx/
                    #   错误路径/报告管线；mock 含 /models、/metrics 与 tool-call 好路径）
                    # 运行产物全部落 /tmp 随脚本退出清理，不污染仓库
scripts/mock_server.py   # 本地 mock OpenAI 兼容流式服务（smoke.sh 底层依赖）
                    # 环境变量 MOCK_TOKEN_DELAY 可调每 chunk 间隔（默认 0.05≈20 tok/s），用来构造降速现场
scripts/stall-e2e/  # 降速熔断端到端回归：假慢速服务端 + 配置，验证
                    # 「聚合降速 → 中止当前场景 → 冷却 → 续跑下一场景 → 报告 note 留痕」整条链路
                    # （与 smoke.sh 分开：这里的 mock 需要按请求区分快慢，探针必须秒回）
scripts/gen_html_report.py  # JSON → 自包含 HTML 分析报告（多文件合并 + 自动结论 + 内嵌 AI 摘要）
scripts/validate_report.js  # 报告 JS 校验（占位符/图表可执行）
deploy/             # 推理服务 compose 存档：vLLM 基线 + SGLang / TensorRT-LLM / llama.cpp / TGI
                    # 引擎横评（各文件头部含与基线的逐参数对照与 bench 侧注意点）
```

## 设计文档

- [docs/architecture.md](docs/architecture.md) — 内部架构：模块边界、数据流、扩展点、已知坑位（二开/维护者向）
- [docs/data-contract.md](docs/data-contract.md) — 数据契约：JSON 输出结构、聚合口径、报告侧对齐规则
- [docs/latency-baselines.md](docs/latency-baselines.md) — 体验基线 3 档制的依据与原文链接
