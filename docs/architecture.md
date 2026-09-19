# 内部架构设计
> 面向维护者/二开者：模块边界、数据流、扩展点、已知坑位。使用方法见 [README](../README.md)；
> 数据契约（JSON 输出与聚合口径）见 [data-contract.md](data-contract.md)；
> 设计决策与测量哲学见 [AGENTS.md](../AGENTS.md)；评测体系（指标/分层/减法）见 [testing-architecture.md](testing-architecture.md)；运行方式见 [CODEBUDDY.md](../CODEBUDDY.md)；会话形状与场景拆分的决策记录见 [archive/workload-refactor-plan.md](archive/workload-refactor-plan.md)。
>
> **本文不写行号**——行号随每次提交漂移（历史教训），定位一律用函数/类型名 grep。

## 总体数据流

```
YAML 配置 (configs/*.yaml) + 外置 profile / request set
  │  config.Load（通用 + model_overrides 按模型差异合并）
  ▼
cmd/bench/main.go ── 子命令派发：probe / user / rps / concurrency（CLI 只做 flag，语义在场景层）
  │                    probe 分支：engine.Probe → ProbeResult JSON（+ toolprobe 检查）
  ▼
scenario 层（internal/scenario）── 场景经 Register 注册表派发
  │  user：profile + 经典书语料生成式多轮会话（assistant=被测模型真实回复，动态 prefix cache）
  │  rps / concurrency：request set 冻结快照；开环=独立泊松发射时钟+在飞闸门，闭环=共享游标
  │  每个场景：setupServerMetrics 观测装配 → startWindow/finishWindow 窗口差值 → 请求循环
  ▼
engine 层（internal/engine）── Client.Chat：逐 chunk 计时 → TurnMetrics
  │  SSE 解析(sse.go) · 语料窗口(corpus_filler.go/filler.go，probe 校准与 user 模式共用)
  │  ShareGPT 解析(sharegpt.go，request set 加载) · 探针(probe/toolprobe/cacheprobe)
  ▼
report 层（internal/report）── Report 结构落盘 JSON（按模型分区 PartitionByModel）
  │
  ▼  （离线，非本工具职责）
外部数据分析（pandas / notebook / 客户 BI）── 读场景 JSON + probe JSON 自行聚合
```

契约一句话：**Go 只产出原始数据（per-request/per-turn JSON + probe），所有聚合、判级、呈现都在工具之外**（分析侧能算的不碰 Go；2026-09-17 报告层已整体剥离，数据契约即对外接口，见 [data-contract.md](data-contract.md)）。

## 包职责

| 包 | 关键文件 | 职责 | 备注 |
|---|---|---|---|
| `cmd/bench` | main.go | CLI 入口：子命令派发（probe/user/rps/concurrency）、flag 解析、Ctrl+C/SIGHUP 优雅中断、输出路径/模型分区 | 场景语义不进 main——main 只按注册表派发 |
| `internal/config` | config.go | YAML 加载 + env 覆盖 + `model_overrides` 合并（ForModel）；`IntList` 支持标量/列表（输出长度扫描）；thinking 变体派生（Variants，含关闭态 enable_thinking 兜底注入） | schema 不保向后兼容（决策见 AGENTS.md） |
| `internal/corpus` | corpus.go + data/books/ | 公版书语料库（12 本 en/zh）：按书加载、确定性取窗（Window）、CharsPerToken 校准系数 | 语料是 user 模式生成器的文本原料，不是压测数据源 |
| `internal/auth` | auth.go | 认证方案抽象：bearer / 裸 key / 自定义 header / none，chat 与 /metrics 共用 | 替代了早期 3 处硬编码 `Bearer ` |
| `internal/engine` | client.go / sse.go / sharegpt.go / filler.go / corpus_filler.go / probe.go / toolprobe.go / cacheprobe.go | OpenAI 兼容客户端与逐 chunk 计时；SSE 解析；ShareGPT request set 加载；语料窗口；兼容性探针 | 见下"engine 内部" |
| `internal/scenario` | scenario.go / user.go / requests.go / profile.go | 场景编排：user（profile 驱动生成式会话）、rps/concurrency（冻结快照 + barrier 调度）；观测装配；SWRR 档位分派；goodput；warmup/失败/取消完整采集 | 场景经 Register 注册表派发（main 不直接 import 各场景实现） |
| `internal/smetrics` | smetrics.go | 服务端 /metrics 采集（**可选的第二数据源**，客户端实测才是基线）：counter 差值 / gauge 轮询（label-aware、固定容量缓冲）/ histogram 分位估计 | 缺失或抓取失败一律不产生错误语义（`finishWindow` 返回 `Available=false`+`Note`，观测关闭返回 nil） |
| `internal/report` | report.go | JSON 输出结构定义与落盘：Report / MultiturnRun / ConcurrentLevel / AuxiliaryRequest / PartitionByModel / ServerMetricsSummary | 主压测、warmup、correctness 原始数据分组落盘；只定义结构不做聚合 |

