#!/usr/bin/env python3
"""本地 mock OpenAI 兼容服务，仅用于 llm-perf 冒烟测试。

支持：
- stream=true  -> SSE 流式，逐 chunk 输出
- stream=false -> 一次性 JSON 响应
- chat_template_kwargs.enable_thinking -> 先输出 reasoning_content 段（带延迟）再输出 content
- usage 恒定返回（含 completion_tokens_details.reasoning_tokens）
- 请求带 tools -> 返回结构化 tool_calls（非流式 message.tool_calls /
  流式 delta.tool_calls 按 index 分片增量拼 arguments），probe tool-call 检查的"好引擎"路径
"""
import json, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer

# TOKEN_DELAY：每个输出 chunk 的间隔秒数，默认 0.05（≈20 tok/s）。
# 调大可构造"服务端降速"现场，用于验证降速熔断（stall_guard）——
# 见 scripts/stall-e2e/ 的完整回归用例。
TOKEN_DELAY = float(os.environ.get("MOCK_TOKEN_DELAY", "0.05"))

# 直方图桶分布：确定性（越小的桶装越多），便于冒烟断言分位落在预期区间。
HIST_FRACS = ((0.01, 0.10), (0.05, 0.35), (0.1, 0.55), (0.25, 0.80),
              (0.5, 0.90), (1.0, 0.97), (2.5, 0.995))


