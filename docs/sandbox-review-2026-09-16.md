# 沙盘推演：数据采集与计算逻辑审查 + 死代码扫描（2026-09-16）

- **审查基线**：llm-perf @ `5f0fa61`（main）
- **审查方式**：全链路只读走查（cmd → engine → scenario → smetrics → report → gen_html_report.py）+ mock 实测重算 + 静态分析三件套（deadcode / pyflakes / vulture）
- **前置语境**：承接 ROADMAP §11（2026-09-12 全量审计）。§11 已收口的"接缝死重量"（plan / histograms / batch 等）本轮只复核现状，不重复展开；本文只记**新发现**与**复核结论**。
- **纪律**：本轮为只读沙盘推演，**未改动任何代码与文档**；行动清单待拍板。
- **落地**：行动清单（§五）已于 2026-09-16 全部实施（含回归断言：Go 单测 / fixtures I 组 / smoke 时长制画像断言），代码与文档随同日提交入库；本报告保留为审查过程记录。

## 结论摘要

| 维度 | 结论 |
|---|---|
| 采集计时链路 | ✅ 自洽——mock 实测 4 run 全部派生指标重算误差 < 0.05ms；空首 chunk / 负 think / "测不出 ≠ 0" 等边界处理正确 |
| 计算与聚合 | ⚠️ P2-1：单发 TTFT 图未剔除失败 run（与全文件其余统计口径不一致） |
| 决策类逻辑 | ⚠️ P2-2：recoveryProbe 模型选择未对齐 `-m` 子串语义（边缘组合下误判"服务端未恢复"） |
| 计划 / 展示 | P3×2：plan 画像不含时长制；时长制 soak 下"发车"列 span_s 失真 |
| 死代码 | Go 零死代码（deadcode 复核）；Python 侧新发现 6 项（1 死函数 + 4 死变量 + 1 组死 import，另有 2 个测试夹具死变量） |
| 落盘未消费字段 | 均属"设计内原始存档"（ROADMAP §11.3 已定性），非缺陷；建议 data-contract 补记 `server_counter_delta` 语义 |

## 一、数据流全链路（审查范围）

```
config.yaml ─> cmd/bench/main.go ─> internal/scenario ─> internal/engine ─> 结果 JSON ─> scripts/gen_html_report.py ─> HTML
                    │                     │                      │
                    │                     │                      ├─ sse.go   逐 chunk 计时
                    │                     │                      ├─ stall.go 单流中位判速（12.10）
                    │                     │                      └─ probe.go 健康检查 / KV 画像
                    │                     ├─ 统计：runOne / finalizeLevel / goodputOf / attachKVCapacity
                    │                     └─ 编排：单发 / 多轮 / 闭环 / 开环 / soak（10.5）
                    └─ 场景编排 / 熔断器 / 恢复探针 / 落盘分区
```

已通读（只读）：`cmd/bench/main.go`；`internal/{engine, scenario, smetrics, report, config, corpus}` 全部源文件；`scripts/{gen_html_report.py, report_fixtures_test.py, mock_server.py, smoke.sh}`；`docs/data-contract.md`、`ROADMAP.md`；配合实测回放验证。

## 二、采集逻辑审查（正确性）

### 2.1 计时口径 —— ✅ 自洽（含实测证据）

- TTFT 取 reasoning / content **首个含 token 包**的较早者（`client.go:251-269`）；role-only 空首 chunk 仅在全无 token 时兜底，原始首帧保留在 `first_chunk_at` 供核查——与 data-contract 口径 10 一致。
- `think_ms` = first_reasoning→first_content，负值钳 0 + 告警（`client.go:277-285`）；`decode_ms` 在 thinking 吃光预算时**清 0 让键消失**（`client.go:290-297`）——"测不出"与"真的是 0"可区分。
- **实测重算**：起 `scripts/mock_server.py`，跑 single 场景 2 档 × 2 run，用 `sent_at / first_chunk_at / end_at` 重算全部派生指标：
  - run 示例：`ttft_ms` 落盘 261.554 / 重算 261.551；`e2e_ms` 370.491 / 370.487；`tpot_ms` 54.469 / 54.468；`tokens_per_sec` 27.539 / 27.539
  - **全部 4 run 误差 < 0.05ms**（仅时间戳序列化精度差）→ 派生计算无算术缺陷。

