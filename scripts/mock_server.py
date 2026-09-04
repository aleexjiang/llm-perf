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