def hist_family(name, count, avg):
    """生成一个 Prometheus 直方图 family（延迟类指标单位秒）。

    smetrics 只对 `HistNames()` 里的 family 取窗口差值，分位按桶边界估算——
    因此这里给出真实的 _bucket/_sum/_count 三件套即可跑通「服务端延迟分解」。
    """
    lines = ['%s_bucket{le="%g"} %d' % (name, le, int(count * frac)) for le, frac in HIST_FRACS]
    lines.append('%s_bucket{le="+Inf"} %d' % (name, count))
    lines.append("%s_sum %.3f" % (name, count * avg))
    lines.append("%s_count %d" % (name, count))
    return "\n".join(lines) + "\n"


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    # 模拟 vLLM /metrics：让观测层（counter 差值 / gauge 轮询 / histogram 窗口）有真路径可测。
    # requests_served 随抓取次数累加（counter 差值恒为正）；chat_served/gen_tokens 随真实
    # 推理请求累加——后者是 10.1「两源一致性守卫」的服务端基准（客户端吞吐 vs 服务端生成吞吐）。
    requests_served = 0
    chat_served = 0
    gen_tokens = 0

    def do_GET(self):
        if self.path.endswith("/metrics"):
            H.requests_served += 1
            n = H.requests_served
            nc = H.chat_served
            body = (
                f'vllm:prefix_cache_queries_total{{engine="0"}} {n * 1000}\n'
                f'vllm:prefix_cache_hits_total{{engine="0"}} {n * 800}\n'
                f'vllm:num_preemptions_total{{engine="0"}} 0\n'
                f'vllm:spec_decode_num_drafts_total{{engine="0"}} {n * 10}\n'
                f'vllm:spec_decode_num_accepted_tokens_total{{engine="0"}} {n * 16}\n'
                f'vllm:generation_tokens_total{{engine="0"}} {H.gen_tokens}\n'
                f'vllm:num_requests_running{{engine="0"}} {1 + nc % 3}\n'
                # 排队深度随负载变化（非零）：报告「waiting 峰值」列与 saturation_guard.max_waiting
                # 建议值需要真实的非零观测才走得到（恒 0 会让该列整片不渲染，路径测试不到）
                f'vllm:num_requests_waiting{{engine="0"}} {(nc // 2) % 3}\n'
                f'vllm:gpu_cache_usage_perc{{engine="0"}} 0.31\n'
                # 12.12 KV 容量画像：info 型指标（值恒 1、配置在 label）——报告侧据此并列
                # 「实测拐点 vs KV 上界」。取值对齐用户实测形态（fp8 / block 1600 / 池 1.5M tk）
                f'vllm:cache_config_info{{block_size="1600",cache_dtype="fp8",'
                f'gpu_memory_utilization="0.9",kv_cache_max_concurrency="5.74",'
                f'kv_cache_size_tokens="1505497"}} 1\n'
            ).encode() + "".join(
                hist_family(name, nc, avg) for name, avg in (
                    ("vllm:request_queue_time_seconds", 0.005),
                    ("vllm:request_prefill_time_seconds", 0.05),
                    ("vllm:request_decode_time_seconds", 0.20),
                    ("vllm:time_to_first_token_seconds", 0.06),
                    ("vllm:inter_token_latency_seconds", 0.02),
                    ("vllm:e2e_request_latency_seconds", 0.25),
                )).encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path.endswith("/models"):
            # 模型列表（probe models_list / context_limit 检查的数据源）
            body = json.dumps({
                "object": "list",
                "data": [
                    {"id": "mock-model-a", "object": "model", "max_model_len": 8192},
                    {"id": "mock-model-b", "object": "model", "max_model_len": 8192},
                ],
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        raw = self.rfile.read(int(self.headers["Content-Length"]))
        body = json.loads(raw)
        thinking = bool(body.get("chat_template_kwargs", {}).get("enable_thinking"))
        stream = bool(body.get("stream", True))
        tools = body.get("tools")
        # usage 按 4 chars/token 模拟——刻意与构造侧的 en 系数（corpus.CharsPerToken=4.0）一致：
        # 这样 smoke 里 probe 的 filler_fidelity 落到"保真度可信"，两源一致性检查也对得上。
        prompt_tokens = sum(len(m["content"]) for m in body["messages"]) // 4 or 1
        n_reason, n_content = (4, 3) if thinking else (0, 3)
        usage = {
            "prompt_tokens": prompt_tokens,
            "completion_tokens": n_reason + n_content,
            "total_tokens": prompt_tokens + n_reason + n_content,
            "completion_tokens_details": {"reasoning_tokens": n_reason},
        }
        # 服务端自身的产出统计（/metrics 口径）：与 usage 一致，便于两源一致性检查对得上，
        # 也对得上「客户端 loss」类断言的失败方向（客户端少算 → 偏差为负）。
        H.chat_served += 1
        H.gen_tokens += n_reason + n_content

        # ── tool-call 路径：请求带 tools 即返回结构化调用（好引擎形态） ──
        if tools:
            call = {"index": 0, "id": "call_mock1", "type": "function",
                    "function": {"name": tools[0]["function"]["name"],
                                 "arguments": "{\"city\": \"北京\"}"}}
            if not stream:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({
                    "choices": [{
                        "message": {"role": "assistant", "content": "",
                                    "tool_calls": [call]},
                        "finish_reason": "tool_calls",
                    }], "usage": usage,
                }, ensure_ascii=False).encode())
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()

            def emit(d):
                self.wfile.write(f"data: {json.dumps(d, ensure_ascii=False)}\n\n".encode())
                self.wfile.flush()

            # 首片带 id/name；arguments 按增量分片拼接（真实引擎的流式形态）
            emit({"choices": [{"delta": {"tool_calls": [{
                "index": 0, "id": call["id"], "type": "function",
                "function": {"name": call["function"]["name"], "arguments": "{\"ci"}}]}}]})
            emit({"choices": [{"delta": {"tool_calls": [{
                "index": 0, "function": {"arguments": "ty\": \"北京\"}"}}]}}]})
            emit({"choices": [{"delta": {}, "finish_reason": "tool_calls"}]})
            emit({"choices": [], "usage": usage})
            self.wfile.write(b"data: [DONE]\n\n")
            return

        if not stream:
            time.sleep(0.2)
            msg = {"role": "assistant", "content": "chunk0 chunk1 chunk2"}
            if thinking:
                msg["reasoning_content"] = "reason0 reason1 reason2 reason3"
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({
                "choices": [{"message": msg}], "usage": usage,
            }, ensure_ascii=False).encode())
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        def emit(d):
            self.wfile.write(f"data: {json.dumps(d, ensure_ascii=False)}\n\n".encode())
            self.wfile.flush()

        time.sleep(0.2)  # 模拟 prefill
        if thinking:
            for i in range(n_reason):
                time.sleep(TOKEN_DELAY)
                emit({"choices": [{"delta": {"reasoning_content": f"reason{i} "}}]})
        for i in range(n_content):
            time.sleep(TOKEN_DELAY)
            emit({"choices": [{"delta": {"content": f"chunk{i} "}}]})
        emit({"choices": [], "usage": usage})
        self.wfile.write(b"data: [DONE]\n\n")


HTTPServer(("127.0.0.1", int(os.environ.get("MOCK_PORT", "18099"))), H).serve_forever()
