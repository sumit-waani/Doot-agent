#!/usr/bin/env python3
"""A minimal OpenAI-compatible chat completions server, for exercising the agent
loop without spending money or depending on the network.

It streams responses from a scripted scenario file, one entry per call, in the
same SSE shape the real API uses. This is what makes it possible to verify the
loop's control flow -- tool calls, plan approval, pausing, compaction -- against
real HTTP rather than a mocked client.

Usage:
    stub.py <port> <scenario.json>

Scenario format: a JSON list. Each entry is one response:
    {
      "content": "text to stream",
      "tool_calls": [{"name": "exec", "arguments": {"command": "ls"}}],
      "prompt_tokens": 1000,
      "completion_tokens": 50
    }

Requests past the end of the list repeat the last entry, so a loop that keeps
calling does not crash the stub.
"""

import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SCENARIO = []
CALLS = []


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # keep the test output readable
        pass

    def do_GET(self):
        if self.path == "/__calls":
            body = json.dumps(CALLS).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_error(404)

    def do_POST(self):
        if not self.path.endswith("/chat/completions"):
            self.send_error(404)
            return

        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        try:
            request = json.loads(raw)
        except Exception:
            request = {}

        # Record what the agent actually sent, so the test can assert on the
        # prompt, the tool schemas, and whether usage was requested.
        CALLS.append({
            "model": request.get("model"),
            "n_messages": len(request.get("messages", [])),
            "roles": [m.get("role") for m in request.get("messages", [])],
            "tool_names": sorted(
                t.get("function", {}).get("name")
                for t in request.get("tools", []) or []
            ),
            "include_usage": bool(
                (request.get("stream_options") or {}).get("include_usage")
            ),
            "system_head": next(
                (m.get("content", "")[:80] for m in request.get("messages", [])
                 if m.get("role") == "system"),
                "",
            ),
            "last_user": next(
                (m.get("content", "")[:120] for m in reversed(request.get("messages", []))
                 if m.get("role") == "user"),
                "",
            ),
        })

        index = min(len(CALLS) - 1, len(SCENARIO) - 1)
        step = SCENARIO[index] if SCENARIO else {}

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        for chunk in build_chunks(step):
            self.write_chunk("data: " + json.dumps(chunk) + "\n\n")
            time.sleep(0.005)
        self.write_chunk("data: [DONE]\n\n")
        self.write_chunk("")

    def write_chunk(self, text):
        data = text.encode()
        self.wfile.write(("%X\r\n" % len(data)).encode() + data + b"\r\n")
        self.wfile.flush()


def envelope(choices, usage=None):
    out = {
        "id": "chatcmpl-stub",
        "object": "chat.completion.chunk",
        "created": 0,
        "model": "stub-model",
        "choices": choices,
    }
    if usage is not None:
        out["usage"] = usage
    return out


def build_chunks(step):
    chunks = []

    chunks.append(envelope([{
        "index": 0,
        "delta": {"role": "assistant", "content": ""},
        "finish_reason": None,
    }]))

    # Content is split so the agent's delta batching is genuinely exercised.
    content = step.get("content", "")
    for i in range(0, len(content), 12):
        chunks.append(envelope([{
            "index": 0,
            "delta": {"content": content[i:i + 12]},
            "finish_reason": None,
        }]))

    tool_calls = step.get("tool_calls") or []
    for position, call in enumerate(tool_calls):
        call_id = call.get("id", "call_%d_%d" % (len(CALLS), position))

        # Name first, then arguments in fragments: the accumulator has to stitch
        # these together the same way it would for the real API.
        chunks.append(envelope([{
            "index": 0,
            "delta": {"tool_calls": [{
                "index": position,
                "id": call_id,
                "type": "function",
                "function": {"name": call["name"], "arguments": ""},
            }]},
            "finish_reason": None,
        }]))

        arguments = call.get("arguments", {})
        encoded = arguments if isinstance(arguments, str) else json.dumps(arguments)
        for i in range(0, len(encoded), 16):
            chunks.append(envelope([{
                "index": 0,
                "delta": {"tool_calls": [{
                    "index": position,
                    "function": {"arguments": encoded[i:i + 16]},
                }]},
                "finish_reason": None,
            }]))

    chunks.append(envelope([{
        "index": 0,
        "delta": {},
        "finish_reason": "tool_calls" if tool_calls else "stop",
    }]))

    # Usage arrives in a final choices-empty chunk, exactly as the real API does
    # when stream_options.include_usage is set.
    prompt = step.get("prompt_tokens", 1200)
    completion = step.get("completion_tokens", 40)
    chunks.append(envelope([], usage={
        "prompt_tokens": prompt,
        "completion_tokens": completion,
        "total_tokens": prompt + completion,
        "prompt_tokens_details": {"cached_tokens": step.get("cached_tokens", 0)},
    }))

    return chunks


def main():
    global SCENARIO
    port = int(sys.argv[1])
    with open(sys.argv[2]) as f:
        SCENARIO = json.load(f)

    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
