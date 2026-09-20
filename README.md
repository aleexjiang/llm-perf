# llm-perf

客户自部署 LLM 推理服务性能评测工具（Go，单二进制，无运行时依赖）。

**契约：输入 YAML 配置，输出 JSON 原始数据。** 工具专注数据采集，不做报告渲染（2026-09-17
报告层已整体剥离）——JSON 拿回来用 pandas / notebook / WorkBuddy 等外部工具二次加工，
数据结构与口径见 [docs/data-contract.md](docs/data-contract.md)（契约版本随每份 JSON 的
`schema_version` 落盘）。

面向堡垒机/内网交付场景：本机交叉编译出 linux/amd64 二进制，连同配置文件
（`bench` + `config.yaml`，端点与认证直接写进配置）拷贝到客户环境执行，跑完把 JSON 拉回来分析。

## 场景矩阵（显式子命令，2026-09-18 新架构）

```bash
./bench probe        -c example.yaml    # ① 能力与健康探针（换引擎先跑）
./bench user         -c example.yaml    # ② 生成式多轮用户会话（profile 驱动）
./bench rps          -c example.yaml    # ③ 冻结请求快照的开环到达（体验拐点）
./bench concurrency  -c example.yaml    # ④ 固定在飞齐射（总吞吐拐点，对齐 vLLM bench serve）
```

| 场景 | 输入 | 回答的问题 |
|---|---|---|
| `probe` | 无 trace | 引擎兼容性、usage/思考/tool-call 能力、/metrics、context limit |
| `user` | profile + 经典书语料 | 真实形状多轮会话里 TTFT 逐轮斜率、动态 prefix cache、单流体验 |
| `rps` | 冻结请求快照 | 到达率 X req/s 下的排队、TTFT P95、goodput——可对外承诺的到达率 |
| `concurrency` | 冻结请求快照（ShareGPT） | 总吞吐拐点：系统最多能装多少；与 vLLM bench serve 横向对比 |

两个正交开关贯穿全部场景：

- **思考模式**（`thinking.mode: both/on/off`，默认 `both`）：off/on 两套 `extra_body` JSON
  透传进请求体（对齐 vLLM `--extra-body`）。`both` 自动跑 A/B 对照；
  思考开启时 `max_tokens` 自动抬到 `max_tokens_floor`（默认 2048），防止思考吃光输出预算
- **流式**（`stream: true/false`，默认 `true`）：非流式只能测端到端延迟与 usage，
  TTFT/ITL/思考拆分不可测（JSON 中相应字段缺省）；用于 E2E 对照与网关缓冲问题排查

**rps vs concurrency 只差调度器**（请求源相同）：

- **rps**（`rps.rates`）：请求按 Poisson 到达（`burstiness` 可调），`max_concurrency` 防无限堆积；
  到达率超过服务能力时积压真实发生——测出过载下的排队曲线与体验拐点
- **concurrency**（`concurrency.levels`）：N 个在飞打满后有空位就补（`request_rate: 0` = inf 齐射）；
  服务端永远满负荷但不持续积压——测出总吞吐拐点与容量天花板

两个拐点配合读：**concurrency 告诉你系统有多能装，rps 告诉你敢承诺装多少**。

## 数据源（全部外置，raw trace 不进仓库）

- **user 模式**：profile.json（`scripts/profile_build.py` 从 raw trace 提取特征，不含消息文本）
  + 内置 12 本公版书语料（一用户一书，seed 确定性选书）。运行时 user/context 由 profile+seed 生成，
  **assistant 用被测模型真实回复**——prefix cache 反映真实生成链，不是冻结输入
- **rps/concurrency 模式**：ShareGPT 数据集直接读取（`request_set.sharegpt_path`），
  与 vLLM `bench serve --dataset-name sharegpt` 同口径：prompt = 截至最后一条 user 的 history，
  输出预算 = 其后 assistant 回复估算 token，`seed` 蓄水池抽样。每条请求是**冻结独立快照**，
  同 seed 同样本序，可复现、可与 vLLM 对比

