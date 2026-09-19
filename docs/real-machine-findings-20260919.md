# 真机测试发现记录（2026-09-19）

> 状态：已全部修复（2026-09-19 第二批）。各项修复对应关系见第四节；真机回归验证：
> server_metrics 落盘（available=true）、new_tokens 非零、思考默认关闭、空回复轮终止。
>
> 环境：vLLM 直连 `<real-endpoint>/v1`，模型 `qwen3.8-27b`（Qwen3.8-27B-FP8），
> `max_model_len=262144`，/metrics 可达（567 项 vLLM 指标）。
> 工具版本：commit 34a58d0 + user 场景修复（shared_base seed / heavy turn bounds / 失败终止）。
>
> 测试矩阵：user 场景 8 组 profile 变体（首轮 token × 每轮增量 × 轮数）× 2 用户，
> thinking off（两组对照：未显式关闭 vs 显式 `enable_thinking=false`）。
> rps/concurrency 各 1 组最小冒烟（mini fixture）。
> 原始数据：`/tmp/real-*.json`（含 batch1/2/3 与 fix 复测组），临时目录，重启即失。

---

## 一、HIGH：影响 user 模式结论可信度的问题

### 1.1 部署默认 thinking=auto，未显式关闭时 user 多轮完全失真

- 现象：batch1/2 全部轮次 `reasoning_chars=500-690、content_chars=0、finish=length`——
  128 token 输出预算全被思考链吃掉，content 为空。
- 根因：配置 `thinking.mode: off` 且未写 `extra_body_off` 时，请求**不带任何思考参数**，
  依赖部署默认；而该部署（Qwen3.8 chat template）默认 `enable_thinking=true`，
  长上下文输入下模型自行决定思考 → 输出耗尽。短 prompt 手工测试则正常出 content
  （模型自行选择不思考），行为不一致、隐蔽性极强。
- 后果链：content 空 → assistant 不进 history → 每轮 prompt 只增长 user+context
  （动态 prefix cache 的核心卖点失效）→ 且产生连续 user 消息（违反方案 A 不变量）。
- 验证：显式 `extra_body_off: {chat_template_kwargs: {enable_thinking: false}}` 后
  content 正常（467/503/575 字符），assistant 正常进 history。
- **结论：user/rps/concurrency 模式必须显式传 enable_thinking=false（或 true），
  不能依赖部署默认。建议：thinking.extra_body_off 为空时对已知 Qwen 模板自动兜底，
  或至少在报告 Note 中显式声明"未传思考参数，按部署默认"。**

### 1.2 偶发空回复轮：content=0 + finish=stop + 无 error，未识别为异常

- 现象（fix 组，显式关思考后仍出现）：fix-t3 用户1 t3、fix-t4 用户1 t9——
  `content=0、reasoning=0、finish=stop、error 为空、ttft_ms 缺失`（JSON 字段级缺失）。
- 频率：fix 组 45 轮中 3 轮 ≈ 7%，均为长上下文轮（87K/193K）。
- 后果：该轮 assistant 为空 → 下一轮连续 user 消息（同 1.1 后果）；
  且 `ttft_ms` 字段整体缺失（omitempty），外部分析需按 key 存在性防御。
- **结论：① TurnMetrics 需要一个"空回复轮"标记（finish=stop 且 content=0 且非取消），
  并计入会话异常；② 空回复轮同样应终止会话（与失败轮同语义）；
  ③ ttft_ms 等字段对非流式/异常轮的缺省行为需要文档化。**

### 1.3 user 场景 server_metrics 观测层从未装配（代码 bug）

- 现象：8 组 JSON 的 `server_metrics.available` 全部为 None，日志无任何观测层输出；
  而 /metrics 可达且含 567 项 vLLM 指标（curl 验证）。
- 根因：`internal/scenario/user.go` 手工构造 `env{...}` 后**未调用 setupServerMetrics**——
  e.srv 恒为 nil，startWindow 恒返回空。rps/concurrency（走 newEnvSilent）不受影响。
