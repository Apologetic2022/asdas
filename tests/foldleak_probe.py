"""Continue a tool conversation whose live session is gone.

That is the path a desktop client hits after any gateway restart or idle
timeout, and it is the one that rebuilds the history as prose. The question is
whether the next turn still comes back as a real tool_use block or as text that
merely looks like one.
"""

import json
import sys
import urllib.error
import urllib.request

HOST = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8317"
KEY = sys.argv[2] if len(sys.argv) > 2 else "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODEL = sys.argv[3] if len(sys.argv) > 3 else "claude-opus-5"

TOOLS = [
    {
        "name": "AskUserQuestion",
        "description": "Ask the user a multiple-choice question to confirm a requirement.",
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
                            "options": {"type": "array", "items": {"type": "object", "properties": {"label": {"type": "string"}, "description": {"type": "string"}}}},
                            "multiSelect": {"type": "boolean"},
                        },
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
]


def call(messages):
    body = {"model": MODEL, "max_tokens": 1500, "tools": TOOLS, "messages": messages, "stream": True}
    req = urllib.request.Request(
        HOST + "/v1/messages",
        json.dumps(body).encode(),
        {"Content-Type": "application/json", "x-api-key": KEY, "anthropic-version": "2023-06-01"},
    )
    raw = urllib.request.urlopen(req, timeout=300).read().decode()
    blocks, stop, text = {}, None, ""
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        try:
            chunk = json.loads(line[6:])
        except ValueError:
            continue
        idx = chunk.get("index")
        if chunk.get("type") == "content_block_start":
            blocks[idx] = dict(chunk.get("content_block") or {})
            blocks[idx]["_json"] = ""
        elif chunk.get("type") == "content_block_delta":
            d = chunk.get("delta") or {}
            b = blocks.setdefault(idx, {"type": "text", "text": "", "_json": ""})
            if d.get("type") == "text_delta":
                b["text"] = b.get("text", "") + d.get("text", "")
                text += d.get("text", "")
            elif d.get("type") == "input_json_delta":
                b["_json"] += d.get("partial_json", "")
        elif chunk.get("type") == "message_delta":
            stop = (chunk.get("delta") or {}).get("stop_reason") or stop
    content = []
    for _, b in sorted(blocks.items()):
        raw_json = b.pop("_json", "")
        if b.get("type") == "tool_use":
            try:
                b["input"] = json.loads(raw_json) if raw_json.strip() else {}
            except ValueError:
                b["input"] = {}
        content.append(b)
    return content, stop, text


def report(step, content, stop, text):
    calls = [b for b in content if b.get("type") == "tool_use"]
    print("step %d: stop=%s tool_use=%d %s" % (step, stop, len(calls), " ".join(c.get("name", "?") for c in calls)))
    leaks = [m for m in ("[called tool", "toolu_", "tool_use_id", "mcp_") if m in text]
    if leaks:
        print("   !! the reply contains a tool call written as TEXT: %s" % ", ".join(leaks))
        print("   ", text.strip()[:500].replace("\n", "\n    "))
        return False
    if text.strip():
        print("   text:", text.strip()[:160].replace("\n", " "))
    return True


messages = [
    {
        "role": "user",
        "content": "Build me a landing page. First confirm the colour scheme with me using AskUserQuestion. Do not answer in prose.",
    }
]

ok = True
content, stop, text = call(messages)
ok &= report(1, content, stop, text)
calls = [b for b in content if b.get("type") == "tool_use"]
if not calls:
    print("RESULT: BROKEN (no first tool call to continue from)")
    sys.exit(1)

messages.append({"role": "assistant", "content": content})
messages.append(
    {
        "role": "user",
        "content": [
            {"type": "tool_result", "tool_use_id": c["id"], "content": "The user chose: Dark + violet accent."}
            for c in calls
        ],
    }
)

import os
import subprocess
import time

if os.environ.get("RESTART_CMD"):
    print("--- dropping the live session: %s ---" % os.environ["RESTART_CMD"])
    subprocess.run(os.environ["RESTART_CMD"], shell=True, check=False)
    time.sleep(9)

for step in (2, 3):
    messages.append(
        {"role": "user", "content": "Good. Now confirm the framework with AskUserQuestion before writing anything."}
        if step == 2
        else {"role": "user", "content": "Now write the page to /tmp/page.html with Write."}
    )
    try:
        content, stop, text = call(messages)
    except urllib.error.HTTPError as exc:
        print("step %d: HTTP %d %s" % (step, exc.code, exc.read()[:200].decode("utf8", "replace")))
        ok = False
        break
    ok &= report(step, content, stop, text)
    calls = [b for b in content if b.get("type") == "tool_use"]
    if not calls:
        ok = False
        print("   !! no tool call at all on step %d" % step)
    messages.append({"role": "assistant", "content": content})
    if calls:
        messages.append(
            {
                "role": "user",
                "content": [{"type": "tool_result", "tool_use_id": c["id"], "content": "done"} for c in calls],
            }
        )

print("RESULT:", "ok" if ok else "BROKEN")
