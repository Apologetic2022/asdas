#!/usr/bin/env python3
"""Force a long agentic tool loop and report per-turn prompt-cache usage.

Each turn is one segment of a single live Cursor run. Cursor only reports usage
when the whole run ends, so without a per-run ledger every turn but the last
answers with no usage at all and is billed as a full uncached prompt.

Usage: toolloop_strict.py [port] [turns] [model]
"""
import json
import sys
import urllib.request
import uuid

PORT = sys.argv[1] if len(sys.argv) > 1 else "8317"
TURNS = int(sys.argv[2]) if len(sys.argv) > 2 else 6
MODEL = sys.argv[3] if len(sys.argv) > 3 else "claude-opus-5"
BASE = f"http://127.0.0.1:{PORT}/v1/chat/completions"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"

TOOLS = [{
    "type": "function",
    "function": {
        "name": "read_sensor",
        "description": "Read exactly one numbered sensor. Only ever one per call.",
        "parameters": {
            "type": "object",
            "properties": {"index": {"type": "integer"}},
            "required": ["index"],
        },
    },
}]

BALLAST = ("You are a diagnostics agent for a large sensor array. "
           "Follow the operator's instructions exactly and never guess a reading. " * 400)

# A per-run nonce keeps each run a distinct conversation. Without it a repeated
# run is an exact duplicate of an earlier one and resumes that conversation from
# the checkpoint cache instead of starting a new turn.
NONCE = uuid.uuid4().hex[:8]

messages = [
    {"role": "system", "content": BALLAST},
    {"role": "user", "content":
        f"Session {NONCE}. Read sensors 1 through {TURNS}. You must call read_sensor "
        f"for exactly ONE sensor per reply, never more than one tool call per reply, "
        f"and you must not summarise until you have read all {TURNS} sensors."},
]


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


print(f"port={PORT} model={MODEL} turns={TURNS}")
print(f"{'turn':>4} {'prompt':>9} {'cached':>9} {'fresh':>9} {'compl':>7} {'cache%':>7}  finish")
rows = []
missing = 0
for turn in range(1, TURNS + 1):
    raw = call({"model": MODEL, "stream": True, "messages": messages, "tools": TOOLS})
    content, calls, finish, usage = parse_stream(raw)
    if usage is None:
        missing += 1
    usage = usage or {}
    prompt = usage.get("prompt_tokens", 0)
    cached = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
    compl = usage.get("completion_tokens", 0)
    pct = (100.0 * cached / prompt) if prompt else 0.0
    print(f"{turn:>4} {prompt:>9} {cached:>9} {prompt - cached:>9} {compl:>7} {pct:>6.1f}%  {finish}")
    rows.append((prompt, cached))
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
            "content": json.dumps({"reading": 40 + turn, "unit": "C", "log": "nominal " * 300}),
        })

billed = sum(r[0] for r in rows)
fresh = sum(r[0] - r[1] for r in rows)
print()
print(f"segments with NO usage reported at all: {missing} / {len(rows)}")
if billed:
    print(f"prompt tokens billed across the run:    {billed}")
    print(f"of which read fresh (not cached):       {fresh}")
    print(f"run-wide cache hit rate:                {100.0 * (1 - fresh / billed):.1f}%")
else:
    print("no prompt tokens reported at all")