另有 `bench probe` 兼容性探针（换引擎先跑）、`debug` 原始流量留存与 `/metrics` 服务端观测层，
见下文[兼容性](#兼容性多推理引擎支持)与[服务端观测层](#服务端观测层metrics)。## 指标口径

每个流式请求逐 chunk 记录时间戳，拆分为：

- **TTFT**：首个含 reasoning/content token 的 chunk（含排队 + prefill；role-only/usage 空帧不计入）
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

## 语料库：12 本公版书（user 模式文本原料）

user 模式的合成文本（system 基座 / user 输入 / context）全部取自内置语料库
（go:embed 打进二进制，堡垒机无需额外文件）：一用户一本书，`hash(session_seed)` 确定性选书。

| lang | 书目 |
|---|---|
| en | War and Peace / Moby Dick / Pride and Prejudice / Crime and Punishment / Les Misérables / Don Quixote / Sherlock Holmes |
| zh | 红楼梦 / 三国演义 / 水浒传 / 西游记 / 儒林外史 |

`corpus_lang: en|zh` 决定语料语言；字符/token 换算已在真实 Qwen 服务上校准
（en 4.0、zh 1.4，偏差 <3%），`bench probe` 的 `filler_fidelity` 检查实测本部署真实换算比，
偏离 >25% 告警。窗口起点由 (session_seed, turn) 派生：同配置重跑文本完全一致（可复现），
会话间内容互不相同，会话内尾部 append-only（prefix cache 前缀逐字节保留）。

**上下文截止**：`max_prompt_tokens`（或 CLI `--max-ctx`）设定 user 多轮的上下文上限——
会话逐轮逼近上限，到顶即停（日志注明提前结束）；`bench probe` 会读取服务端
`max_model_len` 并对比计划压测的最大上下文（默认按 agent 基线 40K），超限直接告警。## 兼容性：多推理引擎支持

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
魔改引擎行为看一眼 raw 文件就清楚；未显式开启 debug 时不会把失败请求写入系统临时目录。

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
不中断、不降级措辞：外部分析应如实标注「客户端实测（基线）；未启用或端点未提供 /metrics」。
未知指标命名不再静默套用 vLLM 语义，只记录端点可达并跳过语义化服务端数据。

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
- **KV 容量画像**（12.12）：从 `vllm:cache_config_info`（info 型指标、配置在 label）提取 KV 池
  容量 / 满上下文口径并发上界 / block / dtype，落盘 `kv_capacity`——报告在容量拐点结论里并列
  「实测拐点 vs KV 上界」，先回答「是不是 KV 内存先满」再谈槽位；probe 的 /metrics 可用性检查
  也顺带提取（跑压测前就能拿到）
- 结果进 JSON（`server_metrics` 汇总 + 逐请求 `server_counter_delta`）与报告「服务端观测」章节

典型用法：TTFT 高时看命中率（低=缓存没生效）与 queue 时间（高=排队）、preemptions>0（KV 压力）。

## 其他

- **测试盐值**：`seed_salt: N`（CLI `--seed-salt`）给所有 prompt 种子叠加盐值——服务端 prefix cache
  是内存态且不会被挤出，同一配置重跑时"冷缓存"测量会被上次测试污染；**每次测试递增盐值**，
  或重启服务端清缓存（二选一）
- **连接层重试**：`retry: {max_attempts: 2, backoff_ms: 300}` 对瞬时失败（reset/5xx/429）重试，默认关闭；重试留痕 warnings/retry_count；失败/取消原始请求均保留
- **goodput**：`slo: {goodput: {ttft_ms: 2000, tpot_ms: 100}}` 定义 SLO，rps/concurrency 结果输出达标数与有效吞吐

## 快速开始

```bash
# 本机
make build-linux                    # 产出 bin/bench-linux-amd64
scp bin/bench-linux-amd64 configs/example.yaml 堡垒机:~/llm-perf/

# 堡垒机
mv bench-linux-amd64 bench && chmod +x bench
# 端点/key 直接写在配置文件里（endpoint + api_key 字面量；该配置勿入库）
./bench probe -c example.yaml       # ① 先探针：确认引擎兼容性与思考开关参数
./bench user -c example.yaml        # ② 生成式多轮：真实形状 + 动态 prefix cache
./bench rps -c example.yaml         # ③ 开环到达：排队-延迟曲线
./bench concurrency -c example.yaml # ④ 固定在飞：总吞吐拐点（vLLM 对比）
```

## 输出

每个子命令落一个 JSON 文件（含全部原始数据：逐 turn/请求计时、逐 chunk 派生指标、usage token）：

```bash
./bench user         -c example.yaml   # → llm-perf-test/output/user-<ts>.json（sessions，含 profile 标签）
./bench rps          -c example.yaml   # → llm-perf-test/output/rps-<ts>.json（requests，含 request_rate）
./bench concurrency  -c example.yaml   # → llm-perf-test/output/concurrency-<ts>.json（requests，含 level）
```

**按模型分区**：配置了多个模型时，数据按模型分区落 `<output_dir>/<模型>/<场景>-<ts>.json`
（模型名取 `/` 后末段），重测/作废单模型数据不纠缠；`run.log` 与 `raw/` 仍在 output_dir
顶层（测试级共享）。单模型配置（或 `-m` 过滤后只剩一个）保持原布局直接落 output_dir：

```bash
llm-perf-test/output/
├── run.log
├── DeepSeek-V4-Flash-0731/
│   ├── user-<ts>.json
│   └── rps-<ts>.json
└── Qwen3.8-27B/
    ├── user-<ts>.json
    └── concurrency-<ts>.json
```

多模型 + `-o xxx.json` 会报错（一个文件装不下多个分区），请给目录。

JSON 结构见 `internal/report/report.go` 与 [docs/data-contract.md](docs/data-contract.md)：
`multiturn`（user 会话）/ `concurrent`（rps/concurrency 档位）两个主场景数组，
每条主压测请求是 `engine.TurnMetrics`（含 `phase=benchmark`；流式含原始 chunk 序列 `content_times_ms`，
供 chunk 间隔和抖动等外部分析）；失败/取消请求也完整保留。观测层开启时附 `server_metrics` 汇总
与两源一致性 `source_check`。

**分析在工具之外**：聚合、判级、画图由消费方完成。体验基线三档阈值（`slo_baseline`）随 JSON
透出，判级不要内置自己的常量；报告判据的方法论依据见 [docs/latency-baselines.md](docs/latency-baselines.md)。

## 配置

见 `configs/example.yaml`，含详细注释。端点与认证优先写配置文件（`endpoint` + `api_key` 字面量）；
环境变量 `LLM_PERF_ENDPOINT`、`LLM_PERF_API_KEY` 优先级最高（应急通道；另有 `api_key_env` 指定从哪个变量读 key）。

**测试类别（`test:`）决定报告结论区问什么，不改测量**：`performance`（默认，留空即此）= 瓶颈在哪、
容量边界多远；`benchmark` = 标准格上这台部署处于什么水平（跨部署可比，预设配置见
`configs/benchmark.yaml`）；
`soak` = 长时间跑会不会退化/出事故（结论区给稳定性三问：事故 / 正确性 / 漂移——漂移判据要求
分时段长跑采样，当前用 rps 模式长时运行采集，缺证据处如实写 NA）。
三者共用同一套四个数与判据——换类别只换首屏口径，
详细数据仍在折叠附录里一个不少。

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
                    #   时长制 soak/错误路径/数据契约 schema_version+raw_timings；mock 含 /models、/metrics 与 tool-call 好路径）
                    # 运行产物全部落 /tmp 随脚本退出清理，不污染仓库
scripts/mock_server.py   # 本地 mock OpenAI 兼容流式服务（smoke.sh 底层依赖）
                    # 环境变量 MOCK_TOKEN_DELAY 可调每 chunk 间隔（默认 0.05≈20 tok/s），用来构造降速现场
                    # （与 smoke.sh 分开：这里的 mock 需要按请求区分快慢，探针必须秒回）
deploy/             # 推理服务 compose 存档：vLLM 基线 + SGLang / TensorRT-LLM / llama.cpp / TGI
                    # 引擎横评（各文件头部含与基线的逐参数对照与 bench 侧注意点）
```

## 设计文档

- [docs/architecture.md](docs/architecture.md) — 内部架构：模块边界、数据流、扩展点、已知坑位（二开/维护者向）
- [docs/data-contract.md](docs/data-contract.md) — 数据契约：JSON 输出结构、聚合口径、报告侧对齐规则
- [docs/latency-baselines.md](docs/latency-baselines.md) — 体验基线 3 档制的依据与原文链接
- [docs/testing-architecture.md](docs/testing-architecture.md) — 评测体系架构：指标第一性原理与冻结、四层模型、三类测试、trace/filler 分工、客户端计时立场、减法纪律（2026-09-12 定稿）
- [AGENTS.md](AGENTS.md) — 统一项目指南：项目边界、运行方式、设计拍板、验证纪律与工程陷阱
