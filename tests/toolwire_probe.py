"""Inspect what a client actually receives for a tool call.

The report is that a confirmation tool renders as a raw code block instead of a
UI card, so what matters is the exact shape of the stream: whether the tool_use
block is announced as its own content block, whether the name that comes back
is the one the client declared, whether the accumulated input parses as JSON,
and whether the message stops with stop_reason "tool_use". A client that gets
any of those wrong falls back to showing the raw payload.
"""

import json
import sys
import urllib.error
import urllib.request

HOST = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8317"
KEY = sys.argv[2] if len(sys.argv) > 2 else "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODEL = sys.argv[3] if len(sys.argv) > 3 else "claude-opus-5"
DUMP = "--dump" in sys.argv

# The two tools from the report, named exactly as the desktop client declares
# them, alongside a plain one as a control.
TOOLS = [
    {
        "name": "AskUserQuestion",
        "description": "Ask the user a multiple-choice question to confirm a requirement before proceeding.",
        "input_schema": {
            "type": "object",
            "properties": {
                "questions": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "question": {"type": "string"},
                            "header": {"type": "string"},
                            "options": {
                                "type": "array",
                                "items": {
                                    "type": "object",
                                    "properties": {"label": {"type": "string"}, "description": {"type": "string"}},
                                    "required": ["label", "description"],
                                },
                            },
                            "multiSelect": {"type": "boolean"},
                        },
                        "required": ["question", "header", "options", "multiSelect"],
                    },
                }
            },
            "required": ["questions"],
        },
    },
    {
        "name": "Write",
        "description": "Write a file to the user's filesystem.",
        "input_schema": {
            "type": "object",
            "properties": {"file_path": {"type": "string"}, "content": {"type": "string"}},
            "required": ["file_path", "content"],
        },
    },
    {
        "name": "Bash",
        "description": "Run a shell command.",
        "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]},
    },
]

PROMPT = (
    "I want you to build me a landing page, but first confirm the details with me. "
    "Use the AskUserQuestion tool to ask which colour scheme and which framework I want. "
    "Do not answer in prose; call the tool."
)


def run():
    body = {
        "model": MODEL,
        "max_tokens": 1024,
        "tools": TOOLS,
        "messages": [{"role": "user", "content": PROMPT}],
        "stream": True,
    }
    req = urllib.request.Request(
        HOST + "/v1/messages",
        json.dumps(body).encode(),
        {"Content-Type": "application/json", "x-api-key": KEY, "anthropic-version": "2023-06-01"},
    )
    raw = urllib.request.urlopen(req, timeout=300).read().decode()
    if DUMP:
        print(raw[:4000])
        print("...")

    events, blocks, stop_reason, text = [], {}, None, ""
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        try:
            chunk = json.loads(line[6:])
        except ValueError:
            continue
        events.append(chunk.get("type"))
        idx = chunk.get("index")
        if chunk.get("type") == "content_block_start":
            blocks[idx] = dict(chunk.get("content_block") or {})
            blocks[idx]["_json"] = ""
        elif chunk.get("type") == "content_block_delta":
            delta = chunk.get("delta") or {}
            block = blocks.setdefault(idx, {"type": "text", "text": "", "_json": ""})
            if delta.get("type") == "text_delta":
                block["text"] = block.get("text", "") + delta.get("text", "")
                text += delta.get("text", "")
            elif delta.get("type") == "input_json_delta":
                block["_json"] += delta.get("partial_json", "")
        elif chunk.get("type") == "message_delta":
            stop_reason = (chunk.get("delta") or {}).get("stop_reason") or stop_reason

    print("event types:", " ".join(dict.fromkeys(e for e in events if e)))
    print("stop_reason:", stop_reason)
    ok = True
    tool_blocks = [b for b in blocks.values() if b.get("type") == "tool_use"]
    if not tool_blocks:
        ok = False
        print("NO tool_use BLOCK. The model answered in text instead:")
        print("   ", (text[:400] or "(empty)").replace("\n", "\n    "))
    for block in tool_blocks:
        raw_json = block.get("_json", "")
        try:
            parsed = json.loads(raw_json) if raw_json.strip() else block.get("input", {})
            parsed_ok = True
        except ValueError:
            parsed, parsed_ok = raw_json, False
        print("tool_use  id=%s  name=%s  json_ok=%s" % (block.get("id"), block.get("name"), parsed_ok))
        print("   input:", json.dumps(parsed, ensure_ascii=False)[:300] if parsed_ok else repr(parsed)[:300])
        if not parsed_ok:
            ok = False
        if block.get("name") not in [t["name"] for t in TOOLS]:
            ok = False
            print("   !! name is not one the client declared")
        if not str(block.get("id", "")).startswith("toolu"):
            print("   !! id %r is not an Anthropic tool_use id" % block.get("id"))
    if tool_blocks and stop_reason != "tool_use":
        ok = False
        print("!! stop_reason is %r, a client needs 'tool_use' to render the call" % stop_reason)
    if text.strip() and tool_blocks:
        print("text alongside the call:", text.strip()[:200])
    print("RESULT:", "ok" if ok else "BROKEN")


try:
    run()
except urllib.error.HTTPError as exc:
    print("HTTP %d %s" % (exc.code, exc.read()[:400].decode("utf8", "replace")))
