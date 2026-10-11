#!/usr/bin/env python3
"""A scripted OpenAI-compatible chat-completions server, for testing the
agent without paying for model calls.

    python3 stub_model.py --port 8399 [--delay 30] [--log requests.jsonl]

Containers reach it as http://host.docker.internal:8399/v1. Each request is
answered from the conversation so far:

- no tools in the request (OpenCode's title and summary calls): a short title;
- tools, and no tool result yet: one bash call that lists the working directory
  and writes hello.txt there, after waiting --delay seconds so a run can be watched
  (a stream sends its first chunk before the wait);
- a tool result present: a final text message.

Streaming (SSE) and non-streaming requests are both answered, with token
usage. Every request is logged with the Authorization header it carried, so
a test can check which key reached the endpoint.
"""

import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

COMMAND = "ls -la && echo hello > hello.txt && cat hello.txt"
USAGE = {"prompt_tokens": 120, "completion_tokens": 12, "total_tokens": 132, "prompt_tokens_details": {"cached_tokens": 0}, "completion_tokens_details": {"reasoning_tokens": 0}}


def reply(body):
    """(message, finish reason, whether to wait --delay first)."""
    messages = body.get("messages", [])
    if not body.get("tools"):
        return {"content": "Say hello"}, "stop", False
    if any(m.get("role") == "tool" for m in messages):
        return {"content": "Done: hello.txt says hello."}, "stop", False
    call = {
        "id": "call_stub_1",
        "type": "function",
        "function": {
            "name": "bash",
            "arguments": json.dumps(
                {"command": COMMAND, "description": "List files and write hello.txt"}
            ),
        },
    }
    return {"content": None, "tool_calls": [call]}, "tool_calls", True


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def _send(self, status, payload, content_type="application/json"):
        data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            self._send(200, {"object": "list", "data": [{"id": "scripted", "object": "model"}]})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        record = {
            "time": time.time(),
            "path": self.path,
            "authorization": self.headers.get("Authorization"),
            "x-api-key": self.headers.get("x-api-key"),
            "stream": bool(body.get("stream")),
            "tools": len(body.get("tools") or []),
            "roles": [m.get("role") for m in body.get("messages", [])],
        }
        if self.server.log:
            with open(self.server.log, "a") as f:
                f.write(json.dumps(record) + "\n")
        if not self.path.endswith("/chat/completions"):
            self._send(404, {"error": "not found"})
            return
        message, finish, wait = reply(body)
        model = body.get("model", "scripted")
        now = int(time.time())
        if not body.get("stream"):
            if wait:
                time.sleep(self.server.delay)
            self._send(200, {
                "id": "chatcmpl-stub", "object": "chat.completion", "created": now, "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant", **message}, "finish_reason": finish}],
                "usage": USAGE,
            })
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        def event(delta, finish_reason=None, usage=None):
            chunk = {
                "id": "chatcmpl-stub", "object": "chat.completion.chunk", "created": now, "model": model,
                "choices": [{"index": 0, "delta": delta, "finish_reason": finish_reason}],
            }
            if usage:
                chunk["usage"] = usage
            self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
            self.wfile.flush()

        # Headers and the first chunk go out before the wait, as a real
        # endpoint's would.
        event({"role": "assistant", "content": ""})
        if wait:
            time.sleep(self.server.delay)
        if message.get("tool_calls"):
            call = message["tool_calls"][0]
            event({"tool_calls": [{"index": 0, **call}]})
        else:
            for word in message["content"].split(" "):
                event({"content": word + " "})
        event({}, finish, USAGE)
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, default=8399)
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--delay", type=float, default=0, help="seconds to wait before the tool call")
    p.add_argument("--log", help="append one JSON line per request here")
    a = p.parse_args()
    server = ThreadingHTTPServer((a.host, a.port), Handler)
    server.delay, server.log = a.delay, a.log
    print(f"stub model on http://{a.host}:{a.port}/v1", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