## engine 内部

- **client.go**：`Client.Chat` = 重试循环（RetryPolicy 只重试连接层瞬时失败：传输错误/5xx/429/流中断；4xx 是确定性行为不重试）→ `attempt` 组请求体（ExtraBody 只允许供应商扩展字段，不能覆盖 model/messages）→ 流式走 `readStream`，非流式走 `readWhole` → `Finalize` 由原始时间戳算派生指标（TTFT/思考拆分/ITL 分位/TPOT）。缺少 `[DONE]`、body 读取失败和主动取消均保留完整指标与状态。raw 请求/响应只在显式 DebugDir 下留存（0600）；极短输出轮（completion<8 且 stop）decode 指标置空（噪声防护）。
- **sse.go**：`ingestSSEBody` 是流解析唯一入口（时钟注入，生产 time.Now / 测试合成时钟）；`[DONE]` 后继续消费到 EOF（连接可复用）；`knownDeltaKeys` 白名单之外的键记 warnings（魔改引擎探测）；`tool_calls` 分片按 index 分桶聚合。
- **sharegpt.go**：request set 加载（ShareGPT conversations → 冻结 prompt 快照 + 输出预算），供 rps/concurrency。
- **filler.go / corpus_filler.go**：语料窗口取文本（语料未注册时回退合成词表）。filler 不再是正式压测数据源——现在只服务 probe 的 filler_fidelity 校准（与 user 模式同口径）与测试 mock。
- **probe.go**：`Probe` 兼容性探针。核心是 **`ProbeCheck` 的证据分级**：`TierCore`（标准 OpenAI 兼容面，`/chat/completions`）才是配置基线；`TierExt`（`/metrics`、`/models` 的 `max_model_len`、`Server` 头）只作增强，服务端未提供时记 `NA`（`OK=true`），不判失败也不进通过率。另有 `buildSuggestedConfig` 把结论收敛成可粘回的 YAML；认证自举与挂载点扫描只在明确被拒或落空时触发。URL 统一按 `origin + 绝对路径` 拼装。
- **toolprobe.go / cacheprobe.go**：tool-call 健康检查（verdict 分级 PASS/FAIL/WARN/INCOMPLETE）；prefix cache 定性探针（`--cache` 开启时）。

## scenario 内部

- **user.go**：`runOneUserSession` = profile 定形状（档位 SWRR 分派、turns/增量采样）→ 语料定内容（system 基座一次定型冻结 + user/context 尾部注入，cache 安全双约束）→ 被测模型真实 assistant 进 history。失败轮与空回复轮（finish=stop 无可见输出）终止会话避免连续 user。stagger_ms 支持会话启动错峰。
- **requests.go**：rps/concurrency 共用 request set + `runRequestBarrier` 调度——闭环（request_rate=0）共享游标有空位即发；开环（request_rate>0）独立发射协程按泊松过程注入容量=level 的队列（到达率与处理耗时解耦，vLLM 语义）。
- **profile.go**：profile.json 结构与校验（轻/中/重档位、轮次范围、首轮 ≈30K 硬约束）；由外部 profile-build 脚本从 raw trace 提炼（仓库不含原始数据）。

