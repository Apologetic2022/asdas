#!/usr/bin/env python3
"""Ask for a parallel tool batch and report how much of it survives one turn.

Upstream emits a parallel batch as back-to-back frames, ~13ms end to end, which
may or may not land in a single read of the response body. A read loop that
parks the segment on the first tool call it decodes drops the rest of the batch
into later turns, so the client gets one call per round trip.

Not every model batches: claude-sonnet-4-5 and claude-fable-5 are serialised by
the agent harness upstream and answer one call at a time no matter what. Use
grok-4.6 or claude-opus-5, which do batch.

Usage: parallel_toolcall_probe.py [port] [runs] [model]
"""
import json
import sys
import urllib.request
import uuid
from collections import Counter

PORT = sys.argv[1] if len(sys.argv) > 1 else "8317"
RUNS = int(sys.argv[2]) if len(sys.argv) > 2 else 8
MODEL = sys.argv[3] if len(sys.argv) > 3 else "grok-4.6"
BASE = f"http://127.0.0.1:{PORT}/v1/chat/completions"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"

CITIES = ["Paris", "Tokyo", "Lima"]
TOOLS = [{
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get the current weather for exactly one city.",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}]


def run(stream):
    nonce = uuid.uuid4().hex[:8]
    body = {
        "model": MODEL,
        "stream": stream,
        "messages": [{"role": "user", "content":
                      f"(session {nonce}) Call get_weather three times in ONE reply, in "
                      f"parallel, for {', '.join(CITIES)}. Issue all three calls together."}],
        "tools": TOOLS,
        "max_tokens": 400,
    }
    req = urllib.request.Request(
        BASE, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"})
    with urllib.request.urlopen(req, timeout=180) as resp:
        raw = resp.read().decode()

    if not stream:
        d = json.loads(raw)
        return [c["function"]["arguments"]
                for c in (d["choices"][0]["message"].get("tool_calls") or [])]

    calls = []
    for line in raw.splitlines():
        if not line.startswith("data: ") or line[6:].strip() == "[DONE]":
            continue
        try:
            chunk = json.loads(line[6:])
        except json.JSONDecodeError:
            continue
        for choice in chunk.get("choices", []):
            calls.extend((choice.get("delta") or {}).get("tool_calls") or [])
    return [c.get("function", {}).get("arguments") for c in calls]


print(f"model={MODEL} runs={RUNS} per mode, want {len(CITIES)} calls each")
for mode in (False, True):
    counts = Counter()
    for i in range(RUNS):
        try:
            args = run(mode)
        except Exception as exc:
            counts["err"] += 1
            print(f"  {'stream' if mode else 'unary '} run {i + 1}: ERR {exc}")
            continue
        counts[len(args)] += 1
        print(f"  {'stream' if mode else 'unary '} run {i + 1}: {len(args)} call(s) "
              f"{[json.loads(a).get('city') for a in args]}")
    full = counts[len(CITIES)]
    verdict = "PASS" if full == RUNS else "FAIL"
    print(f"  {verdict}: full batch in {full}/{RUNS} "
          f"{'streaming' if mode else 'non-streaming'} runs\n")
