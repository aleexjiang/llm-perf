# 内部架构设计

> 面向维护者/二开者：模块边界、数据流、扩展点、已知坑位。使用方法见 [README](../README.md)；
> 数据契约（JSON 输出与聚合口径）见 [data-contract.md](data-contract.md)；
> 设计决策与测量哲学见 [AGENTS.md](../AGENTS.md)；能力规划见 [ROADMAP.md](../ROADMAP.md)。

## 总体数据流

```
YAML 配置 (configs/*.yaml)
  │  config.Load（通用 + model_overrides 按模型差异合并）
  ▼
cmd/bench/main.go ── 组装 Client（auth/chat_path/retry/debug）、解析 --turns × --concurrency
  │                    probe 分支：engine.Probe → ProbeResult JSON（+ toolprobe 检查）
  ▼
scenario 层（internal/scenario）── 按组合查表派发 Single / Multiturn / Concurrent
  │  每个场景：预热 → metrics 窗口(startWindow/finishWindow) → 请求循环 → 正确性金丝雀
  │  负载生成：filler（token 精确填充）或 trace（真实会话回放）
  ▼
engine 层（internal/engine）── Client.Chat：逐 chunk 计时 → TurnMetrics
  │  SSE 解析(sse.go) · filler/trace 负载(filler.go/trace.go) · /metrics 采集(smetrics)
  ▼
report 层（internal/report）── Report 结构落盘 JSON（按模型分区 PartitionByModel）
  │
  ▼  （离线，非 Go 职责）
scripts/gen_html_report.py ── 读多份 JSON → merge/analyze → 自包含 HTML + perf-summary
```

契约一句话：**Go 只产出原始 JSON；所有聚合、判级、报告呈现都在 Python 报告侧**（报告侧能算的不碰 Go）。

## 包职责

| 包 | 关键文件 | 职责 | 备注 |
|---|---|---|---|
| `cmd/bench` | main.go（515 行） | CLI 入口：flag 解析、模式派发、Ctrl+C 优雅中断、输出路径/模型分区 | `--concurrency cfg` 是字面量（读配置 concurrent.levels），不是文件路径 |
| `internal/config` | config.go | YAML 加载 + env 覆盖 + `model_overrides` 合并（ForModel）；`IntList` 支持标量/列表（输出长度扫描） | schema 不保向后兼容（决策见 AGENTS.md） |
| `internal/corpus` | corpus.go | 填充语料加载（en/zh/自定义路径），供 filler 构造 token 精确文本 | |
| `internal/auth` | auth.go | 认证方案抽象：bearer / 裸 key / 自定义 header / none，chat 与 /metrics 共用 | 替代了早期 3 处硬编码 `Bearer ` |
| `internal/engine` | client.go / sse.go / filler.go / trace.go / probe.go / toolprobe.go / corpus_filler.go | OpenAI 兼容客户端与逐 chunk 计时；SSE 解析；负载生成；兼容性探针 | 见下"engine 内部" |
| `internal/scenario` | scenario.go（1111 行）、plan.go（战役画像） | 三场景编排：Single(374) / Multiturn(505) / Concurrent(700)；闭环(885)/开环(946)；混跑形状计划(803)/聚合(839)；正确性金丝雀(317)；goodput(296-316)；plan.go：PlanSummary 开跑前估算请求量（复用 ClampLadder/MaxTokensList/Variants，展示口径=执行口径） | 场景经 Register 注册表派发（main 不直接 import 各场景实现） |
| `internal/smetrics` | smetrics.go | 服务端 /metrics 观测层：counter 差值 / gauge 轮询 / histogram 分位估计；指标名归一化（去 `_total`） | 引擎指标名表(249-298)硬编码，未知引擎回落 vLLM(302-307) 无告警（ROADMAP 附录 C） |
| `internal/report` | report.go | JSON 输出结构定义与落盘：Report / SingleRow / MultiturnRun / ConcurrentLevel / PartitionByModel | 只定义结构不做聚合 |

## engine 内部

