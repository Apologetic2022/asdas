#!/usr/bin/env python3
"""Ask the model to exercise the Cursor harness's own workspace tools.

These are not the client's MCP tools: the harness advertises Shell, Read,
Write, Glob and friends on every run, and this headless gateway backs almost
none of them. What matters is that they fail in a way the model can act on.
The failure to look for is the opaque kind - a shell that reports no exit
status, a "No exec result", or a write that reports success and then cannot be
read back - because the model retries those for whole segments and then tells
the user its environment is broken.

Usage: builtin_tools_probe.py [port] [model]
"""
import json
import sys
import urllib.request
import uuid

PORT = sys.argv[1] if len(sys.argv) > 1 else "8317"
MODEL = sys.argv[2] if len(sys.argv) > 2 else "claude-sonnet-4-5"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
BASE = f"http://127.0.0.1:{PORT}/v1/chat/completions"

NONCE = uuid.uuid4().hex[:8]
TASK = f"""(session {NONCE}) Run this diagnostic with your OWN built-in workspace tools.
Do not ask me anything first, just do it and report.

1. Use your shell/terminal tool to run: echo HELLO-{NONCE}
2. Use your write tool to create probe-{NONCE}.txt containing PROBE-{NONCE}
3. Use your read tool to read probe-{NONCE}.txt back
4. Use your glob/list tool to list *.txt in the working directory

Then report one line each, exactly:
SHELL: <ok|fail> <what you got>
WRITE: <ok|fail> <what you got>
READ: <ok|fail> <what you got>
GLOB: <ok|fail> <what you got>
"""

req = urllib.request.Request(
    BASE,
    data=json.dumps({
        "model": MODEL, "stream": False,
        "messages": [{"role": "user", "content": TASK}],
        "max_tokens": 1500,
    }).encode(),
    headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
)
d = json.loads(urllib.request.urlopen(req, timeout=300).read())
choice = d["choices"][0]
report = choice["message"].get("content") or "<no text>"
print(f"model={MODEL} nonce={NONCE} finish={choice.get('finish_reason')}")
print(report)
print("usage:", json.dumps(d.get("usage")))

OPAQUE = ["no exit status", "no exec result", "unknown or expired", "no handler"]
hits = [phrase for phrase in OPAQUE if phrase in report.lower()]
print()
if hits:
    print("FAIL: the model still saw an opaque tool failure:", ", ".join(hits))
else:
    print("PASS: no opaque tool failures; unsupported tools said so plainly")
