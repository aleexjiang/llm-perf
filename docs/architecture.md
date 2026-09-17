# 内部架构设计

> 面向维护者/二开者：模块边界、数据流、扩展点、已知坑位。使用方法见 [README](../README.md)；
> 数据契约（JSON 输出与聚合口径）见 [data-contract.md](data-contract.md)；
> 设计决策与测量哲学见 [AGENTS.md](../AGENTS.md)；评测体系（指标/分层/减法）见 [testing-architecture.md](testing-architecture.md)；运行方式见 [CODEBUDDY.md](../CODEBUDDY.md)。
>
> **本文不写行号**——行号随每次提交漂移（历史教训），定位一律用函数/类型名 grep。

## 总体数据流

```
YAML 配置 (configs/*.yaml)
  │  config.Load（通用 + model_overrides 按模型差异合并）
  ▼
cmd/bench/main.go ── 组装 Client（auth/chat_path/retry/debug）、解析 --turns × --concurrency
  │                    probe 分支：engine.Probe → ProbeResult JSON（+ toolprobe 检查）
  ▼
scenario 层（internal/scenario）── 按组合派发 Multiturn / Concurrent（公共入口只运行多轮）
  │  每个场景：预热 → metrics 窗口(startWindow/finishWindow) → 请求循环 → 正确性金丝雀
  │  负载生成：filler（token 精确填充）或 trace（真实会话回放）
  ▼
engine 层（internal/engine）── Client.Chat：逐 chunk 计时 → TurnMetrics
  │  SSE 解析(sse.go) · filler/trace 负载(filler.go/trace.go) · /metrics 采集(smetrics)
  ▼
report 层（internal/report）── Report 结构落盘 JSON（按模型分区 PartitionByModel）
  │
  ▼  （离线，非本工具职责）
外部数据分析（pandas / notebook / 客户 BI）── 读场景 JSON + probe JSON + *.stall.csv 自行聚合
```

契约一句话：**Go 只产出原始数据（per-request/per-turn JSON + probe + stall.csv），所有聚合、判级、呈现都在工具之外**（分析侧能算的不碰 Go；2026-09-17 报告层已整体剥离，数据契约即对外接口，见 [data-contract.md](data-contract.md)）。

## 包职责

| 包 | 关键文件 | 职责 | 备注 |
|---|---|---|---|
| `cmd/bench` | main.go | CLI 入口：flag 解析、模式派发、Ctrl+C/SIGHUP 优雅中断、输出路径/模型分区 | `--concurrency cfg` 是字面量（读配置 concurrent.levels），不是文件路径 |
| `internal/config` | config.go | YAML 加载 + env 覆盖 + `model_overrides` 合并（ForModel）；`IntList` 支持标量/列表（输出长度扫描） | schema 不保向后兼容（决策见 AGENTS.md） |
| `internal/corpus` | corpus.go | 填充语料加载（en/zh/自定义路径），供 filler 构造 token 精确文本 | |
| `internal/auth` | auth.go | 认证方案抽象：bearer / 裸 key / 自定义 header / none，chat 与 /metrics 共用 | 替代了早期 3 处硬编码 `Bearer ` |
| `internal/engine` | client.go / sse.go / filler.go / trace.go / probe.go / toolprobe.go / corpus_filler.go | OpenAI 兼容客户端与逐 chunk 计时；SSE 解析；负载生成；兼容性探针 | 见下"engine 内部" |
| `internal/scenario` | scenario.go、plan.go（测试画像） | 多轮编排：Multiturn / Concurrent；闭环 runClosedRound / 开环 runOpenRound；混跑形状计划/聚合；正确性金丝雀；goodput；warmup/失败/取消完整采集；plan.go：PlanSummary 开跑前估算请求量 | 场景经 Register 注册表派发（main 不直接 import 各场景实现） |
| `internal/smetrics` | smetrics.go | 服务端 /metrics 采集（**可选的第二数据源**，客户端实测才是基线）：counter 差值 / gauge 轮询 / histogram 分位估计；指标名归一化（去 `_total`）。缺失或抓取失败一律不产生错误语义（`finishWindow` 返回 `Available=false`+`Note`，观测关闭返回 nil） | 引擎指标名表硬编码；指标名前缀自动识别引擎（vllm:/sglang:），**未识别显式告警、不静默回落 vLLM**（2026-09-08 修） |
| `internal/report` | report.go | JSON 输出结构定义与落盘：Report / SingleRow / MultiturnRun / ConcurrentLevel / AuxiliaryRequest / PartitionByModel | 主压测、warmup、correctness 原始数据分组落盘；只定义结构不做聚合 |

## engine 内部

