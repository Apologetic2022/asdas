#!/usr/bin/env python3
"""Measure per-turn usage across an agentic tool loop on the CPA gateway.

This is the shape that exposed the caching bug: a client that re-sends its whole
history every turn while the model keeps calling tools. Each turn is one segment
of a single live Cursor run, and Cursor only reports usage when the run ends, so
every turn but the last used to be billed as a full, uncached prompt.

Usage: cache_toolloop_probe.py [port] [turns] [model]
"""
import json
import sys
import urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8317"
TURNS = int(sys.argv[2]) if len(sys.argv) > 2 else 6
MODEL = sys.argv[3] if len(sys.argv) > 3 else "grok-4.5"
BASE = f"http://127.0.0.1:{PORT}/v1/chat/completions"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"

TOOLS = [{
    "type": "function",
    "function": {
        "name": "read_sensor",
        "description": "Read one numbered sensor. Sensors are numbered 1 upwards.",
        "parameters": {
            "type": "object",
            "properties": {"index": {"type": "integer", "description": "sensor number"}},
            "required": ["index"],
        },
    },
}]

# A bulky, stable prefix so a cache hit is unmistakable in the token counts.
BALLAST = ("You are a diagnostics agent for a sensor array. "
           "Follow the operator's instructions exactly and never guess a reading. " * 120)


def call(payload, timeout=300):
    req = urllib.request.Request(
        BASE,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read().decode()


def parse_stream(raw):
    tool_calls, finish, usage, content = [], None, None, ""
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        data = line[6:].strip()
        if data == "[DONE]":
            continue
        try:
            chunk = json.loads(data)
        except json.JSONDecodeError:
            continue
        for ch in chunk.get("choices", []):
            delta = ch.get("delta", {})
            content += delta.get("content") or ""
            tool_calls.extend(delta.get("tool_calls") or [])
            if ch.get("finish_reason"):
                finish = ch["finish_reason"]
        if chunk.get("usage"):
            usage = chunk["usage"]
    return content, tool_calls, finish, usage


messages = [
    {"role": "system", "content": BALLAST},
    {"role": "user", "content": f"Read sensors 1 through {TURNS} one at a time, "
                                "calling read_sensor once per message, then report the total."},
]

print(f"port={PORT} model={MODEL} turns={TURNS}")
print(f"{'turn':>4} {'prompt':>8} {'cached':>8} {'fresh':>8} {'compl':>7} {'cache%':>7}  finish")
rows = []
for turn in range(1, TURNS + 1):
    raw = call({"model": MODEL, "stream": True, "messages": messages, "tools": TOOLS})
    content, calls, finish, usage = parse_stream(raw)
    usage = usage or {}
    prompt = usage.get("prompt_tokens", 0)
    cached = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
    compl = usage.get("completion_tokens", 0)
    pct = (100.0 * cached / prompt) if prompt else 0.0
    print(f"{turn:>4} {prompt:>8} {cached:>8} {prompt - cached:>8} {compl:>7} {pct:>6.1f}%  {finish}")
    rows.append((prompt, cached, compl))

    if not calls:
        break
    messages.append({
        "role": "assistant",
        "content": content,
        "tool_calls": [{
            "id": c.get("id"),
            "type": "function",
            "function": {
                "name": c.get("function", {}).get("name"),
                "arguments": c.get("function", {}).get("arguments") or "{}",
            },
        } for c in calls],
    })
    for c in calls:
        messages.append({
            "role": "tool",
            "tool_call_id": c.get("id"),
            "name": c.get("function", {}).get("name"),
            # A chunky result so the prompt visibly grows each turn.
            "content": json.dumps({"reading": 40 + turn, "unit": "C", "log": "nominal " * 200}),
        })

billed_prompt = sum(r[0] for r in rows)
billed_fresh = sum(r[0] - r[1] for r in rows)
print()
print(f"prompt tokens billed across the run: {billed_prompt}")
print(f"of which read fresh (not cached):    {billed_fresh}")
print(f"run-wide cache hit rate:             {100.0 * (1 - billed_fresh / billed_prompt):.1f}%"
      if billed_prompt else "no prompt tokens reported")
