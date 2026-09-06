# TODO — 代码改进待办

来源：2026-09-06 全量代码 review（健壮性/性能/可扩展性/设计模式）。
P0 项（gofmt、scenario 测试、lastPrompt 修复、重试策略）已完成于 `6291c69`，此处只记录剩余项。
每项含：问题、位置、建议方案、验收标准。做完请勾选并在提交信息里注明本文件章节号。

## P1（下一次接新引擎 / 跑长战役前完成）

### 1. MetricsProvider 接口——观测层多引擎化
- **问题**：`internal/smetrics/smetrics.go` 的 `counterNames / gaugeNames / HistNames`（~L180-210）硬编码 vLLM 指标名
  （`vllm:prefix_cache_hits` 等）。接 SGLang / MindIE 时观测层要么测不到数据要么改代码。
- **建议**：抽 `MetricsProvider` 接口：`CounterNames() map[string][]string`（语义键→候选名列表）、
  `GaugeNames() map[string][]string`、`HistNames() []string`、`Available(ctx) (bool, string)`。
  vLLMProvider 为第一个实现；`bench probe` 已能识别引擎（`probe.go guessEngine`），据此选择 provider，
  识别失败时回落到 vLLM 命名并告警。配置可加 `metrics_provider: "vllm"|"sglang"|"auto"` 覆盖。
- **验收**：SGLang 实例（或 mock）上 probe 报告"观测层可用（sglang 命名）"，bench 产出命中计数；
  现有 smetrics 测试不改语义仍通过。

### 2. Scenario 接口 + 注册表——场景矩阵的扩展点
- **问题**：`cmd/bench/main.go` 的 switch 硬编码场景分发；新增场景要同时改 main / scenario / report / gen_html_report.py 四处，
  且 `scenario.go` 的 `Single/Multiturn/Concurrent` 签名虽一致却没有形式化约束。
- **建议**：定义 `type Scenario interface { Name() string; Run(ctx, *config.Config, *engine.Client, string) (*report.Report, error) }`
  + `registry = map[string]Scenario`；main 变为查表执行；`bench all` 按注册顺序遍历。
- **验收**：新增一个空场景（如 `bench observe`）只需新增一个文件 + 一行注册，main 的 switch 消失。

### 3. run.log 追加化——战役日志被覆盖（真实踩过）
- **问题**：`cmd/bench/main.go:122` 用 `os.Create` 打开 run.log，同目录第二轮测试会覆盖第一轮日志。
  2026-09-05 两轮对照测试时第一轮日志已实际丢失。
- **建议**：改为 `os.OpenFile(..., os.O_APPEND|os.O_CREATE)`，每次进程启动写一行分隔标记
  （`===== campaign 2026-09-06T02:07 seed_salt=1 =====`），或按启动时间戳轮转
  `run-<ts>.log` + `run.log` 符号链接（后者报告读取更简单，二选一）。
- **验收**：同一 output 目录连续跑两轮，两轮日志均可读，分隔标记含时间戳与 seed_salt。

### 4. GaugePoller 降级标注——观测失败要可见
- **问题**：`internal/smetrics/smetrics.go` GaugePoller 抓取失败完全静默（`once()` 里 err 直接 return）。
  观测层挂掉时报告里的 gauge 只是"样本少"，无法区分"服务空闲"与"观测失效"。
- **建议**：Poller 记录连续失败次数与首次失败原因；`Summary()` 增加返回
  `Degraded bool / LastError string / FailureCount int`；`report.ServerMetricsSummary` 增加对应字段，
  报告「服务端观测」章节在 degraded 时给出醒目提示。
- **验收**：人工停掉 /metrics（如改端口）跑一轮小场景，报告明确显示观测降级而非正常空数据。

## P2（择机，均为小改动）

### 5. appendRaw 常开内存churn
- **问题**：`internal/engine/sse.go:225` 每个流式请求把所有行追加进 256KB 环形缓冲（`maxRawKeep`），
  即便不开 debug。长流下多数追加是空转（达到上限后仍在做 len 检查与切片拼接）。
- **建议**：`DebugDir == ""` 时只保留头部 64KB（够失败排查用），或 appendRaw 加快路径判断。
- **验收**：基准 `BenchmarkIngestSSEBody`（新增）在 debug=false 时分配字节数下降。

