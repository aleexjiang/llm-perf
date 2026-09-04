#!/usr/bin/env python3
"""本地 mock OpenAI 兼容流式服务，仅用于 llm-perf 冒烟测试。"""
import json, time
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        def emit(d):
            self.wfile.write(f"data: {json.dumps(d, ensure_ascii=False)}\n\n".encode())
            self.wfile.flush()
        # 模拟首包延迟
        time.sleep(0.2)
        for i in range(3):
            time.sleep(0.05)
            emit({"choices": [{"delta": {"content": f"chunk{i} "}}]})
        emit({"choices": [], "usage": {"prompt_tokens": body and sum(len(m["content"]) for m in body["messages"]) // 4 or 1,
                                        "completion_tokens": 3, "total_tokens": 4}})
        self.wfile.write(b"data: [DONE]\n\n")

HTTPServer(("127.0.0.1", 18099), H).serve_forever()