- **client.go**：`Client.Chat`(307) = 重试循环（RetryPolicy 只重试连接层瞬时失败：传输错误/5xx/429/流中断；4xx 是确定性行为不重试）→ `attempt`(357) 组请求体（ExtraBody 透传只挡 model/messages）→ 流式走 `readStream`，非流式走 `readWhole` → `Finalize`(223) 由原始时间戳算派生指标（TTFT/思考拆分/ITL 分位/TPOT）。自定义 Transport：连接池 256/128 + DisableCompression（压缩攒批破坏 ITL 精度）。
- **sse.go**：`ingestSSEBody` 是流解析唯一入口（时钟注入，生产 time.Now / 测试合成时钟）；`deltaPayload`(35) 自定义 UnmarshalJSON 一次解析同时拿值和键名清单，`knownDeltaKeys`(19) 白名单之外的键记 warnings（魔改引擎探测）；`tool_calls` 分片按 index 分桶聚合。
- **filler.go**：token 精确的合成填充。注意 `SystemMsg`(74) 把工具定义当纯文本塞 system 消息——只模拟体积，不发真实 `tools` 字段（定位见 AGENTS.md tool-call 一节）。
- **trace.go**：`LoadTrace`(61) 加载 ShareGPT / sessions 格式；`replay_mode: full` 按原序注入全部 role，user_only 只回放 user 轮。
- **probe.go**：`Probe`(101) 兼容性探针——引擎识别（Server 头猜测）、上下文探测、usage 检查等 ProbeCheck 列表。
- **toolprobe.go**：tool-call 健康检查，verdict 分级 PASS/FAIL/WARN/INCOMPLETE（检查自身失败不计 ❌），带 `toolCallSelfCheckNotice`(37) 自检提示常量。

## 关键数据结构

- **`engine.TurnMetrics`**（client.go:102）：单请求全量计时+token 统计，是所有场景行的叶子单元。注意两类字段的区别：
  - 带 `json:"-"` 的字段**不进压测数据**（`ToolCalls` 只被 probe 消费）——给 TurnMetrics 加字段时先想清楚是否污染压测 JSON；
  - `omitempty` 派生指标在非流式/无思考时缺省——报告侧必须容忍缺失。
- **`report.Report`**（report.go:118）：单场景单文件。`Environment`（引擎识别存档）与 `ConfigRaw`（配置原文）随每份 JSON 落盘——回看数据时"当时什么引擎什么配置"有据可查。
- **`smetrics.Sample`**：一次 /metrics 抓取快照；场景层 `startWindow/finishWindow`（scenario.go:251/264）取窗口差值挂到 Report.Server。

## 扩展点指引

| 要加什么 | 改哪里 |
|---|---|
| 新的请求级指标 | `TurnMetrics` 加字段 + `Finalize` 计算（注意 json:"-/omitempty 语义）→ 报告侧 gen_html_report.py 的 `analyze()` 消费 |
| 新的 probe 检查项 | `engine.ProbeOptions` + `probe.go` 的 ProbeCheck 列表（或 toolprobe.go 模式：独立文件 + verdict 分级） |
| 新场景/新负载模式 | scenario.go 注册新 Scenario 或扩展 Concurrent（闭环 runClosedRound / 开环 runOpenRound 已是两条成熟路径） |
| 配置新字段 | config.go 对应子结构 + Load 默认值；**不做向后兼容双轨** |
| 报告新章节 | gen_html_report.py：analyze() 出数据 → 新 render 函数 → main() 组装；判级常量收敛到 SLO_TIERS(150) |
| 服务端新指标 | smetrics.go 指标名表加归一化名；未知引擎命名差异优先考虑告警而不是静默回落 |

## 已知坑位（改代码前先看）

1. **`--concurrency cfg` 是字面量**：main.go 据此读 `config.Concurrent.Levels`，与逗号数字列表两条解析路径。
2. **seed 规则**（scenario.go:1067-1082）：`fixed_seed` / 会话 / worker 三套确定性 seed 公式 + `--seed-salt` 战役隔离——重跑对照必须换盐，否则命中服务端前缀缓存。
3. **截断必须按 rune**：`TruncateRunes`（client.go:207）是唯一正确姿势，按字节切中文出半个 UTF-8 字符（历史 bug）。
4. **多模型分区**：`PartitionByModel`（report.go:166）+ main.go `modelDirName`（保留 Unicode，中文模型名不清洗成同形碰撞）；run.log 与 raw/ 留在 output_dir 顶层共享。
5. **run.log 追加不覆盖**（main.go:233）：同目录多轮测试日志都要留得住。
6. **Bash grep 静默空**：本仓库维护中已多次踩到 shell grep 对部分模式返回空导致误判，扫描代码用结构化搜索工具并交叉验证。
