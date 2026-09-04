# llm-perf

客户自部署 LLM 推理服务性能评测工具（Go，单二进制，无运行时依赖）。

面向堡垒机/内网交付场景：本机交叉编译出 linux/amd64 二进制，连同配置三件套
（`bench` + `config.yaml` + `.env`）拷贝到客户环境执行，跑完把 `output/` 目录拉回来分析。

## 三个场景

| 场景 | 命令 | 回答的问题 |
|---|---|---|
| `single` | `bench single` | 单请求 TTFT / decode 速度随上下文长度如何增长？前缀缓存有没有命中？ |
| `multiturn` | `bench multiturn` | 多轮对话**滚**到 40k 时每轮 TTFT 如何？（模拟 agent：system + tool defs + 逐轮增长 history） |
| `concurrent` | `bench concurrent` | 并发 1→2→4→8→16 时 TTFT 衰减多少？整体吞吐峰值在哪？ |

`bench all` 依次跑三个场景。

## 指标口径

每个请求逐 chunk 记录时间戳，拆分为：

- **TTFT**：首个任意 chunk（含排队 + prefill）
- **TTFT reasoning**：首个 `reasoning_content` chunk ≈ prefill 完成时刻（思考模型）
- **TTFT content**：首个可见内容 chunk = prefill + 思考
- **思考时长** = TTFT content − TTFT reasoning
- **decode 时长 / ITL 分位数 / tokens per second**
- token 数取自响应 `usage` 字段（服务端精确值，非本地估算）

单发版关键对照：`fixed_seed: true` 时各 run 复用同一 prompt——Run2+ 的 TTFT 显著低于 Run1 即前缀缓存命中。
多轮版关键判定：turn N 的 TTFT ≈ turn N−1 TTFT + 新增 token 的 prefill ⇒ 缓存命中；接近全量 prefill ⇒ 未命中。

## 快速开始

```bash
# 本机
make build-linux                    # 产出 bin/bench-linux-amd64
scp bin/bench-linux-amd64 configs/example.yaml 堡垒机:~/llm-perf/

# 堡垒机
mv bench-linux-amd64 bench && chmod +x bench
export LLM_PERF_ENDPOINT=http://10.0.201.1:30082/router/v1
export LLM_PERF_API_KEY=...         # 如服务需要
./bench all -c example.yaml
```

结果在 `output/<时间戳>-<场景>/` 下：`report.json`（原始数据）+ `report.html`（图表报告）。

## 配置

见 `configs/example.yaml`，含详细注释。环境变量优先级最高：
`LLM_PERF_ENDPOINT`、`LLM_PERF_API_KEY`（`api_key_env` 指定从哪个变量读 key）。

## 开发

```bash
make build          # 本机二进制
make test
scripts/mock_server.py  # 本地 mock OpenAI 兼容流式服务（configs/smoke.yaml 配套冒烟）
```
