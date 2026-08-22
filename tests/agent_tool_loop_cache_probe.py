"""Drive /v1/messages the way an agent client actually drives it.

Claude Code does not hold one HTTP request open across a tool loop. Each step is
a separate request carrying the whole conversation so far, growing by one
tool_use/tool_result pair each time, and echoing back the exact tool_use id and
input the gateway emitted. That last part matters: a fabricated id cannot match
the parked upstream session, so a probe that invents one measures a path no real
client takes.

Prints the three Anthropic counters, which are disjoint, plus their sum, so a
step that reports its whole prompt as a cache creation is obvious.
"""

import json
import sys
import urllib.error
import urllib.request

HOST = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8317"
KEY = sys.argv[2] if len(sys.argv) > 2 else "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODEL = sys.argv[3] if len(sys.argv) > 3 else "claude-opus-5"
STEPS = int(sys.argv[4]) if len(sys.argv) > 4 else 6
STREAM = (sys.argv[5] if len(sys.argv) > 5 else "stream") == "stream"

FILLER = "Reference material section: the quick brown fox jumps over the lazy dog.\n" * 900

TOOLS = [
    {
        "name": "Bash",
        "description": "Run a shell command and return its output.",
        "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]},
    }
]

SYSTEM = [
    {"type": "text", "text": "You are a coding agent. Use the Bash tool to inspect the project. Always call a tool."},
    {"type": "text", "text": FILLER, "cache_control": {"type": "ephemeral"}},
]


def call(messages):
    body = {
        "model": MODEL,
        "max_tokens": 256,
        "system": SYSTEM,
        "tools": TOOLS,
        "tool_choice": {"type": "any"},
        "messages": messages,
        "stream": STREAM,
    }
    req = urllib.request.Request(
        HOST + "/v1/messages",
        json.dumps(body).encode(),
        {
            "Content-Type": "application/json",
            "x-api-key": KEY,
            "anthropic-version": "2023-06-01",
        },
    )
    raw = urllib.request.urlopen(req, timeout=300).read().decode()
    if not STREAM:
        return json.loads(raw)

    # Rebuild the content blocks from the event stream so the tool_use id and
    # input can be echoed back verbatim.
    usage, blocks = {}, {}
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        try:
            chunk = json.loads(line[6:])
        except ValueError:
            continue
        if isinstance(chunk.get("message"), dict) and chunk["message"].get("usage"):
            usage.update(chunk["message"]["usage"])
        if chunk.get("usage"):
            usage.update(chunk["usage"])
        idx = chunk.get("index")
        if chunk.get("type") == "content_block_start":
            blocks[idx] = dict(chunk.get("content_block") or {})
            blocks[idx].setdefault("_json", "")
        elif chunk.get("type") == "content_block_delta":
            delta = chunk.get("delta") or {}
            block = blocks.setdefault(idx, {"type": "text", "text": "", "_json": ""})
            if delta.get("type") == "text_delta":
                block["text"] = block.get("text", "") + delta.get("text", "")
            elif delta.get("type") == "input_json_delta":
                block["_json"] += delta.get("partial_json", "")

    content = []
    for _, block in sorted(blocks.items()):
        raw_json = block.pop("_json", "")
        if block.get("type") == "tool_use":
            try:
                block["input"] = json.loads(raw_json) if raw_json.strip() else {}
            except ValueError:
                block["input"] = {}
        content.append(block)
    return {"usage": usage, "content": content}


messages = [{"role": "user", "content": [{"type": "text", "text": "Investigate this repo step by step."}]}]

print("%-5s %9s %9s %9s %9s   %s" % ("step", "input", "read", "create", "sum", "note"))
for step in range(1, STEPS + 1):
    try:
        result = call(messages)
    except urllib.error.HTTPError as exc:
        print("%-5d FAIL %d %s" % (step, exc.code, exc.read()[:200].decode("utf8", "replace")))
        break
    u = result.get("usage", {})
    inp = u.get("input_tokens", 0)
    read = u.get("cache_read_input_tokens", 0)
    create = u.get("cache_creation_input_tokens", 0)
    note = ""
    if create > read and create > 1000:
        note = "<-- writes its whole prompt"
    print("%-5d %9d %9d %9d %9d   %s" % (step, inp, read, create, inp + read + create, note))

    content = [b for b in result.get("content", []) if b.get("type") in ("text", "tool_use") and (b.get("text") or b.get("type") == "tool_use")]
    calls = [b for b in content if b.get("type") == "tool_use"]
    if not calls:
        print("      (no tool call; the loop ends here)")
        break
    messages.append({"role": "assistant", "content": content})
    messages.append(
        {
            "role": "user",
            "content": [
                {"type": "tool_result", "tool_use_id": c["id"], "content": "file_%d.txt\nnotes_%d.md\n" % (step, step)}
                for c in calls
            ],
        }
    )
