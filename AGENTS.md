# AGENTS.md — llm-perf 项目与工作指南

本文件是给维护者和 AI 助手的统一入口：先说明项目边界与运行方式，再记录设计拍板、
负载口径、验证纪律和工程陷阱。指标方法论、JSON 字段与内部模块边界分别见
`docs/testing-architecture.md`、`docs/data-contract.md` 和 `docs/architecture.md`。

## 项目定位

项目只负责采集客户自部署 LLM 推理服务的原始性能数据；Go 侧交付单二进制，
不生成 HTML 报告。聚合、分位、判级和呈现由 pandas、notebook、客户 BI 或其他分析工具完成。

- 核心契约：输入 YAML 配置，输出自描述的 JSON 原始数据。
- 对外交付面：`schema_version` + `docs/data-contract.md`；结构变更必须递增版本。
- 面向堡垒机/内网交付：本机交叉编译 linux/amd64，把二进制和配置模板带到客户环境运行，
  跑完后只回传 JSON 与日志。
- 本仓库模板默认把测试产物写入 `llm-perf-test/output`；客户环境可按需覆盖 `output_dir`。

## 命令入口

2026-09-18 起使用显式子命令。公共入口只有四个场景：

```bash
./bench probe        -c example.yaml    # 引擎、认证、thinking、tool-call、usage、/metrics 探测
./bench user         -c example.yaml    # profile 驱动的生成式多轮用户会话
./bench rps          -c example.yaml    # 冻结请求快照的开环 Poisson 到达
./bench concurrency  -c example.yaml    # 冻结请求快照的固定在飞齐射
```

常用覆盖参数包括 `-m`（按模型子串过滤）、`-o`（JSON 文件或目录）、`-seed-salt`、
`--thinking on|off` 和 `--max-ctx`；`user` 还支持 `--users`。场景参数放在 YAML 中，
不在 CLI 中重新发明调度旋钮。

**user** 需要先离线生成 profile：

```bash
python3 scripts/profile_build.py --trace <raw-trace> --out <profile.json>
./bench user -c customer.yaml --users 8
```

**rps/concurrency** 直接读取 `request_set.sharegpt_path`。请求源相同，只差调度器：
rps 用到达率控制节奏，适合回答“敢承诺多少请求/秒”；concurrency 用在飞数量打满服务端，
适合回答“系统最多能装多少”。user 的 prefix cache 是真实生成链形成的动态 cache；
rps/concurrency 是冻结快照 cache。两类结果不能混表。

### 客户环境运行顺序

1. `make build-linux` 生成 `bin/bench-linux-amd64`。
2. 复制二进制和不含敏感信息的配置模板到客户环境。
3. 先跑 `./bench probe -c customer.yaml`，确认标准面可用后再压测。
4. 按目标选择 user、rps 或 concurrency；思考模式另起测试时更换 `seed_salt`。

## 输出与配置

场景 JSON 的主数组是 `multiturn[]`（user）和 `concurrent[]`（rps/concurrency）；
probe JSON 是独立结构。warmup、correctness、失败和主动取消都保留完整
`TurnMetrics`，warmup/correctness 位于 `auxiliary_requests[]`，不进入主 KPI。
`run.log` 追加不覆盖；`raw/` 只在显式 `debug: true` 或请求失败时留证据。
多模型输出必须使用目录，结果按模型子目录分区。

端点、API key 和客户生产参数只放本地配置：

- `configs/customer*.yaml` 和 `docs/customer-*.md` 已被 gitignore。
- 自有环境测试配置和测试产物放在 `llm-perf-test/`，整目录不入库。
- 仓库只保留 `configs/example.yaml`、`configs/benchmark.yaml`、smoke 配置和测试 fixture。

## 设计拍板

- **采集器边界（2026-09-17）**：报告层已整体剥离。Go 只保证原始数据可信、完整、自描述；
  “分析侧能算的不碰 Go”。
- **原始请求一条不丢（2026-09-17）**：warmup、benchmark、correctness、失败和主动取消都采集完整
  `TurnMetrics`。用途用 `phase` 标记，统计隔离与完整落盘是两条独立规则，不能因为“不进统计”就丢数据。
- **受控变量优先**：每个维度可隔离、可归因。真实感来自测试设计与配置组合，不来自把真实因素揉进一次跑分。
- **体验优先（2026-09-16）**：判据先看单流速度、用户可感知的 TTFT/排队和容量拐点；
  服务端聚合吞吐与资源指标只作归因诊断，不充当体验结论。
- **RPS 是默认主路径（2026-09-17）**：开环 `request_rate` 表示到达率；闭环 `levels` 是辅助诊断。
  不引入 `ramp/ramp_factor`，避免两套加压节奏和失败止损语义混入采集器。
  会话起点用错峰启动实现，不通过开局注入历史伪造分布。
- **轮间思考时间不做**：TTFT/TPOT 是 per-request 指标；用户思考间隔不改变被测服务的工作点。
  到达节奏用开环 `request_rate` 控制，比模拟用户发呆更直接、可复现。
- **冷、暖 prefill 分开测、分开标注**：真实 agent 长上下文多为暖前缀，冷大 prompt 是少数路径。
  混报会让 TTFT 差一个数量级的两条路径不可解读。