### 2.2 usage 与兼容性告警 —— ✅

- `usage_missing / stream_ended_without_done / unknown_delta_fields` 三类告警齐备；usage 缺失时 token 类键整体消失而非填 0（连锁影响已在 data-contract §7 显式记录）。

### 2.3 服务端观测（smetrics）—— ✅ 边界处理到位

- NaN/±Inf 丢弃（防 JSON 序列化失败）；`_bucket/_sum/_count/_created/_info` 分支解析；`DiffCounters` 负增量钳 0（服务端重启归零场景）；`histQuantile` 桶边界插值；直方图多 label 合并近似已在文档标注为粗估。
- KV 容量画像（12.12）：从 `cache_config_info` info 屉提取、多系列取首（单引擎场景成立）；缺失返回 nil、消费方省略并列项——宽容缺失，不影响结论。

### 2.4 逐请求 server_counter_delta —— ✅ 机制正确（附语义注意点）

- 仅单发 / 串行多轮启用（`scenario.go:83, 180-182, 226-230`），**并发场景不启用**——避免把共享计数器的增量错记到单请求头上，设计正确。
- 实测：chat 驱动的 `generation_tokens` delta 每 run 恒 = 3 = 该请求真实产出 → 窗口对账精确。
- 注意点（已实测解释）：cache 类 delta 反映"抓取窗口内计数器实际变动"。mock 的 cache 计数器按**抓取次数**累加（`mock_server.py:43-59` 自述），加上 GaugePoller 的周期抓取落在请求窗口内，出现 2 倍于单请求的单位数——**夹具特性，非采集缺陷**；真实 vLLM 计数器由请求驱动，单发/串行窗口干净。
- 该字段当前无消费方。建议 data-contract 补一行语义说明：「并发场景恒缺（nil）；窗口可能含周期性抓取/其他流量的近似」。

### 2.5 取消 / 中断判别（12.11）—— ✅

- `roundCtx.Err() != nil` 区分"我们取消的"与"服务端失败"（手工中断不再误报止损）；`finalScrapeCtx` 独立 5s ctx 保证中断后结束快照仍可抓取。语义与 smoke 断言一致。

## 三、计算与聚合审查

### 3.1 分位 / 中位口径一致性 —— ✅

- Go `percentile` 线性插值、P50 偶数样本 = 两中值平均；与 Python `st.median`、`aggregateShapes` 同口径（data-contract §5，2026-09-10 已统一）。
- 报告侧 `MIN_PCT_SAMPLE=20` 门槛（不足退回中位 / 显示 `n<20`）在表格、图表、判级四处一致。

### 3.2 失败剔除口径 —— ⚠️ P2-1：单发 TTFT 图漏剔失败 run

- **位置**：`scripts/gen_html_report.py:1012`
- **现象**：`st.median([r.get("ttft_ms", 0) for r in e["runs"]])` 直接吃全量 runs；同一文件其余统计（表格 `:737`、并发图 `:890`、事件块 `:932`、四象限 `:679`）在 data-contract 口径 2 的要求下全部 `not r.get("error")` 过滤。
- **影响**：失败 run 无 `ttft_ms` → 按 0 计入 → 图中位数被拉低（图比表格"好看"）；失败越多偏差越大。仅图表展示层，不影响表格与结论数字。
- **建议修复**：与 `:737` 同口径过滤（`if not r.get("error") and r.get("ttft_ms") is not None`；空列表保留 None 不出点）。

### 3.3 recoveryProbe 模型选择 —— ⚠️ P2-2：与 `-m` 子串语义不一致

