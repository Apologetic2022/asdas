#!/usr/bin/env python3
"""Same agentic tool loop as cache_toolloop_probe.py, over /v1/messages.

Claude Code talks this dialect, so this is the shape the reported usage
screenshot came from. It prints the Anthropic-side counters, which the
translator derives from the OpenAI payload the Cursor executor emits.

Usage: cache_toolloop_probe_messages.py [port] [turns] [model]
"""
import json
import sys
import urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8317"
TURNS = int(sys.argv[2]) if len(sys.argv) > 2 else 6
MODEL = sys.argv[3] if len(sys.argv) > 3 else "claude-sonnet-4-6"
BASE = f"http://127.0.0.1:{PORT}/v1/messages"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"

TOOLS = [{
    "name": "read_sensor",
    "description": "Read one numbered sensor. Sensors are numbered 1 upwards.",
    "input_schema": {
        "type": "object",
        "properties": {"index": {"type": "integer", "description": "sensor number"}},
        "required": ["index"],
    },
}]

BALLAST = ("You are a diagnostics agent for a sensor array. "
           "Follow the operator's instructions exactly and never guess a reading. " * 120)


def call(payload, timeout=300):
    req = urllib.request.Request(
        BASE,
        data=json.dumps(payload).encode(),
        headers={
            "Content-Type": "application/json",
            "X-Api-Key": KEY,
            "Anthropic-Version": "2023-06-01",
        },
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read().decode()


def parse_stream(raw):
    blocks, usage, stop = [], {}, None
    current = None
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        try:
            ev = json.loads(line[6:].strip())
        except json.JSONDecodeError:
            continue
        kind = ev.get("type")
        if kind == "content_block_start":
            current = dict(ev.get("content_block") or {})
            if current.get("type") == "tool_use":
                current["input_json"] = ""
            blocks.append(current)
        elif kind == "content_block_delta" and current is not None:
            delta = ev.get("delta") or {}
            if delta.get("type") == "text_delta":
                current["text"] = current.get("text", "") + delta.get("text", "")
            elif delta.get("type") == "input_json_delta":
                current["input_json"] = current.get("input_json", "") + delta.get("partial_json", "")
        elif kind == "message_delta":
            usage = ev.get("usage") or usage
            stop = (ev.get("delta") or {}).get("stop_reason") or stop
    return blocks, usage, stop


messages = [{"role": "user", "content": f"Read sensors 1 through 4 one at a time, "
                                        "calling read_sensor once per message, then report the total."}]

print(f"port={PORT} model={MODEL} turns={TURNS}")
print(f"{'turn':>4} {'fresh':>8} {'cached':>8} {'prompt':>8} {'out':>6} {'cache%':>7}  stop")
for turn in range(1, TURNS + 1):
    raw = call({
        "model": MODEL,
        "max_tokens": 1024,
        "stream": True,
        "system": [{"type": "text", "text": BALLAST}],
        "messages": messages,
        "tools": TOOLS,
    })
    blocks, usage, stop = parse_stream(raw)
    fresh = usage.get("input_tokens", 0)
    cached = usage.get("cache_read_input_tokens", 0)
    out = usage.get("output_tokens", 0)
    prompt = fresh + cached
    pct = (100.0 * cached / prompt) if prompt else 0.0
    print(f"{turn:>4} {fresh:>8} {cached:>8} {prompt:>8} {out:>6} {pct:>6.1f}%  {stop}")

    uses = [b for b in blocks if b.get("type") == "tool_use"]
    if not uses:
        break
    messages.append({"role": "assistant", "content": [
        {"type": "text", "text": b.get("text", "")} if b.get("type") == "text" else
        {"type": "tool_use", "id": b.get("id"), "name": b.get("name"),
         "input": json.loads(b.get("input_json") or "{}")}
        for b in blocks if b.get("type") in ("text", "tool_use") and (b.get("text") or b.get("type") == "tool_use")
    ]})
    messages.append({"role": "user", "content": [
        {"type": "tool_result", "tool_use_id": u.get("id"),
         "content": json.dumps({"reading": 40 + turn, "unit": "C", "log": "nominal " * 200})}
        for u in uses
    ]})
