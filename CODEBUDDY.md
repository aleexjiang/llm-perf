# llm-perf 项目运行说明

## 工具定位

项目只负责采集 LLM 推理服务原始数据，不生成 HTML 报告。公共压测入口只支持多轮 agent：

- `--concurrency 1`：单发多轮；
- `--concurrency cfg`：使用配置中的 `rate_sweep`/`request_rate`/`levels`，执行多用户多轮；
- `bench probe`：逐模型执行引擎、thinking、tool-call、usage 与可选 `/metrics` 探针。

所有请求（benchmark、warmup、correctness、失败、主动取消）都保留原始指标；warmup/correctness 放在 `auxiliary_requests`，不进入 benchmark KPI。多模型测试必须使用目录输出，结果按模型子目录隔离。

## 本地开发

```bash
make build
make test
go build ./...
go vet ./...
scripts/smoke.sh
```

`scripts/smoke.sh` 会启动本地 mock OpenAI 兼容服务，验证多模型单发多轮、多用户多轮、RPS、thinking、probe、warmup/correctness 完整采集和数据契约。

## 客户环境运行

1. 使用 `make build-linux` 构建 `bin/bench-linux-amd64`。
2. 将二进制和不含敏感信息的配置模板复制到客户环境。
3. 先执行：

```bash
./bench probe -c customer.yaml
```

4. 单发多轮基线：

```bash
./bench -c customer.yaml --concurrency 1 --thinking off --seed-salt 1 -o output/
```

5. RPS 主容量采集：

```bash
./bench -c customer.yaml --concurrency cfg --thinking off --seed-salt 2 -o output/
```

6. 如果业务确实使用思考模式，再用新的 `seed_salt` 采集 `--thinking on` 或指定 `thinking.levels` 档位。

## 输出数据

- 场景 JSON：`multiturn[]`、`concurrent[]`、`correctness[]`、`auxiliary_requests[]`；
- probe JSON：独立的引擎和能力探测结果；
- `run.log`：测试级日志；
- `raw/`：开启 `debug` 或请求失败时的原始响应证据。

字段和统计口径见 `docs/data-contract.md`，场景选择见 `docs/scenario-guide.md`。`/metrics` 只作为参考、诊断和控制层数据，客户端计时是评测基线。

## 配置与敏感信息

端点、API key、生产参数只放本地配置；不要提交客户配置。历史 `single` 配置字段暂为兼容旧文件保留，但公共 `bench` 入口不会执行，迁移到 `multiturn` 和 `concurrent`。

## 新场景架构（2026-09-18 改造，详见 docs/workload-refactor-plan.md）

新增三个显式子命令（与旧 `--turns × --concurrency` 组合并存，迁移完成后旧入口将下线）：

```bash
# 1) user：生成式多轮用户会话（profile 驱动 + 经典书语料 + 被测模型真实回复）
#    需先离线产出 profile：python3 scripts/profile_build.py --trace <raw-trace> --out <profile.json>
./bench user -c customer.yaml --users 8

# 2) rps：冻结请求快照的开环到达（排队-延迟曲线、体验拐点）
./bench rps -c customer.yaml          # rps.rates / rps.max_concurrency / rps.burstiness

# 3) concurrency：固定在飞齐射（总吞吐拐点，对齐 vLLM bench serve）
./bench concurrency -c customer.yaml  # concurrency.levels；直接读 request_set.sharegpt_path
```

关键语义：

- **user 模式**：一用户一本书（12 本公版书语料库，`corpus.SelectBook(seed)` 确定性选书）；
  system 基座一次定型冻结；合成 context 尾部注入 user 消息（严禁进 system——会破坏
  prefix cache）；每轮把**被测模型真实 assistant 回复**追加进 history（动态 prefix cache）；
  默认 light:medium:heavy = 6:3:1（可调），首轮 prompt ≥35K token（agent 形状硬约束）。
- **rps/concurrency**：请求 = 冻结独立快照（ShareGPT 数据集直接读取，与 vLLM
  `--dataset-name sharegpt` 同口径：prompt=截至最后一条 user 的 history，输出预算=其后
  assistant 估算 token，seed 抽样）。两者只差调度器：rps 到达率控节奏，concurrency
  在飞上限控节奏。
- **prefix cache 口径**：user=动态 cache（真实生成链）；rps/concurrency=冻结 cache
  （同 request set 可复现）。cache on/off 对比与 vLLM 对比用 rps/concurrency，
  不与 user 结果混表。