## 关键数据结构

- **`engine.TurnMetrics`**（client.go）：单请求全量计时+token 统计，是所有场景行和 `auxiliary_requests` 的叶子单元。`phase` 标记 benchmark/warmup/correctness，主动取消用 `cancelled` 标记，`new_tokens` 由场景层推进（user 会话逐轮增量）。注意两类字段的区别：
  - 带 `json:"-"` 的字段**不进压测数据**（`ToolCalls` 只被 probe 消费）——给 TurnMetrics 加字段时先想清楚是否污染压测 JSON；
  - `omitempty` 派生指标在非流式/无思考时缺省——报告侧必须容忍缺失。
- **`report.Report`**（report.go）：单场景单文件。user 场景主数据在 `multiturn[]`（sessions 语义），rps/concurrency 在 `concurrent[]`；warmup/correctness 完整指标位于 `AuxiliaryRequests`；`ConfigRaw`（配置原文，认证字段已脱敏）随每份 JSON 落盘。普通 bench 不隐式发送 probe，环境能力结果由独立 `bench probe` JSON 提供。
- **`smetrics.Sample`**：一次 /metrics 抓取快照；场景层 `startWindow/finishWindow` 取窗口差值挂到 Report.Server。

## 扩展点指引

| 要加什么 | 改哪里 |
|---|---|
| 新的请求级指标 | `TurnMetrics` 加字段 + `Finalize` 计算（注意 json:"-/omitempty 语义）；新增字段必须指认四个数（TTFT/decode 速度/goodput/canary）之一，否则 `json:"-"` 只进诊断面；同步更新 data-contract.md 并递增 `SchemaVersionCurrent` |
| 新的 probe 检查项 | `engine.ProbeOptions` + `probe.go` 的 ProbeCheck 列表（或 toolprobe.go 模式：独立文件 + verdict 分级）。**先判定证据来源**：只用标准面就选 `check`（核心），沾了引擎扩展面就选 `checkExt`/`checkExtNA`——别让可选端点拖累通过率 |
| 新场景 | 实现 `func(ctx, cfg, client, modelFilter) (*report.Report, error)` + `Register(funcScenario{...})` 注册；通用能力（观测窗口、SLO、中断）从 scenario.go 取 |
| 配置新字段 | config.go 对应子结构 + Load 默认值；**不做向后兼容双轨** |
| 服务端新指标 | smetrics.go 指标名表加归一化名；未知引擎命名差异优先考虑告警而不是静默回落 |
| 数据契约结构变更 | report.go / TurnMetrics 改结构 → data-contract.md 同步 → `SchemaVersionCurrent` 递增；外部消费方按 schema_version 做兼容判断 |

## 已知坑位（改代码前先看）

1. **prefix cache 的两条硬约束**（user 场景，详见 archive/workload-refactor-plan.md 13.4）：synthetic context 只能尾部注入、严禁进 system；system 基座会话开始一次定型，禁止时间戳/随机数。违反任何一条，会话内 cache 全部失效。
2. **thinking 关闭必须显式**：`Variants()` 对无 extra_body 的关闭态变体注入 `enable_thinking=false`——部署默认 thinking=auto 时长上下文会把输出预算全吃进思考链（真机坐实）。改这里时保留显式值优先。
3. **seed 规则**：`fixed_seed` / 会话 / worker 三套确定性 seed 公式 + `--seed-salt` 测试隔离——重跑对照必须换盐，否则命中服务端前缀缓存。
4. **截断必须按 rune**：`TruncateRunes`（client.go）是唯一正确姿势，按字节切中文出半个 UTF-8 字符（历史 bug）。
5. **多模型分区**：`PartitionByModel`（report.go）+ main.go `modelDirName`（保留 Unicode，中文模型名不清洗成同形碰撞）；run.log 与 raw/ 留在 output_dir 顶层共享。
6. **run.log 追加不覆盖**：同目录多轮测试日志都要留得住。
7. **Bash grep 静默空**：本仓库维护中已多次踩到 shell grep 对部分模式返回空导致误判，扫描代码用结构化搜索工具并交叉验证。
