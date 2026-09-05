#!/usr/bin/env python3
"""本地 mock OpenAI 兼容服务，仅用于 llm-perf 冒烟测试。

支持：
- stream=true  -> SSE 流式，逐 chunk 输出
- stream=false -> 一次性 JSON 响应
- chat_template_kwargs.enable_thinking -> 先输出 reasoning_content 段（带延迟）再输出 content
- usage 恒定返回（含 completion_tokens_details.reasoning_tokens）
"""
import json, time
from http.server import BaseHTTPRequestHandler, HTTPServer


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    # 模拟 vLLM /metrics：让观测层（counter 差值 / gauge 轮询 / histogram 窗口）有真路径可测。
    # 计数器随请求数累加（全局可变状态），gauge/gauge 直方图为静态样例。
    requests_served = 0

    def do_GET(self):
        if self.path.endswith("/metrics"):
            H.requests_served += 1
            n = H.requests_served
            body = (
                f'vllm:prefix_cache_queries_total{{engine="0"}} {n * 1000}\n'
                f'vllm:prefix_cache_hits_total{{engine="0"}} {n * 800}\n'
                f'vllm:num_preemptions_total{{engine="0"}} 0\n'
                f'vllm:spec_decode_num_drafts_total{{engine="0"}} {n * 10}\n'
                f'vllm:spec_decode_num_accepted_tokens_total{{engine="0"}} {n * 16}\n'
                f'vllm:num_requests_running{{engine="0"}} 1\n'
                f'vllm:num_requests_waiting{{engine="0"}} 0\n'
                f'vllm:gpu_cache_usage_perc{{engine="0"}} 0.31\n'
                f'vllm:request_queue_time_seconds_bucket{{le="0.01"}} {n}\n'
                f'vllm:request_queue_time_seconds_bucket{{le="+Inf"}} {n}\n'
                f'vllm:request_queue_time_seconds_sum {n * 0.005}\n'
                f'vllm:request_queue_time_seconds_count {n}\n'
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
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
        prompt_tokens = sum(len(m["content"]) for m in body["messages"]) // 4 or 1
        n_reason, n_content = (4, 3) if thinking else (0, 3)
        usage = {
            "prompt_tokens": prompt_tokens,
            "completion_tokens": n_reason + n_content,
            "total_tokens": prompt_tokens + n_reason + n_content,
            "completion_tokens_details": {"reasoning_tokens": n_reason},
        }

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
                time.sleep(0.05)
                emit({"choices": [{"delta": {"reasoning_content": f"reason{i} "}}]})
        for i in range(n_content):
            time.sleep(0.05)
            emit({"choices": [{"delta": {"content": f"chunk{i} "}}]})
        emit({"choices": [], "usage": usage})
        self.wfile.write(b"data: [DONE]\n\n")


HTTPServer(("127.0.0.1", 18099), H).serve_forever()
