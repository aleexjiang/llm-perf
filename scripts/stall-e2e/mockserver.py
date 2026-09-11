#!/usr/bin/env python3
"""降速熔断端到端验证用的假服务端。

区分两类请求（探针 vs 压测场景）：
  - prompt 很短（<2000 字符）= 启动时的环境探针 → 立刻吐完所有 token，让探针快点过；
  - prompt 很长（≥2000 字符）= 压测场景的 filler 请求 → 按 SLOW_TPS 慢速吐，逼出熔断。
流式/非流式都按请求的 stream 字段如实响应。

用法: mockserver.py [PORT] [SLOW_TPS] [RECOVER_AFTER]
  RECOVER_AFTER：启动多少秒后"恢复"（长 prompt 也转快吐），模拟服务端中途修复；
  0/缺省 = 永不恢复（8.2 恢复探针回归用：恢复 → 探针通过 → 继续下一场景）。
"""
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 18849
SLOW_TPS = float(sys.argv[2]) if len(sys.argv) > 2 else 5.0
RECOVER_AFTER = float(sys.argv[3]) if len(sys.argv) > 3 else 0.0
SLOW = 1.0 / SLOW_TPS
PROMPT_CHARS_THRESHOLD = 200
HERE = os.path.dirname(os.path.abspath(__file__))

# 恢复时钟锚定「首个慢速请求到达」而非服务端启动：bench 启动耗时有方差，
# 锚启动时刻会让慢速窗口被吃掉（熔断偶发不触发）。锚首个请求后：
# 熔断窗口(1~2.5s) < RECOVER_AFTER(4s) < 探针最早发车(~4.5s)，两边都有裕量。
_first_slow_at = None


def is_slow():
    """长 prompt 慢速吐 token；首个慢速请求 RECOVER_AFTER 秒后视为已修复，全部快吐。"""
    global _first_slow_at
    if RECOVER_AFTER <= 0:
        return True  # 永不恢复
    now = time.time()
    if _first_slow_at is None:
        _first_slow_at = now
        return True
    return now - _first_slow_at < RECOVER_AFTER


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path.endswith("/models"):
            self._json({"object": "list", "data": [
                {"id": "mock", "object": "model", "max_model_len": 32768}]})
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(n) or b"{}")
        max_tokens = int(req.get("max_tokens") or 8)
        stream = bool(req.get("stream"))
        chars = sum(len(str(m.get("content") or "")) for m in req.get("messages", []))
        slow = chars >= PROMPT_CHARS_THRESHOLD and is_slow()
        with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "reqs.log"), "a") as fh:
            fh.write(f"{time.time():.1f} stream={stream} max_tokens={max_tokens} chars={chars} slow={slow}\n")

        if not stream:
            if slow:
                time.sleep(SLOW * max_tokens)
            self._json({
                "id": "c1", "object": "chat.completion", "model": "mock",
                "choices": [{"index": 0, "finish_reason": "stop",
                             "message": {"role": "assistant", "content": "ok " * min(max_tokens, 8)}}],
                "usage": {"prompt_tokens": 10, "completion_tokens": max_tokens, "total_tokens": 10 + max_tokens},
            })
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        try:
            for _ in range(max_tokens):
                c = {"id": "c1", "object": "chat.completion.chunk",
                     "choices": [{"index": 0, "delta": {"content": "tok"}, "finish_reason": None}]}
                self._sse("data: " + json.dumps(c) + "\n\n")
                if slow:
                    time.sleep(SLOW)
            fin = {"id": "c1", "object": "chat.completion.chunk",
                   "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                   "usage": {"prompt_tokens": 10, "completion_tokens": max_tokens, "total_tokens": 10 + max_tokens}}
            self._sse("data: " + json.dumps(fin) + "\n\n")
            self._sse("data: [DONE]\n\n")
            self._sse("")  # 结束 chunked 正文：少了这个结尾块客户端会一直等流结束
        except (BrokenPipeError, ConnectionResetError):
            pass  # 客户端（熔断）主动断开：正是要验证的路径

    def _json(self, obj):
        b = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def _sse(self, s):
        if s == "":
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
            return
        b = s.encode()
        self.wfile.write(f"{len(b):X}\r\n".encode() + b + b"\r\n")
        self.wfile.flush()


srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
recover = f", recover after {RECOVER_AFTER:.0f}s" if RECOVER_AFTER > 0 else ", never recover"
print(f"mock on :{PORT}: short prompt=fast, long prompt={SLOW_TPS} tok/s{recover}", flush=True)
threading.Thread(target=srv.serve_forever, daemon=True).start()
while True:
    time.sleep(3600)