- 后果：user 模式丢失窗口级 cache hit/preemption/排队观测——本轮测试只能靠 TTFT 间接推断
  cache 行为，1.6 的 cache 容量拐点无法直接归因。
- **结论：一行修复（env 构造后调用 setupServerMetrics），建议尽快修。**

### 1.4 new_tokens 指标在 user 场景恒为 0（代码缺口）

- 现象：所有轮 `new_tokens=0`，而实际 prompt 逐轮增长（Δ 手算 3.6K~25K）。
- 根因：new_tokens（相对上一轮新增 prompt tokens）由旧 Multiturn 的 lastPrompt 基准推进
  逻辑计算，user 场景实现时未迁移该逻辑。
- 后果：**增量 prefill 速率（ms/千新 token）这个核心指标在 user 模式缺失**——
  恰是"越聊越贵"曲线的一级变量。
- **结论：user 会话循环中推进 lastPrompt 基准（仅成功轮），恢复 new_tokens。**

---

## 二、MEDIUM：测量口径与行为问题

### 2.1 TTFT 阶梯跳变：prefix cache 容量拐点（~120K×2 用户）

- 现象（t4 组，重增量 10-25K/轮）：
  - 用户1：t1-t4（45K→100K）TTFT 0.7~1.2s → t5（118K）起跳 **11.2s**，此后 6~13s 波动；
  - 用户2：t4（100K）0.72s → t5（122K）**12.9s**，此后 6~17s；
  - 原始组（thinking 污染）同样在 ~120K 跳变：0.7s → 10.3s。
- 两个用户跳变点都在 ~120K prompt、且都在第 5 轮附近——高一致性的系统性拐点，
  而非随机抖动。推断：两用户 KV 前缀合计超 prefix cache/GPU 容量，块开始逐出，
  每轮大比例重新 prefill。因观测层缺失（1.3）无法直接看 cache hit 归因。
- 对照（t8 组，3-8K 增量到 120K）：TTFT 全程 0.6~4.2s 平缓——单用户序列内 cache 正常时
  曲线确实平缓，佐证 t4 的跳变是容量/逐出问题。
- **结论：这是 user 模式最有价值的实测输出之一（TTFT 阶梯 = cache 容量边界的在线信号），
  值得在修复 1.3 后复测并写入容量画像；同时建议报告侧把"逐轮 TTFT 环比跳变 >5x"标为
  cache 逐出疑点。**

### 2.2 双用户无错峰：首轮 TTFT 被并发 prefill 污染

- 现象：users=2 时两个会话同时发首轮（35-40K × 2 prefill 互抢），
  turn1 TTFT 6.6s/7.7s（t1）、16.5s/11.5s（t6）、24.9s/14.3s（t7）——
  同配置两用户首轮 TTFT 差异可达 1.7 倍；且后续轮 TTFT 在 1s 与 8-14s 间双峰交替
  （两会话大 prefill 交错排队）。
- 后果：小用户数下逐轮 TTFT 斜率被调度噪声污染，会话级曲线需要多会话取中位才可用。
- **结论：① user 模式建议加可选的会话启动错峰（stagger）；② 报告侧 user 曲线应默认按
  (session, turn) 聚合中位数而非逐轮均值；③ 文档需说明 users>1 时 TTFT 含排队分量。**

### 2.3 首轮 35-40K 约束与档位增量下限冲突时静默超限

- 现象：t3/fix-t3（ctx 10-25K）首轮实测 prompt=45.4K、43.7K，超出 profile
  first_turn_tokens 上限 40K。
- 根因：实现中首轮 ctx 取 `max(firstTotal - base - user, ctx_lo)`——档位下限优先，
  代码注释"宁可超首轮上限也不产生空转轮"，但无任何告警。
- **结论：超限时打一条 warning 并落盘实际首轮值（目前只在 review 中发现，报告不可见）。**

### 2.4 max_output_tokens 过小使 decode 指标失真

- 现象：max_tokens=128 时大量 `finish=length`（t8 16 轮中 6 轮），content 被截断；
  decode tok/s 因输出过短方差极大（3~78 tok/s）。
