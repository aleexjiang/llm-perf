# 代码审查问题清单

> 审查日期：2026-09-17
> 审查范围：`cmd/`、`internal/engine`、`internal/scenario`、`internal/config`、`internal/report`、`internal/smetrics`、`internal/corpus`、`internal/auth`
> 状态标记：[ ] 未修复 / [x] 已修复

## 总体评价

工程成熟度高于一般压测工具水准：注释记录了大量踩坑决策，并发结构（levelRun 闸门、SWRR 混合档）大部分处理到位，未发现 goroutine 泄漏。主要风险集中在三类：

1. 连接复用缺陷动摇 TTFT 计时有效性（问题 1）；
2. 密钥泄露链（问题 2，客户环境必修）；
3. 配置/CLI 优先级的语义漏洞（问题 4）。

`smetrics` 的问题属于"单机单模型部署测不出、上真环境才炸"的潜伏型。

---

## 高优先级

### [ ] 1. 主压测路径几乎每请求重建 TCP 连接（两个缺陷叠加）

- 位置：
  - `internal/engine/client.go:85` — `MaxIdleConnsPerHost: 0`
  - `internal/engine/sse.go:271-273` — 收到 `[DONE]` 后直接 `break`
- 问题：
  - `MaxIdleConnsPerHost: 0` 注释写"不限"，实际 Go 回退到默认值 **2**，高并发下空闲连接被大量回收；
  - `[DONE]` 后 chunked 终止符未 drain，`Body.Close()` 走 earlyClose 路径，连接不归还连接池。
- 影响：stream=true 的主路径 TTFT 系统性混入 TCP/TLS 握手耗时，违反 `client.go:80-85` 注释里的设计目标，动摇 TTFT/ITL 计时有效性。
- 修法：`MaxIdleConnsPerHost` 显式给大值（如 512，配合 `MaxIdleConns` 调整）；`[DONE]` 后继续 `Scan()` 到 EOF，或返回前 `io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))` 再关闭。修复后用 netstat/服务端连接数指标验证压测期间连接数稳定。

### [ ] 2. api_key 字面量随报告 JSON 泄露

- 位置：`internal/config/config.go:691`（`cfg.Raw = string(data)`）→ `internal/report/report.go:315`（`ConfigRaw`）→ `cmd/bench/main.go:554`
- 问题：配置**原文**（含 `api_key: sk-xxx`）逐字写进每份分区 JSON，权限 0644；`PartitionByModel` 还会复制到每个模型分区，放大泄露面。
- 修法：`Load` 填 `cfg.Raw` 前脱敏 `api_key` / `api_key_env` 行的值（替换为 `<redacted>`）；`SaveJSONAny` 落盘权限改 0600。

### [ ] 3. Chat 可返回 `(nil, err)`，主路径缺 nil 保护 → panic 丢整场数据

- 位置：`internal/engine/client.go:443/450`（`return nil, err`）；调用方 `internal/scenario/scenario.go:231-236`、`scenario.go:840`、`scenario.go:1046`
- 问题：`runOne` 在 `err != nil` 时仍解引用 `m`；重试路径 `client.go:395` 在 ctx 取消时 `last` 也可能为 nil。Ctrl+C 恰好落在重试等待期时 panic，丢失整个场景的内存数据——与工具"中断优雅保存"的核心承诺冲突。`runCorrectness`（scenario.go:483）已写 nil 检查，主路径漏了。
- 修法：`runOne` 收口保证返回非 nil（`if m == nil { m = &engine.TurnMetrics{Error: err.Error()} }`），或三处调用点补 nil 检查。

### [ ] 4. CLI `--concurrency 2,4` 会被配置里的 `rate_sweep` 静默覆盖

- 位置：`cmd/bench/main.go:599-609` + `internal/scenario/scenario.go:1159/1168`
- 问题：CLI 只覆盖 `Concurrent.Levels`，未清 `RateSweep`/`RequestRate`；进入 Concurrent 场景后 `openRates(cc) != nil` 走开环 RPS。用户以为在测闭环并发，实际跑的是配置里的到达率扫描，**测出的结论与请求的实验不符**。`--concurrency 1` + 配置有 `rate_sweep` 时也会额外跑一轮 sweep，与文档语义冲突。
- 修法：显式 CLI 列表时同时清空 `RateSweep`/`RequestRate`；或两者同时存在时打印显著警告并说明实际生效的负载模型。

### [ ] 5. 失败请求默认把 prompt 转储到 `/tmp`，权限 0644、无清理

- 位置：`internal/engine/client.go:524-544`
- 问题：`DebugDir == ""` 且请求失败时写入 `os.TempDir()`。多用户机器上是敏感数据泄露面；大量失败时 /tmp 无限堆积文件且无清理；每次 dump 都 `os.MkdirAll`。
- 修法：默认不落盘（显式 opt-in）；文件权限 0600；MkdirAll 结果缓存。

