"""Declare the whole Claude Code tool set at once and see whether it survives.

The wire rename exists because Anthropic refuses a request that declares a tool
twice, which happens when a client tool shares a name with one of Cursor's own.
One request carrying every name answers whether that still happens, and which
name it is, far more cheaply than probing thirty times against a single
rate-limited account.

A 429 is a quota answer, not a verdict, so it is retried rather than recorded.
"""

import json
import sys
import time
import urllib.error
import urllib.request

HOST, KEY, MODEL = sys.argv[1], sys.argv[2], sys.argv[3]
NAMES = sys.argv[4].split(",")

tools = [
    {"name": n, "description": "Tool %s." % n, "input_schema": {"type": "object", "properties": {"x": {"type": "string"}}}}
    for n in NAMES
]
body = {
    "model": MODEL,
    "max_tokens": 16,
    "tools": tools,
    "messages": [{"role": "user", "content": "Say OK and nothing else."}],
    "stream": False,
}

for attempt in range(1, 8):
    req = urllib.request.Request(
        HOST + "/v1/messages",
        json.dumps(body).encode(),
        {"Content-Type": "application/json", "x-api-key": KEY, "anthropic-version": "2023-06-01"},
    )
    try:
        payload = json.loads(urllib.request.urlopen(req, timeout=180).read())
        text = "".join(b.get("text", "") for b in payload.get("content", []) if b.get("type") == "text")
        print("ACCEPTED all %d names. reply: %s" % (len(NAMES), text.strip()[:80]))
        break
    except urllib.error.HTTPError as exc:
        detail = exc.read()[:300].decode("utf8", "replace").replace("\n", " ")
        if exc.code == 429 or "cooling down" in detail or "resource_exhausted" in detail:
            wait = 20 * attempt
            print("attempt %d: quota (%d), retrying in %ds" % (attempt, exc.code, wait))
            time.sleep(wait)
            continue
        print("REJECTED %d: %s" % (exc.code, detail))
        break
else:
    print("gave up: the account stayed rate limited")