- **结论：user 矩阵测试 max_tokens 建议 ≥256；报告侧对 finish=length 占比高的会话
  应标注"输出预算钳制"。**

### 2.5 极短输出轮的 decode 指标失真并污染分位

- 现象（fix-t4 用户2 t6）：content=2 字符（近空回复）时 `itl_p50=0.0ms、tpot=1.2ms、
  decode tok/s=1704`——物理上不可能（27B 单流不可能 1700 tok/s）。
- 根因：1~2 个 decode chunk 使 ITL 分位/TPOT/decode 速率退化为噪声；该轮本身
  属于 1.2 的近空回复形态。
- 后果：会话级 decode 分位数会被这类轮污染（一个 1700 tok/s 样本会拉高均值/分位）。
- **结论：decode 指标（ITL/TPOT/tok/s）在 completion_tokens < 阈值（如 8）时应置空
  或打标，会话聚合时排除。**

### 2.6 首轮 35-40K 与 262K 上限的组合预警缺失

- 现象：fix-t4 用户2 滚到 256599 token（距 262144 上限 5.5K）仍未触发 ctx limit
  （下轮将超限）。profile（12 轮 × 25K 增量）天然会逼近模型上限，但工具没有
  "预计最大上下文 = 首轮 + 轮数 × 平均增量 vs max_model_len" 的启动预警。
- **结论：加载 profile 时用 first_turn 上限 + turns_hi × ctx_hi 估算累计上限，
  超模型 max_model_len 时启动告警。**

---

## 三、LOW / 观察项

### 3.1 filler_fidelity 对自然文本语料的适用性存疑

- probe 实测：合成词表标称 2000tk → 实测 1212tk（6.6 chars/token，偏差 -39%）。
  该系数只对合成词表成立；user 模式用经典书语料（自然文本，构造系数 4.0），
  本机未实测书语料的真实换算比。首轮实测 t1=36.7-39.7K（构造 35-40K）偏差 <7%，
  说明 4.0 对该部署近似可用，但 probe 的 filler_fidelity 检查测的不是语料路径，
  **"user 模式换算比保真检查"实际没有对应的 probe 实现**（计划 13.6 的职责转移未落地）。

### 3.2 usage 无 cached_tokens

- 该部署 `usage.prompt_tokens_details` 为 None——逐请求 cache 命中不可得，
  cache 观测只能依赖 /metrics 窗口级 counter（当前又因 1.3 缺失）。双重缺失下
  **本轮所有 cache 结论都是 TTFT 间接推断**，需在文档中明示置信级别。

### 3.3 部署空 think 块开销

- 部署默认（未传参数）短请求 completion_tokens=27 而 content 仅 3 字符——
  Qwen3 chat template 强制 `<think>\n\n</think>` 空块，约占 20+ token/请求。
  对 rps/concurrency 的输出预算口径有轻微影响（vLLM bench serve 同模板下同样存在，
  对比公平；但 completion_tokens ≠ 可见输出 token，分析 decode 速度时注意）。

### 3.4 大上下文能力实测（正面结果）

- 256,599 token prompt 成功处理（TTFT 11.1s，finish=stop content 正常）；
  140K 首轮 TTFT 14~25s；90K 首轮 11~16s；37K 首轮 0.9~14s（受并发干扰）。
  **命中前缀后 TTFT 与增量强相关**：t7 turn2（+1.9K）1.8~2.6s、t1 turn2（+1.9K）0.9~1.1s。
- 实测单请求 prefill 速率（cache 冷）：90K≈1.1-1.5s/万tok、140K≈1.1-1.8s/万tok、
  45K≈1.3-2.9s/万tok（并发干扰下限不可靠），数量级 ~1 万 token/秒 prefill。

### 3.5 矩阵结果速览（fix 组，显式关思考）