---

## 中优先级

### [ ] 6. GaugePoller 从"跨系列求和"的 Counters 读 gauge，多系列时读数被放大

- 位置：`internal/smetrics/smetrics.go:194-196`、`smetrics.go:700-707`
- 问题：`Parse` 把所有非直方图指标同时写入 `Counters`（跨 label 系列累加）和 `Gauges`（单值覆盖），而 `GaugePoller.once` 查的是 `Counters`。多 label 系列（如 vLLM 新版按 `model_name` 分系列的 `num_requests_running` / `kv_cache_usage_perc`）时排队深度/KV 占用变成所有系列之和，污染 `waiting_max`（饱和止损标定数据源）与 `LatestWaiting`（实时止损判据）。单模型部署测不出，上真环境才炸。
- 修法：gauge 查询改走 `sample.Gauges`；或按 label 过滤系列。

### [ ] 7. Prometheus 行尾带 timestamp 时整行指标被静默丢弃

- 位置：`internal/smetrics/smetrics.go:219`、`smetrics.go:154/157`
- 问题：`splitMetricLine` 把 labels 之后的全部内容当 value，`ParseFloat("1.5 1712000000000")` 失败 → `continue` 静默丢弃。恰好是 counter 差值类关键指标，`Available`/两源一致性失真且无告警。
- 修法：value 取 `strings.Fields(line[sp+1:])[0]`。

### [ ] 8. tool-call 检查用原始 `o.Auth` 而非自举后的 `effAuth`

- 位置：`internal/engine/toolprobe.go:225`；对照 `probe.go:384-417` 认证自举
- 问题：`decode_speed`（662）、`filler_fidelity`（746）、缓存探针（839）都用 `effAuth`，唯独 `runToolCallCheck` 用原始配置。原始认证错误但自举成功时 T2/T3/T4 全部 401，写出误导性 WARN/FAIL。
- 修法：`runToolCallCheck` 增加 `effAuth auth.Auth` 参数并使用。

### [ ] 9. decode_speed 硬编码 120s 超时

- 位置：`internal/engine/probe.go:660`
- 问题：同文件 199-202 行刚把硬编码 120s 改成配置透传，这里又写死 `NewClient(origin, o.APIKey, 120*time.Second, ...)`，慢网关下该探测项假性失败。
- 修法：改用 `timeout`。

### [ ] 10. `api_key_env` 指向的环境变量未设置时静默降级为无认证

- 位置：`internal/config/config.go:713-715`
- 问题：变量不存在时得到空串，后续不带认证头。变量名打错/忘 export 时可能命中免认证网关"成功"跑完一整轮压测，报告看起来正常。
- 修法：`api_key_env` 非空但环境变量为空时追加 `cfg.Warnings` 或直接报错（fail-fast 更符合本文件其它校验口径）。

### [ ] 11. `thinking.max_tokens_floor: 0`（用户意图关闭下限保护）被静默改成 2048 且永远关不掉

- 位置：`internal/config/config.go:781-783`，配合 `config.go:366-372`
- 问题：`if cfg.Thinking.MaxTokensFloor <= 0 { = 2048 }` 把显式 0 clamp 成 2048；Load 之后 floor 恒 > 0，`MaxTokens` 里的 `t.MaxTokensFloor > 0` 分支永远为真，用户没有任何配置方式关闭"思考开启时抬高 max_tokens"。
- 修法：改 `*int` 区分"未写"（默认 2048）与"显式 0"（关闭）；或显式 0 时打 warning。

### [ ] 12. 饱和判据：观测缺失时不重置计时，与设计意图相悖

- 位置：`internal/scenario/saturation.go:158-163`（observe 闭包）对照 `saturation.go:36-48`
- 问题：decider 注释说"观测缺失时重置计时"，但闭包在 `!ok` 时提前 return，没调 `dec.observe`，`overSince` 保持不变。waiting 超阈 → /metrics 降级整段 window → 恢复后第一个 tick 立即 Trip，"持续超阈"没有被持续观测证明。
- 修法：`!ok` 时也调用 `dec.observe(0, false, now)`，保持"观测缺失即重置"的保守语义。

### [ ] 13. gzip 语料读取报错用错变量，真实错误被吞

- 位置：`internal/corpus/corpus.go:75-78`
- 问题：`data, err = io.ReadAll(zr)`，错误分支打印的是 `zerr`（恒 nil），输出 `%!w(<nil>)`，语料损坏无法排障。
- 修法：改为 `%w", err`。顺带 `defer zr.Close()`（低）。

### [ ] 14. chat 路径候选循环污染 `chatPath`

