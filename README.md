# llm-perf

客户自部署 LLM 推理服务性能评测工具（Go，单二进制，无运行时依赖）。

**契约：输入 YAML 配置，输出 JSON 原始数据。** 工具不做任何报告渲染——把 JSON 拿回来
用 WorkBuddy 等工具二次加工出报告。

面向堡垒机/内网交付场景：本机交叉编译出 linux/amd64 二进制，连同配置三件套
（`bench` + `config.yaml` + `.env`）拷贝到客户环境执行，跑完把 JSON 拉回来分析。

## 场景矩阵

```
                单发（串行）              并发（多虚拟用户）
单轮            bench single             bench concurrent
多轮            bench multiturn          bench concurrent (multiturn: true)
```

| 场景 | 回答的问题 |
|---|---|
| `single` | 单请求 TTFT / decode 速度随上下文长度如何增长？前缀缓存有没有命中？ |
| `multiturn` | 多轮对话**滚**到 40k 时每轮 TTFT 如何？（模拟 agent：system + tool defs + 逐轮增长 history） |
| `concurrent` | 并发 1→2→4→8→16 时 TTFT 衰减多少？整体吞吐峰值在哪？（`multiturn: true` 时每个虚拟用户各自跑完整会话重放） |

两个正交开关贯穿全部场景：

- **思考模式**（`thinking.mode: both/on/off`，默认 `both`）：off/on 两套 `extra_body` JSON
  透传进请求体（对齐 vLLM `--extra-body`，适配任意网关的思考参数）。`both` 自动跑 A/B 对照；
  思考开启时 `max_tokens` 自动抬到 `max_tokens_floor`（默认 2048），防止思考吃光输出预算
- **流式**（`stream: true/false`，默认 `true`）：非流式只能测端到端延迟与 usage，
  TTFT/ITL/思考拆分不可测（JSON 中相应字段缺省）；用于 E2E 对照与网关缓冲问题排查

## 指标口径

每个流式请求逐 chunk 记录时间戳，拆分为：

- **TTFT**：首个任意 chunk（含排队 + prefill）
- **TTFT reasoning**：首个 `reasoning_content` chunk ≈ prefill 完成时刻（思考模型）
- **TTFT content**：首个可见内容 chunk = prefill + 思考
- **思考时长**（`think_ms`）= TTFT content − TTFT reasoning；**每次对话（含多轮每一 turn）都有**
- **decode 时长 / ITL 分位数**（GenAI-Perf 口径，不含 TTFT）/ tokens per second
- token 数取自响应 `usage` 字段（服务端精确值，非本地估算），含 `reasoning_tokens`

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

## 输出

每场景落一个 JSON 文件（含全部原始数据：逐 run 计时、逐 chunk 派生指标、usage token）：

```bash
./bench all -c example.yaml                 # → output/single-<ts>.json 等 3 个文件
./bench single -c example.yaml -o r1.json   # → 指定输出文件名
./bench all -c example.yaml -o results/     # → 指定输出目录
```

JSON 结构见 `internal/report/report.go`：`single` / `multiturn` / `concurrent` 三个数组，
元素分别为档位 / 会话 / 并发档位，每条请求是 `engine.TurnMetrics`。

## 配置

见 `configs/example.yaml`，含详细注释。环境变量优先级最高：
`LLM_PERF_ENDPOINT`、`LLM_PERF_API_KEY`（`api_key_env` 指定从哪个变量读 key）。

## 开发

```bash
make build          # 本机二进制
make test
scripts/mock_server.py  # 本地 mock OpenAI 兼容流式服务（configs/smoke.yaml 配套冒烟）
```