| 组 | 首轮实测 | 轮数 | 增量实测 | TTFT 范围 | 备注 |
|---|---|---|---|---|---|
| t1-fix | 36.7-38.9K | 2/4 | ~1.9K/轮 | 0.9-14.6s | 双峰=并发干扰 |
| fix-t3 | 44.4-45.4K | 4/5 | 13-25K/轮 | 3.3-10.4s | 首轮超上限；1 空轮 |
| fix-t4 | 44.4-45.4K | 10/12 | 11-25K/轮 | 0.9-17.1s | **~120K 阶梯**；256K 成功；1 空轮 |
| fix-t8 | 37.1-40.3K | 14/16 | 3.6-8.6K/轮 | 1.5-4.5s | 曲线平缓（cache 健康） |

### 3.6 rps/concurrency 冒烟（链路验证，mini fixture）

- concurrency：levels [1,4]，8 prompts，ok=8/8，吞吐 15→28 tok/s（并发翻倍吞吐近翻倍，
  形态正确）；rps：rate=2/s、12 prompts、ok=12/12、wall=5.7s（到达窗口符合）。
- 未发现调度层异常；mini fixture 数据量太小，结论仅限"链路可用"。
- 待办（按用户指示往后放）：用真实 ShareGPT 大样本复测；验证 num_prompts/seed 与
  vLLM bench serve 抽样一致性；H3（barrier 有限 request_rate 到达率失真）修复后
  复测 request_rate>0 组合。

---

## 四、修复状态（2026-09-19 全部完成）

| # | 问题（对应发现） | 修复方式 |
|---|---|---|
| 1 | user 装配 server_metrics（1.3） | UserScenario env 构造后调用 setupServerMetrics；真机回归 available=true |
| 2 | new_tokens 基准推进（1.4） | 会话循环维护 prevPrompt 基准（仅成功轮推进）；真机回归逐轮 new 非零 |
| 3 | 空回复轮识别 + 终止（1.2） | finish=stop 且 completion<8 视为空回复轮：warnings 留痕 + 终止会话 |
| 4 | thinking 显式化兜底（1.1） | Variants() 对无 extra_body 的关闭态变体注入 enable_thinking=false（真机验证） |
| 5 | 首轮超上限告警（2.3）+ 组合上限预警（2.6） | 首轮 >FirstTurnTokens[1] 告警；启动时 estMaxContext vs max_prompt_tokens 预警 |
| 6 | 极短输出 decode 指标置空（2.5） | Finalize 中 completion<8 时 ITL/TPOT/tok/s 置空并 warnings 留痕 |
| 7 | user 会话错峰（2.2） | user.stagger_ms 配置（默认 0）：第 N 个用户延迟 N×stagger 启动 |
| 8 | H3 barrier 限速（rps request_rate 失真） | 独立发射时钟按泊松注入 + 容量=level 队列背压（vLLM 语义），含回归测试 |
| 9 | filler_fidelity 语料路径（3.1） | probe 校准前自动加载内置语料，与 user 模式同口径 |

2.4（max_tokens 过小）属配置建议，已由矩阵测试文档记录；rps/concurrency 真实 ShareGPT
大样本复测仍为后续事项。

## 五、rps/concurrency 真实样本复测中断（环境事件）

- 时间：2026-09-19 17:00 起，`<real-endpoint>` 全端点（含 /models、/metrics）返回 502
  腾讯 OA 拦截页（`<!DOCTYPE html>...<title>提示</title>`）——网络入口层故障
  （VPN/代理/网关），非服务自身、非工具问题。手动 curl 复现 5/5 全部 502。
- 中断前的部分数据（level=1 期间）：ShareGPT 200 条抽样已出现 143 个唯一请求 502 失败
  （id 分布随机，非特定会话）——即故障在 level=1 时已发生，只是当时未识别为环境问题。
- 观测到的工具问题已修复：`/metrics` 抓取超时 5s 在高负载下不够
  （`context deadline exceeded` → 观测层误判不可用），已放宽到 15s（fc38e42）。
- 待环境恢复后重跑：concurrency levels [1,8,32] × 200 prompts、rps rates [1,4] × 100
  prompts（配置：`/tmp/cfg-conc-real.yaml`、`/tmp/cfg-rps-real.yaml`，临时目录需重建）。
  部分日志存于 `llm-perf-test/real-matrix/real-conc-real.log`。