- 位置：`internal/engine/probe.go:355-362`
- 问题：循环内先把 `chatPath` 改成候选值再探测；所有候选都失败时 `chatPath` 停在最后一个候选上，错误信息和 `buildAbortConfig` 写的是从未成功的路径，误导排障。
- 修法：用局部变量试探，命中才赋回 `chatPath`。

---

## 低优先级 / 性能

### [ ] 15. sse scanner 每请求分配 1MB 初始缓冲

- 位置：`internal/engine/sse.go:261`
- 问题：`scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)`，万级并发就是 GB 级分配/GC 压力。动机（超长 tool_calls 行）只需要调大 max。
- 修法：`scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)`。

### [ ] 16. SSE delta 每行做两次 json.Unmarshal

- 位置：`internal/engine/sse.go:64-84`
- 问题：先按结构体解一遍、再解到 `map[string]json.RawMessage` 收集键名，与注释宣称"一次解析"矛盾；`Keys/UnknownKeys` 每行 sort 也不必要（多数 delta 只有 1~2 键）。
- 修法：用 json.Decoder token 流只扫顶层键名；键名清单输出前排一次即可。

### [ ] 17. percentile 重复排序与拷贝

- 位置：`internal/engine/client.go:296-306`、`345-359`
- 问题：`Finalize` 已对 `itl` 排序，`percentile` 每次又拷贝 + `sort.Float64s`，共 4 次多余排序/拷贝。
- 修法：排序一次后按索引取分位。

### [ ] 18. corpus 语料重复大块分配

- 位置：`internal/corpus/corpus.go:83-88`、`106-127`
- 问题：`[]rune(text)` 使语料 string + rune 双份驻留（英文语料约 32MB）；`Window` 每请求分配 targetChars rune 切片 + string 转换，大 prompt 下 MB 级瞬时分配。另 `Window` 负 seed 会 panic（`corpus.go:112`，导出 API 无防护）。
- 修法：Window 结果按（targetChars, seed）做小型 LRU 缓存，或 `strings.Builder` 直接写 string；负 seed 入口取绝对值。

### [ ] 19. `runOne` 用 `len([]rune(...))` 数字数

- 位置：`internal/scenario/scenario.go:203`
- 修法：`utf8.RuneCountInString(last.Content)`。

### [ ] 20. SWRR 反复重跑，O(n²)

- 位置：`internal/scenario/plan.go:253-259` + `internal/scenario/scenario.go:659-688`
- 问题：每次 `effectiveTurnsAt` 重跑一遍 SWRR，对 num_prompts 逐会话求和即平方级，num_prompts 大时画像计算明显变慢。
- 修法：SWRR 增量推进（一次遍历同时累计 sum 和各档计数），或缓存 profileAt 结果。

### [ ] 21. GaugePoller 样本无界增长 + 全量拷贝

- 位置：`internal/smetrics/smetrics.go:626-634`、`703`、`776-786`
- 问题：`samples` 只追加不清理，500ms 间隔多天 soak 每键约 17 万采样/天；`samplesSince` 每次全量拷贝尾段。
- 修法：环形缓冲或按时间窗裁剪。

### [ ] 22. `normalizeMaxTokens` 与 `normalizeLadder` 重复代码

- 位置：`internal/config/config.go:1341-1359` vs `1366-1383`；Load 内三段 max_tokens 模板（828/850/871）
- 问题：前 18 行完全重复；Load 里同一套模板抄三遍。可删约 60 行。
- 修法：抽 `sortDedupInts([]int) ([]int, bool)` 和 `applyMaxTokensDefault(where string, def IntList) error`。

### [ ] 23. Multiturn 逐轮循环与 `collectSessionTurns` 约 70 行近乎逐行复制

- 位置：`internal/scenario/scenario.go:818-889` vs `1026-1068`
- 问题：nextTurnTokens / estPrompt / KeepAssistant / ctxLimit 终止等逻辑复制且已出现细微差异（hook 检查与 KeepAssistant 顺序），后续修 bug 极易只改一边。
- 修法：抽公共 session 执行函数，Multiturn 传入 turn 计数与日志 hook。

### [ ] 24. appendRaw 无条件写入完整请求体

- 位置：`internal/engine/client.go:447`、`516-521`
- 问题：(a) 请求体可挤掉 256KB 封顶内的响应证据；(b) `string(payload)` 全量拷贝；(c) `rawResp` 在每个请求无条件累积（`sse.go:266` 逐行 `line + "\n"`），即使 DebugDir 为空。
- 修法：转储请求体用 `truncate(payload, N)`；`appendRaw` 仅在需要转储时启用；成功 dump 后 `rawResp` 置 nil。

### [ ] 25. 非 200 响应只读 2048 字节未 drain

- 位置：`internal/engine/client.go:468-477`
- 问题：连接不能复用，429/5xx 高发场景放大问题 1。
- 修法：`io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))` 后再返回。

