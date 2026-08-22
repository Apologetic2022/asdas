"""Reproduce the reported session: the full Claude Code toolset, a task that
needs a colliding tool, and a check that the gateway's wire naming stays out of
both the reply and the model's reasoning.

The original symptom was not a malformed call. It was the model noticing that
its toolset had been renamed underneath it, deciding the instruction to use the
renamed copies might be a prompt injection, and saying so to the user.
"""

import json
import sys
import urllib.error
import urllib.request

HOST, KEY, MODEL = sys.argv[1], sys.argv[2], sys.argv[3]

NAMES = ("Agent,Artifact,AskUserQuestion,Bash,CronCreate,CronDelete,CronList,DesignSync,Edit,"
         "EnterPlanMode,EnterWorktree,ExitPlanMode,ExitWorktree,Glob,Grep,Monitor,NotebookEdit,"
         "PowerShell,PushNotification,Read,ReportFindings,ScheduleWakeup,SendMessage,Skill,"
         "TaskOutput,TaskStop,WebFetch,WebSearch,Workflow,Write").split(",")

SCHEMAS = {
    "Write": {"type": "object", "properties": {"file_path": {"type": "string"}, "content": {"type": "string"}}, "required": ["file_path", "content"]},
    "AskUserQuestion": {"type": "object", "properties": {"questions": {"type": "array", "items": {"type": "object"}}}, "required": ["questions"]},
}
TOOLS = [
    {"name": n, "description": "The %s tool." % n, "input_schema": SCHEMAS.get(n, {"type": "object", "properties": {"x": {"type": "string"}}})}
    for n in NAMES
]

body = {
    "model": MODEL,
    "max_tokens": 2000,
    "tools": TOOLS,
    "messages": [{"role": "user", "content": "Write a one-paragraph HTML page about pelicans to /tmp/pelican.html. Use your Write tool."}],
    "stream": True,
}
req = urllib.request.Request(
    HOST + "/v1/messages",
    json.dumps(body).encode(),
    {"Content-Type": "application/json", "x-api-key": KEY, "anthropic-version": "2023-06-01"},
)
try:
    raw = urllib.request.urlopen(req, timeout=300).read().decode()
except urllib.error.HTTPError as exc:
    print("HTTP %d %s" % (exc.code, exc.read()[:300].decode("utf8", "replace")))
    sys.exit(1)

blocks, stop, text, thinking = {}, None, "", ""
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
    elif chunk.get("type") == "content_block_delta":
        d = chunk.get("delta") or {}
        if d.get("type") == "text_delta":
            text += d.get("text", "")
        elif d.get("type") == "thinking_delta":
            thinking += d.get("thinking", "")
    elif chunk.get("type") == "message_delta":
        stop = (chunk.get("delta") or {}).get("stop_reason") or stop

calls = [b for b in blocks.values() if b.get("type") == "tool_use"]
print("stop_reason:", stop, "| tool calls:", [c.get("name") for c in calls])
ok = bool(calls) and all(c.get("name") in NAMES for c in calls)
if not calls:
    print("!! no tool call; the model answered in prose:", text.strip()[:300])
for where, blob in (("reply", text), ("reasoning", thinking)):
    if "mcp_" in blob:
        ok = False
        i = blob.index("mcp_")
        print("!! the gateway's wire naming leaked into the %s: ...%s..." % (where, blob[max(0, i - 160):i + 120].replace("\n", " ")))
for word in ("injection", "not available", "unavailable", "绕行", "不可用"):
    if word in text or word in thinking:
        print("?? model mentions %r: check context" % word)
print("RESULT:", "ok" if ok else "BROKEN")
