# TODO — 代码改进待办

来源：2026-09-06 全量代码 review（健壮性/性能/可扩展性/设计模式）。
P0 项（gofmt、scenario 测试、lastPrompt 修复、重试策略）已完成于 `6291c69`，此处只记录剩余项。
每项含：问题、位置、建议方案、验收标准。做完请勾选并在提交信息里注明本文件章节号。

## P1（✅ 2026-09-06 全部完成，提交见 git log "P1"）

### 1. ✅ MetricsProvider 接口——观测层多引擎化
- 已实现：`MetricsProvider` 接口 + `VLLMProvider`（原命名表）+ `SGLangProvider`（草案：排队 gauge
  `num_running_reqs/num_queue_reqs/token_usage`，缓存 counter 留空待真机校准）+ `DetectProvider`
  （按指标名前缀自动识别，未知回落 vLLM）。`DiffCounters/HistDeltas/StartGaugePoller` 均带 provider；
  newEnv 抓一次样本自动识别并在日志注明命名。待办遗留：SGLang 缓存 counter 名接真机时补。
- **问题**：`internal/smetrics/smetrics.go` 的 `counterNames / gaugeNames / HistNames`（~L180-210）硬编码 vLLM 指标名
  （`vllm:prefix_cache_hits` 等）。接 SGLang / MindIE 时观测层要么测不到数据要么改代码。
- **建议**：抽 `MetricsProvider` 接口：`CounterNames() map[string][]string`（语义键→候选名列表）、
  `GaugeNames() map[string][]string`、`HistNames() []string`、`Available(ctx) (bool, string)`。
  vLLMProvider 为第一个实现；`bench probe` 已能识别引擎（`probe.go guessEngine`），据此选择 provider，
  识别失败时回落到 vLLM 命名并告警。配置可加 `metrics_provider: "vllm"|"sglang"|"auto"` 覆盖。
- **验收**：SGLang 实例（或 mock）上 probe 报告"观测层可用（sglang 命名）"，bench 产出命中计数；
  现有 smetrics 测试不改语义仍通过。

### 2. ✅ Scenario 接口 + 注册表——场景矩阵的扩展点
- 已实现：`Scenario` 接口 + `Register/Lookup/All`（保注册顺序），main.go 改查表分发，switch 消除。
  测试锁定 Lookup/All 语义。
- **问题**：`cmd/bench/main.go` 的 switch 硬编码场景分发；新增场景要同时改 main / scenario / report / gen_html_report.py 四处，
  且 `scenario.go` 的 `Single/Multiturn/Concurrent` 签名虽一致却没有形式化约束。
- **建议**：定义 `type Scenario interface { Name() string; Run(ctx, *config.Config, *engine.Client, string) (*report.Report, error) }`
  + `registry = map[string]Scenario`；main 变为查表执行；`bench all` 按注册顺序遍历。
- **验收**：新增一个空场景（如 `bench observe`）只需新增一个文件 + 一行注册，main 的 switch 消失。

### 3. ✅ run.log 追加化——战役日志被覆盖（真实踩过）
- 已实现：O_APPEND + 每次启动写 `===== campaign <RFC3339>（tool <版本>）=====` 分隔标记。
  冒烟验证：同目录连跑两轮，日志含 2 个战役标记。
- **问题**：`cmd/bench/main.go:122` 用 `os.Create` 打开 run.log，同目录第二轮测试会覆盖第一轮日志。
  2026-09-05 两轮对照测试时第一轮日志已实际丢失。
- **建议**：改为 `os.OpenFile(..., os.O_APPEND|os.O_CREATE)`，每次进程启动写一行分隔标记
  （`===== campaign 2026-09-06T02:07 seed_salt=1 =====`），或按启动时间戳轮转
  `run-<ts>.log` + `run.log` 符号链接（后者报告读取更简单，二选一）。
- **验收**：同一 output 目录连续跑两轮，两轮日志均可读，分隔标记含时间戳与 seed_salt。

### 4. ✅ GaugePoller 降级标注——观测失败要可见
- 已实现：Poller 记录成功/失败/连续失败/最后错误（`Health()`，`Degraded()` 判定：
  从未成功或连续失败 ≥5）；`ServerMetricsSummary.ObservationDegraded/ObservationNote`
  进 JSON 与报告（红色警示条）。顺带修复：轮询场景的 Scraper 重试置 0（快速失败，
  下一 tick 天然是重试——原来内嵌 3 次重试会让一次轮询阻塞 600ms+，降级延迟数秒才可见）。