- **TTFT 体验判据以 30–40K 输入档为准**：现代 agent 基线上下文约 35K，短输入徽章代表不了 agent 体验。
  三档阈值和方法论出处见 `docs/latency-baselines.md`。
- **tool-call 只落 probe 健康检查**：默认开启，不进压测路径。agent 主导业务用纯推理压测汇报吞吐时，
  必须主动说明该边界。
- **user 首轮按 agent 形状约束在约 35K token**：轻中重档按会话分派，SWRR 确定性分配；
  不做会话内逐轮换档。会话档位标签随 `MultiturnRun` 落盘。
- **配置 schema 不保向后兼容**：迭代期改字段直接改类型，不做双轨兼容。
- **告警必须附可核实证据**：包含请求摘要、响应片段和 capture 范围；判据保守化，
  不确定降 WARN；capture 只在显式指定目录时落盘。
- **指标层冻结（2026-09-12）**：评测指标只有 TTFT、decode 速度、goodput@SLO、正确性 canary 四个数。
  `TurnMetrics` 核心字段封版；新增字段必须指认四数之一，否则用 `json:"-"` 只进诊断面。
  新功能必须指认控制层或诊断层价值，指认不出不做。配置面不开新顶层旋钮；
  新能力优先做成预设配置或文档指南，必要的模型差异放 `model_overrides`。
- **trace 与 filler 分工；多模型隔离优先（2026-09-12）**：trace 的第一交付物是离线画像；
  filler 是受控变量仪器。多模型默认逐模型隔离测试加组合归因，混合回放只在体感失配且已排除
  客户端/网络因素时复活。
- **同目标多路径必须同口径（2026-09-12）**：语料换算、prompt 构造、抽样、分位和吞吐口径统一后才能比较。
  口径系数要可实测；`probe` 的 `filler_fidelity` 偏离超过 25% 告警。
- **客户端计时是测量点选择，不是缺陷（2026-09-12）**：网络路径属于用户体验真值。工具开销与被测间隔
  差几个数量级；两源一致性检查用于发现客户端瓶颈，不做计时架构重构。
- **`test:` 只切表达层，不切测量（2026-09-13）**：`benchmark|performance|soak` 共用同一套数据与判据，
  不为类别复制管线或另立判据。跨部署可比依赖同一组格、同一口径和同一判据。

## 负载形态矩阵

| 路径 | 场景 | 状态 |
|---|---|---|
| 暖（真实 agent 典型体验） | 错峰开环 user/multiturn，逐轮共享前缀 | 已有能力 |
| 冷（最坏角落） | 冻结大 prompt，换 seed 击败缓存 | 已有能力 |
| 缓存收益量化 | `fixed_seed` 对照 + server_metrics 命中率 | 已有能力 |
| 稳态 soak | 会话续跑，时长制采样 | 已有能力 |

## 验证纪律

交付前至少执行：

```bash
gofmt -l internal/ cmd/
go build ./...
go vet ./...
go test ./... -count=1
```

`gofmt` 必须无输出。CLI、config 或场景交互逻辑改动后，还必须运行：

```bash
scripts/smoke.sh
```

`smoke.sh` 启动 local mock OpenAI 兼容服务，验证 probe、user、rps、concurrency、thinking、
warmup/correctness 完整采集、模型分区和数据契约。HTML 报告层已移除；新增报告面应放进外部分析流程，
不要回到 Go 或仓库脚本里。

文档与代码同步的硬规则：

1. 改 `TurnMetrics` 或 `Report` 结构时同步 `docs/data-contract.md`，并递增 `SchemaVersionCurrent`。
2. 改模块边界或扩展点时同步 `docs/architecture.md`。
3. 改指标定义、阈值或测试形态时同步 `docs/testing-architecture.md` / `docs/latency-baselines.md`。
4. 新增设计拍板写进本文件，操作口径写进对应 `docs/` 文档，不在会话里只留口头结论。

## 工程陷阱

- 同一文件并行编辑会互相覆盖；串行修改，改完用搜索复核。
- shell `grep` 对部分模式可能静默返回空；关键结论要用结构化搜索并交叉验证。
- bench CLI 配置用 `-c`；输出不要用 `head` 截断，SIGPIPE 会杀进程，重定向到文件。
- `TurnMetrics` 新增字段默认考虑 `json:"-"`，避免污染压测 JSON 契约。
- 展示口径必须等于执行口径：测试画像复用 `ClampLadder` 和现有估算函数，不另写一套估算；
  reach 是估算值，输出带 `~`。
- prefix cache 两条硬约束：synthetic context 只能尾部注入，严禁进 system；system 基座会话开始后冻结，
  不放时间戳或随机数。
- thinking 关闭必须显式。`Variants()` 和 probe 的关闭态兜底注入 `enable_thinking=false`；
  修改时不要丢失显式值优先逻辑。
- 三套 seed 公式加 `-seed-salt` 负责测试隔离；重跑对照必须换盐或清服务端缓存。
- 截断必须走 `TruncateRunes`，不能按字节切中文。
- `run.log` 追加不覆盖，多轮测试日志都要留得住。