- **位置**：`cmd/bench/main.go:701-707`
- **现象**：`-m` 文档语义为"包含该子串的模型"（`main.go:196`），场景侧 `filterModels` 用 `strings.Contains`（`scenario.go:1661-1672`）；但 `probeModel` 用 `m == *modelFilter` 精确相等 → 用子串过滤（如 `-m mock` 命中 mock-model-a）时 probeModel 恒为空串。
- **影响**：熔断触发 + 冷却结束后，恢复探针用空模型发请求 → 请求失败 → 判定"🛑 服务端未恢复"并停止剩余场景（且把该结论写进已落盘 JSON 的 note）。属边缘组合（熔断触发 + 子串 `-m`），一旦命中是**错误决策**。
- **建议修复**：复用同一过滤逻辑取首个匹配（`filterModels(cfg.ActiveModels(), *modelFilter)`；空则回退 `ActiveModels()[0]`）。

### 3.4 计划画像与时长制 —— P3-1：plan 估算不含 `duration_seconds`

- **位置**：`internal/scenario/plan.go:191, 203`
- **现象**：闭环估算 `per := level * cc.RunsPerWorker`；10.5 时长制下实际请求量由墙钟决定（`runs_per_worker` 被忽略）。画像仍按 runs_per_worker 估算 → 请求总数失真（日志 + JSON `plan` 字段）。
- **影响**：纯展示（执行正确），但消费者会据此对规模/耗时产生错误预期。
- **建议**：时长效分支改为说明式输出（"按 ~Xs 墙钟，请求数取决于吞吐"）。

### 3.5 "发车"列在时长制 soak 下的失真 —— P3-2

- **位置**：`gen_html_report.py:628-634`（推导）+ `scenario.go:1257-1262`（落盘）
- **现象**：ramp 由 sessions 的 `start_offset_s / batch` 推导；10.5 renew 重开的会话同样记 offset（`scenario.go:1262`"时长制下偏移恒记"），而 batch 固定为 worker 初始批次 → `span_s = max(offs) - min(offs)` ≈ 整场 soak 时长，而不是初始爬坡窗口。
- **影响**：并发表显示"爬坡 N 批 / 数百秒"+ 注记"该窗口内尚处暂态" → 读者会把整场 soak 误读为暂态。展示层问题。
- **建议**：span 只取首批（batch==1）会话的 offset 差；或按"每批**首次**启动时刻"计算。

### 3.6 其他复核（未发现问题，记录免复查）

- 四象限聚合、soak 三问（漂移 / 稳态 / 事故时间轴）、goodput 只判已配置维度、两源一致性共同分母、TTFT 徽章"无权威锚点不判级"、横轴以服务端 usage 中位为准（偏差 >5% 并列配置值）——逻辑与文档一致。
- `lv.get("multiturn")` 防御性兜底（8 处）恒与 `or lv.get("sessions")` 配对；该键从未被 Go 落盘 → 无害冗余，不动。
- `_iso_parse` 对带时区 ISO 串解析正常（实测 `sent_at` 格式无单调时钟段）；时段分箱依赖运行机本地时区——同机同跑无碍，跨时区回读旧数据需注意（认知项，不改代码）。
- `Sample.Gauges` 快照数值不参与结论（仅计数与命名族探测，`smetrics.go:353-382 / scenario.go:119`）；gauge 观测走 `GaugePoller` 独立链路——设计如此，非死重量。

## 四、死代码与未消费字段

### 4.1 Go 侧：零死代码（复核）

- `go run golang.org/x/tools/cmd/deadcode@latest ./...` → 仅 `UnloadCorpus`（`internal/engine/corpus_filler.go:24`）。
- 实为**测试专用**：filler_test（4 处）/ probe_test（4 处）/ corpus_filler_test（2 处）共 10 处引用；deadcode 默认不把测试当入口 → **白名单，非死代码**。

### 4.2 Python 侧死代码清单（本轮新发现，pyflakes + vulture）