### [ ] 26. chunk 时间戳取在 JSON 解析之后

- 位置：`internal/engine/sse.go:282`
- 问题：`clock()` 在 `parseSSEData` 完成后求值，时间戳混入解析耗时（µs 级，大 chunk/低配机下系统性偏移）。
- 修法：解析前取时钟，零成本改进。

### [ ] 27. 其它健壮性杂项

- `internal/engine/client.go:388` — 指数退避 `backoff << (i-1)` 极大 MaxAttempts（>35）时溢出为负，建议封顶 `i-1 < 30`；
- `cmd/bench/main.go:297-303` — run.log 初始化失败被静默吞掉（磁盘满/权限时既没日志也没提示），至少 log 一条到 stderr；
- `cmd/bench/main.go:551/569` — `run()` 内 `os.Exit(1)` 跳过 defer（lf.Close/cancel），且场景失败时已采集的 `rep` 不落盘；
- `internal/scenario/scenario.go:581` — trace 单发路径 `UserTurns[0]` 越界风险（`trace.go:259` 路径不保证非空），取值前判空跳过；
- `internal/scenario/saturation.go:67-69/99-103` — levelRun 墙钟 timer 已触发未执行的回调与 `finish()` 收尾竞态，可能出现"日志说墙钟触发但 Aborted 为空"；
- `internal/scenario/scenario.go:1355-1365` — `runOpenRound` 重复计算 n，删第二段；
- `internal/scenario/scenario.go:653` — 硬编码 `1.07` 与 `plan.go:26` 的 `planTemplateOverhead` 常量重复，口径易漂移；
- `internal/smetrics/smetrics.go:96-113` — Scrape 对 404/401/403 等确定性失败也重试 2 次，Available 探测白等 600ms+；
- `internal/smetrics/smetrics.go:259-265` — `parseLE` 解析失败返回 0，生成 LE=0 假桶污染分位数，应直接跳过；
- `internal/smetrics/smetrics.go:539-565` — `HistDeltas` 不做 nil 防御，与 `DiffCounters` 契约不一致；
- `internal/smetrics/smetrics.go:148-204` — `splitMetricLine` 每行分配 labels map，可判断含 `{` 再建 map；
- `internal/engine/trace.go:255` — `parseSessions` 要求首个对象 turns 非空，应逐对象过滤；
- `internal/engine/trace.go:70-75` — `.gz` 文件不预检大小（已文档化的取舍）；
- `internal/engine/corpus_filler.go:11` — 全局 `corpusRegistry` 无同步，测试与并发 Filler 同跑是数据竞争，建议 atomic.Pointer 或注释禁止；
- `internal/engine/toolprobe.go:295` — 冗余条件 `st3 == 0 || (err3 != nil && st3 == 0)`；
- `internal/engine/client.go:601` / `probe.go:476` — `truncate` 按字节切可能切出半个 UTF-8 字符，建议统一用 `TruncateRunes`；
- `internal/engine/filler.go:55-65` — zh 路径按 3× 字节构建后再截断，40k 档约 0.5MB 中间分配，可按 rune 计数追加；
- `internal/engine/probe.go:225/330`、`toolprobe.go:226` — 每次调用新建 `http.Client`，probe 序列内无连接复用；
- `internal/engine/sse.go:133-138` — 不支持 SSE 多行 data 事件（OpenAI 系不出现，建议告警文案提示）；
- `internal/report/report.go:359-374` — `PartitionByModel` 共享指针字段跨分区可变；`row.Model == ""` 造空分区；
- `internal/config/config.go:960/999/1473-1481` — 多轮深度估算 `base + turns*turn_tokens` 整型溢出无上界保护，turns 加上界或乘法前判溢出；
- `internal/config/config.go:292-301/423` — `VariantNames` 改 filter 再还原略绕，`ThinkingFor` 末行冗余赋值；
- `cmd/bench/main.go:510/520` — `scenario.Lookup` 忽略 `ok`，sc 理论可为 nil，加防御检查；
- `cmd/bench/main.go:502-508` — `ones` 切片永远只含一个 `1`，用 bool 即可；
- `cmd/bench/main.go:65-73` — usage 文本"排查模式"标题出现两次，应合并。

---

## 并发安全已确认无问题的部分

`levelRun` 的 `stopped atomic.Bool` + `mu` 分离读写；`startSaturationWatch` 的 `tripped`/`loggedOver` 仅观测 goroutine 内读写；`runClosedRound` 各 worker 经 `mu` 追加；`runOpenRound` 的 wg.Add/Wait 时序；`SaturationGuardCfg.Get*` nil 安全；`IncludeUsage/Stream` 在 Load 中填默认值。HTTP response body 常规路径均有 Close，未发现 goroutine 泄漏。