- **问题**：`internal/smetrics/smetrics.go` GaugePoller 抓取失败完全静默（`once()` 里 err 直接 return）。
  观测层挂掉时报告里的 gauge 只是"样本少"，无法区分"服务空闲"与"观测失效"。
- **建议**：Poller 记录连续失败次数与首次失败原因；`Summary()` 增加返回
  `Degraded bool / LastError string / FailureCount int`；`report.ServerMetricsSummary` 增加对应字段，
  报告「服务端观测」章节在 degraded 时给出醒目提示。
- **验收**：人工停掉 /metrics（如改端口）跑一轮小场景，报告明确显示观测降级而非正常空数据。

## P2（择机，均为小改动）

### 5. ⏸ appendRaw 常开内存churn（性能项，按 2026-09-06 决定暂缓）
- **问题**：`internal/engine/sse.go:225` 每个流式请求把所有行追加进 256KB 环形缓冲（`maxRawKeep`），
  即便不开 debug。长流下多数追加是空转（达到上限后仍在做 len 检查与切片拼接）。
- **建议**：`DebugDir == ""` 时只保留头部 64KB（够失败排查用），或 appendRaw 加快路径判断。
- **验收**：基准 `BenchmarkIngestSSEBody`（新增）在 debug=false 时分配字节数下降。

### 6. ⏸ percentile 重复排序（性能项，按 2026-09-06 决定暂缓）
- **问题**：`internal/engine/client.go Finalize` 对同一 ITL 序列调用 6 次 `percentile`，
  每次内部 copy + sort（O(n log n) × 6）。
- **建议**：Finalize 里排序一次，百分位改索引取样（p50/p90/p95/p99/max 共用有序切片）。
- **验收**：现有 sse/client 测试不变通过；新增一个 Finalize 的 ITL 分位单测。

### 7. ✅ trace 超限报错（短期修复完成；流式解析仍开放）
- 已实现（2026-09-06）：非压缩文件先用 os.Stat 检查大小，超 512MB 给出可行动错误
  （"请预先切分或调低 dataset.max_sessions"）；解析失败的报错附截断提示。
- 仍开放（低优先）：`json.Decoder` 流式逐会话解析——除非真的要跑 600MB 级 ShareGPT 全量，否则不值得做。

### 8. ✅ probe 思考探测的 max_tokens 上限
- 已实现（2026-09-06）：`ProbeOptions.ThinkingBudget`（main 传 thinking.max_tokens_floor，
  probe 端 clamp 到 ≤1024），探测结果注明预算值。真机验证：
  `thinking_on: ... 125 字符（探测预算 1024tk）`。

### 9. ✅ 开环模式随机种子浮点截断碰撞
- 已实现（2026-09-06）：改用 `math.Float64bits(rate)`——任意相邻 rate 档位种子必不相同，
  且同一 rate 仍可复现。

### 10. ✅ report.Version 用 ldflags 注入
- 已实现（2026-09-06）：`var Version = "llm-perf/dev"` + Makefile `-ldflags` 注入
  `git describe --tags --always --dirty`。已验证产物内含 `llm-perf/86886e7-dirty`；
  未走 Makefile 的构建（go test / go build）显示 dev。

### 11. ✅ TraceSet.Pick 回绕静默
- 已实现（2026-09-06）：`env.warnTraceWrap(need)` 在 Multiturn / 闭环并发（multiturn）/ 开环
  （multiturn）启动时检查，会话数不足打 ⚠️ 日志。

### 12. ✅ report / cmd 包测试空白
- 已实现（2026-09-06）：report 测 SaveJSONAny 往返/嵌套目录/DefaultName 格式（66.7%）；
  cmd 测 resolveOutPath 三形态。cmd 整体覆盖率仍低（main/usage 未测），resolveOutPath 已覆盖。

## 方向性备忘（review 结论，不设时限）

- **设计模式现状**：时钟注入（sse）、Observer（GaugePoller）、Strategy（ThinkingVariant + extra_body 透传）、
  Registry（corpus）、Facade（engine.Client）已就位且用得对。
- **刻意不做**：不给 Client 抽纯 mock 接口（mock_server 集成测试更贴近真实协议）；
  不做非 OpenAI 传输层抽象（工具契约即 OpenAI 兼容）；smetrics 不改 event-driven。
- **测试策略**：引擎层用 httptest 桩 + 真实时钟；纯函数表驱动；协议解析用魔改流样例
  （sse_test 已有 8 例的表结构可扩展）。
