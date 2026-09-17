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
- `*.stall.csv`：降速控制层采样序列；
- `run.log`：测试级日志；
- `raw/`：开启 `debug` 或请求失败时的原始响应证据。

字段和统计口径见 `docs/data-contract.md`，场景选择见 `docs/scenario-guide.md`。`/metrics` 只作为参考、诊断和控制层数据，客户端计时是评测基线。

## 配置与敏感信息

端点、API key、生产参数只放本地配置；不要提交客户配置。历史 `single` 配置字段暂为兼容旧文件保留，但公共 `bench` 入口不会执行，迁移到 `multiturn` 和 `concurrent`。
