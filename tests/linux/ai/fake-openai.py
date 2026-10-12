#!/usr/bin/env python3
"""Stand-ins for tests/linux/ai-smoke.sh. Loopback only, standard library only.

  fake-openai.py serve PORT LOG   an OpenAI-compatible provider. FAKE_EXPECT_KEY is the key it accepts.
  fake-openai.py trap HOST PORT LOG   accepts connections and writes one line per connection: the address
                                  Studio would reach if it ignored the base URL it was given.

The provider answers GET /v1/models and POST /v1/chat/completions (streaming and not) and nothing
else: a request to /v1/responses is recorded and refused, so the test sees which API Studio used.
LOG gets one JSON line per request with what the test asserts on. The key itself is never written
down, only whether the Authorization header matched.
"""
import json
import os
import socket
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()


def record(path, entry):
    with LOCK, open(path, "a") as f:
        f.write(json.dumps(entry) + "\n")


def serve(port, log):
    expect = "Bearer " + os.environ["FAKE_EXPECT_KEY"]

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def reply(self, code, obj):
            raw = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            auth_ok = self.headers.get("Authorization") == expect
            record(log, {"method": "GET", "path": self.path, "auth_ok": auth_ok})
            if self.path != "/v1/models":
                return self.reply(404, {"error": {"message": "not found"}})
            if not auth_ok:
                return self.reply(401, {"error": {"message": "bad key"}})
            self.reply(200, {"object": "list", "data": [{"id": "fake-model", "object": "model"}]})

        def do_POST(self):
            raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
            text = raw.decode("utf-8", "replace")
            try:
                body = json.loads(text or "{}")
            except ValueError:
                body = {}
            auth_ok = self.headers.get("Authorization") == expect
            fmt = body.get("response_format") or {}
            record(log, {
                "method": "POST", "path": self.path, "auth_ok": auth_ok,
                "model": body.get("model"), "stream": bool(body.get("stream")),
                "response_format": fmt.get("type"),
                "tools": len(body.get("tools") or []),
                "has_store": "store" in body, "reasoning_effort": body.get("reasoning_effort"),
                # What the Assistant tells the provider about the database depends on the
                # organization's AI opt-in level.
                "has_schema_names": "The available database schema names are" in text,
                "no_schema_access": "You don't have access to any schemas." in text,
            })
            if self.path != "/v1/chat/completions":
                return self.reply(404, {"error": {"message": "this provider has no " + self.path}})
            if not auth_ok:
                return self.reply(401, {"error": {"message": "bad key"}})
            model = body.get("model") or "fake-model"
            if fmt.get("type") in ("json_schema", "json_object"):
                content = json.dumps({"title": "Fake title", "description": "Fake description"})
            else:
                content = "Hello from the fake provider"
            if not body.get("stream"):
                return self.reply(200, {
                    "id": "chatcmpl-fake", "object": "chat.completion", "created": 1, "model": model,
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                })
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()
            chunks = [
                {"choices": [{"index": 0, "delta": {"role": "assistant", "content": content}, "finish_reason": None}]},
                {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                 "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}},
            ]
            for c in chunks:
                c.update({"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": model})
                self.wfile.write(("data: " + json.dumps(c) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")

    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()


def trap(host, port, log):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((host, port))
    s.listen(8)
    while True:
        conn, peer = s.accept()
        record(log, {"trapped": "%s:%d" % (host, port), "from": peer[0]})
        conn.close()


if __name__ == "__main__":
    if sys.argv[1] == "serve":
        serve(int(sys.argv[2]), sys.argv[3])
    elif sys.argv[1] == "trap":
        trap(sys.argv[2], int(sys.argv[3]), sys.argv[4])
    else:
        sys.exit("usage: fake-openai.py serve PORT LOG | trap HOST PORT LOG")