### 6. percentile 重复排序
- **问题**：`internal/engine/client.go Finalize` 对同一 ITL 序列调用 6 次 `percentile`，
  每次内部 copy + sort（O(n log n) × 6）。
- **建议**：Finalize 里排序一次，百分位改索引取样（p50/p90/p95/p99/max 共用有序切片）。
- **验收**：现有 sse/client 测试不变通过；新增一个 Finalize 的 ITL 分位单测。

### 7. trace 大文件流式解析
- **问题**：`internal/engine/trace.go:88` 全量读入（512MB LimitReader 截断），ShareGPT 全量 ~600MB
  会被截断后 JSON 解析失败，报错信息有误导性。
- **建议**：短期——截断时报错改为"文件超过 512MB 上限，请预先切分或调低 max_sessions"；
  长期——`json.Decoder` token 流逐会话解析，边读边过滤 min_turns/max_sessions。
- **验收**：构造 600MB 文件时错误信息可指导用户行动；常规文件行为不变。

### 8. probe 思考探测的 max_tokens 上限
- **问题**：`internal/engine/probe.go:226` 思考探测硬编码 `max_tokens=512`，思考长的模型
  （如 Qwen3 对长 prompt 思考 1500+ token）会吃满预算导致误报"思考开关未生效"。
- **建议**：抬到 1024，或复用 config 的 `thinking.max_tokens_floor`（取 min(1024, floor)），
  探测结果 detail 里注明预算值。
- **验收**：对 Qwen3.8-27B probe，thinking_on 检查在长 prompt 下不再误报。

### 9. 开环模式随机种子浮点截断碰撞
- **问题**：`internal/scenario/scenario.go:571` `rand.NewSource(int64(rate * 1000))`——
  rate=0.5001 与 0.5002 截断后同种子，到达序列相同。
- **建议**：`int64(math.Float64bits(rate))`（稳定且无碰撞），或乘 1e6。
- **验收**：rate 0.5001/0.5002 生成的到达序列首个间隔不同。

### 10. report.Version 用 ldflags 注入
- **问题**：`internal/report/report.go` 的 `Version = "llm-perf/0.3"` 手工维护，
  发版容易忘升（JSON 里的 tool 字段用于追溯）。
- **建议**：`var Version = "dev"`，Makefile build 时 `-ldflags "-X .../report.Version=llm-perf/$(VERSION)"`，
  VERSION 取 git describe。
- **验收**：make build 产物运行时 JSON 的 tool 字段带 git 版本号。

### 11. TraceSet.Pick 回绕静默
- **问题**：`internal/engine/trace.go:111` 会话数不足时静默取模复用，日志无提示——
  测试覆盖的会话多样性比配置预期低时不易察觉。
- **建议**：Multiturn/Concurrent 启动时若 `sessions > len(trace.Sessions)` 打一行
  log 提示"会话将回绕复用 N 次"。
- **验收**：sessions=4 + trace 2 会话时日志出现回绕提示。

### 12. report / cmd 包测试空白
- **问题**：`internal/report`（130 行）与 `cmd/bench`（219 行）0% 覆盖。report 是纯结构 + SaveJSONAny；
  cmd 的 `resolveOutPath` 有分支逻辑值得测。
- **建议**：report 测 SaveJSONAny 的目录创建/JSON 可回读；cmd 测 resolveOutPath 三种输入形态
  （空/`.json` 后缀/目录）与 usage 分支。
- **验收**：report 与 cmd 覆盖率 > 60%。

## 方向性备忘（review 结论，不设时限）

- **设计模式现状**：时钟注入（sse）、Observer（GaugePoller）、Strategy（ThinkingVariant + extra_body 透传）、
  Registry（corpus）、Facade（engine.Client）已就位且用得对。
- **刻意不做**：不给 Client 抽纯 mock 接口（mock_server 集成测试更贴近真实协议）；
  不做非 OpenAI 传输层抽象（工具契约即 OpenAI 兼容）；smetrics 不改 event-driven。
- **测试策略**：引擎层用 httptest 桩 + 真实时钟；纯函数表驱动；协议解析用魔改流样例
  （sse_test 已有 8 例的表结构可扩展）。