- **client.go**：`Client.Chat` = 重试循环（RetryPolicy 只重试连接层瞬时失败：传输错误/5xx/429/流中断；4xx 是确定性行为不重试）→ `attempt` 组请求体（ExtraBody 只允许供应商扩展字段，不能覆盖 stream/max_tokens/stream_options）→ 流式走 `readStream`，非流式走 `readWhole` → `Finalize` 由原始时间戳算派生指标（TTFT/思考拆分/ITL 分位/TPOT）。缺少 `[DONE]`、body 读取失败和主动取消均保留完整指标与状态。自定义 Transport：连接池 256/128 + DisableCompression（压缩攒批破坏 ITL 精度）。
- **sse.go**：`ingestSSEBody` 是流解析唯一入口（时钟注入，生产 time.Now / 测试合成时钟）；`deltaPayload` 自定义 UnmarshalJSON 一次解析同时拿值和键名清单，`knownDeltaKeys` 白名单之外的键记 warnings（魔改引擎探测）；`tool_calls` 分片按 index 分桶聚合。
- **filler.go**：token 精确的合成填充。注意 `SystemMsg` 把工具定义当纯文本塞 system 消息——只模拟体积，不发真实 `tools` 字段（定位见 AGENTS.md tool-call 一节）。
- **trace.go**：`LoadTrace` 加载 ShareGPT / sessions 格式；`replay_mode` **默认 full**（按原序注入全部 role），`user_only` 只回放 user 轮（显式配置才生效）。
- **probe.go**：`Probe` 兼容性探针。核心是 **`ProbeCheck` 的证据分级**：`TierCore`（标准 OpenAI 兼容面，`/chat/completions`）才是配置基线；`TierExt`（`/metrics`、`/models` 的 `max_model_len`、`Server` 头）只作增强，服务端未提供时记 `NA`（`OK=true`），不判失败也不进通过率——很多服务经网关代理后没有这些端点，把缺失当故障会到处误报。`Tally`/`Summarize` 按分级出统计。另有 `buildSuggestedConfig` 把结论收敛成可粘回的 YAML（扩展面推导项一律注释掉）；认证自举（`auth_scheme`）与挂载点扫描（`chat_path`/`models_path`/`metrics_path`）只在明确被拒（401/403）或落空（404/405）时触发。URL 统一按 `origin + 绝对路径` 拼装（`splitOrigin`/`resolvePath`），候选挂载点因此能整体替换。
- **toolprobe.go**：tool-call 健康检查，verdict 分级 PASS/FAIL/WARN/INCOMPLETE（检查自身失败不计 ❌），带 `toolCallSelfCheckNotice` 自检提示常量。

## 关键数据结构

- **`engine.TurnMetrics`**（client.go）：单请求全量计时+token 统计，是所有场景行和 `auxiliary_requests` 的叶子单元。`phase` 标记 benchmark/warmup/correctness，主动取消用 `cancelled` 标记。注意两类字段的区别：
  - 带 `json:"-"` 的字段**不进压测数据**（`ToolCalls` 只被 probe 消费）——给 TurnMetrics 加字段时先想清楚是否污染压测 JSON；
  - `omitempty` 派生指标在非流式/无思考时缺省——报告侧必须容忍缺失。
- **`report.Report`**（report.go）：单场景单文件。主压测请求位于场景数组，warmup/correctness 完整指标位于 `AuxiliaryRequests`；`Environment`（引擎识别存档）与 `ConfigRaw`（配置原文）随每份 JSON 落盘——回看数据时"当时什么引擎什么配置"有据可查。
- **`smetrics.Sample`**：一次 /metrics 抓取快照；场景层 `startWindow/finishWindow` 取窗口差值挂到 Report.Server。

## 扩展点指引

| 要加什么 | 改哪里 |
|---|---|
| 新的请求级指标 | `TurnMetrics` 加字段 + `Finalize` 计算（注意 json:"-/omitempty 语义）；新增字段必须指认四个数（TTFT/decode 速度/goodput/canary）之一，否则 `json:"-"` 只进诊断面；同步更新 data-contract.md 并递增 `SchemaVersionCurrent` |
| 新的 probe 检查项 | `engine.ProbeOptions` + `probe.go` 的 ProbeCheck 列表（或 toolprobe.go 模式：独立文件 + verdict 分级）。**先判定证据来源**：只用标准面就选 `check`（核心），沾了引擎扩展面就选 `checkExt`/`checkExtNA`——别让可选端点拖累通过率 |
| 新场景/新负载模式 | scenario.go 注册新 Scenario 或扩展 Concurrent（闭环 runClosedRound / 开环 runOpenRound 已是两条成熟路径） |
| 配置新字段 | config.go 对应子结构 + Load 默认值；**不做向后兼容双轨** |
| 服务端新指标 | smetrics.go 指标名表加归一化名；未知引擎命名差异优先考虑告警而不是静默回落 |
| 数据契约结构变更 | report.go / TurnMetrics 改结构 → data-contract.md 同步 → `SchemaVersionCurrent` 递增；外部消费方按 schema_version 做兼容判断 |

## 已知坑位（改代码前先看）

1. **`--concurrency cfg` 是字面量**：main.go 据此读 `config.Concurrent.Levels`，与逗号数字列表两条解析路径。
2. **seed 规则**（scenario.go「种子派生」一节）：`fixed_seed` / 会话 / worker 三套确定性 seed 公式 + `--seed-salt` 测试隔离——重跑对照必须换盐，否则命中服务端前缀缓存。
3. **截断必须按 rune**：`TruncateRunes`（client.go）是唯一正确姿势，按字节切中文出半个 UTF-8 字符（历史 bug）。
4. **多模型分区**：`PartitionByModel`（report.go）+ main.go `modelDirName`（保留 Unicode，中文模型名不清洗成同形碰撞）；run.log 与 raw/ 留在 output_dir 顶层共享。
5. **run.log 追加不覆盖**：同目录多轮测试日志都要留得住。
6. **Bash grep 静默空**：本仓库维护中已多次踩到 shell grep 对部分模式返回空导致误判，扫描代码用结构化搜索工具并交叉验证。