| 文件:行 | 符号 | 性质 | 工具/置信 |
|---|---|---|---|
| `gen_html_report.py:843` | `axis_lbl()`（嵌套闭包） | **死函数**：零调用；功能被 `meas_label` / `axis_size` 取代（"偏差 >5% 并列配置值"文案未接线） | vulture 60%（人工确认 0 调用） |
| `gen_html_report.py:1235` | `qname` | 死变量（赋值未读） | pyflakes |
| `gen_html_report.py:1237` | `has_think` | 死变量（曾用于列开关，现列名固定） | pyflakes |
| `gen_html_report.py:2191/2200` | `t_note` | 死变量：两处赋值均未读（一页纸表格行未消费该注记） | pyflakes / vulture |
| `gen_html_report.py:2652` | `l0` | 死变量（同一行 `l1` 在用） | pyflakes |
| `report_fixtures_test.py:21/23/25` | `copy` / `json` / `subprocess` | 未用 import | pyflakes 90% |
| `report_fixtures_test.py:49/150` | `quad_entries` / `slope_target` | 夹具内赋值未读 | vulture 100% |

**白名单（曾疑似、维持"非死"结论）**：`landing_cards`（`:2930` 有调用）；`UnloadCorpus`（测试专用）；`itl_p90/p95/max`、`first_chunk_at` 等落盘字段（§11.3 定性为原始存档）。

### 4.3 落盘未消费字段（复核 §11.3）

| 字段 | 消费情况 | 判定 |
|---|---|---|
| `server_counter_delta` / `content_preview` / `reasoning_preview` / `itl_max_ms` / `itl_p90_ms` / `itl_p95_ms` / `total_tokens` / `reasoning_tokens` | py 0 / smoke 0 / Go 测试 0 | 设计内原始存档（排障 / 契约证据），**保留** |
| `retry_count` / `stream_broken` | 报告不直接消费；但经 `warnings[]` / `error`（"stream broken: …"）通道兜底可见（data-contract 质量标记表） | 同上 |
| `SourceCheck.ClientTokens` | 报告只渲染 `client_tps / server_tps / deviation / window_seconds` | 对账证据，保留 |
| `ProbeResult.ServerMetrics`（字符串快照） | merge 显式跳过（`gen_html_report.py:329`"别混进来"） | probe 证据，保留 |

**结论**：字段层面不需要任何清理动作；真正值得顺手清理的只有 4.2 的死变量/死函数。

## 五、行动清单（已全部落地，2026-09-16）

| # | 级别 | 事项 | 改动面 |
|---|---|---|---|
| 1 | P2 | 单发 TTFT 图补失败剔除（`gen :1012`） | py 1 行 |
| 2 | P2 | recoveryProbe 对齐 `filterModels` 语义（`main.go:701`） | go 约 3 行 |
| 3 | P3 | plan 画像补时长制分支（`plan.go`） | go 少量 |
| 4 | P3 | "发车"span_s 改首批口径（`gen :631`） | py 1-2 行 |
| 5 | P3 | 清理 4.2 死代码（死函数 + 死变量 + import） | py 少量 |
| 6 | P3 | data-contract 补 `server_counter_delta` 语义 + 原始存档字段说明 | 文档 |

> 改动 1/2/4 建议附回归断言；按仓库纪律，`gen_html_report.py` 改动需跑 `python3 scripts/report_fixtures_test.py`，Go 侧改动跑完整验证基线（gofmt / build / vet / test / smoke）。

## 附：验证环境重建（可复现）

```bash
python3 scripts/mock_server.py &          # mock 服务（18099）
# /tmp/review-verify.yaml：endpoint=127.0.0.1:18099、single runs=2、prompt_tokens=[800,1600]
./bin/bench single -c /tmp/review-verify.yaml -o /tmp/review-out
# 以 sent_at/first_chunk_at/end_at 重算全部派生指标，与落盘值对照（误差 < 0.05ms）
```

**方法局限（如实声明）**：本地 mock + 静态分析为主；真实 vLLM 环境未重跑（历史实测数据与文档已交叉核对）；HTTP 异常路径（超时 / 断流）以代码走查 + 既有 smoke / fixtures 覆盖为准。
