#!/usr/bin/env python3
"""Replicate ZCode/Task subagent traffic: Anthropic protocol + auto-switch + foreign tool ids."""
import json
import urllib.request
import urllib.error
import sys

HOST = "http://127.0.0.1:8317"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"

TASK_TOOLS = [
    {"name": "Shell", "description": "Executes a shell command and returns stdout/stderr.",
     "input_schema": {"type": "object", "properties": {
         "command": {"type": "string", "description": "The command to execute"},
         "working_directory": {"type": "string"}}, "required": ["command"]}},
    {"name": "Glob", "description": "Find files matching a glob pattern.",
     "input_schema": {"type": "object", "properties": {
         "glob_pattern": {"type": "string"}, "target_directory": {"type": "string"}},
         "required": ["glob_pattern"]}},
    {"name": "Read", "description": "Read a file from the filesystem.",
     "input_schema": {"type": "object", "properties": {
         "path": {"type": "string"}, "offset": {"type": "integer"}, "limit": {"type": "integer"}},
         "required": ["path"]}},
]

SYSTEM = ("You are an autonomous explore subagent. You scan directories and report findings. "
          "Use the provided tools to inspect the filesystem. Be thorough but concise. " * 8)

SCAN_PROMPT = ("Scan the workspace root directory and list top-level projects. "
               "Start by calling the Shell tool with an ls command, or Glob for project markers.")


def post(path, payload, stream=False, timeout=240):
    headers = {"Content-Type": "application/json", "x-api-key": KEY, "anthropic-version": "2023-06-01"}
    req = urllib.request.Request(HOST + path, data=json.dumps(payload).encode(), headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read().decode()
            return resp.status, body if stream else json.loads(body)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


results = []


def show(name, ok, detail):
    print(f"[{'PASS' if ok else 'FAIL'}] {name}: {detail}")
    results.append(ok)
    return ok


def claude_events(raw):
    evs = []
    for line in raw.splitlines():
        if line.startswith("data: "):
            try:
                evs.append(json.loads(line[6:]))
            except json.JSONDecodeError:
                pass
    return evs


# Z1: non-stream claude-4.6-sonnet (auto-switch) with Task-style tools
st, r = post("/v1/messages", {"model": "claude-4.6-sonnet", "max_tokens": 4096, "system": SYSTEM,
             "messages": [{"role": "user", "content": SCAN_PROMPT}], "tools": TASK_TOOLS})
tus = [b for b in r.get("content", []) if b.get("type") == "tool_use"] if st == 200 and isinstance(r, dict) else []
nonempty = st == 200 and isinstance(r, dict) and len(r.get("content", [])) > 0
show("Z1 auto-switch non-stream w/ tools returns content", nonempty and bool(tus),
     f"status={st} blocks={[b.get('type') for b in r.get('content', [])] if isinstance(r, dict) else str(r)[:100]} stop={r.get('stop_reason') if isinstance(r, dict) else '?'}")

# Z2: STREAM claude-4.6-sonnet with tools -> full Claude event sequence (KEY untested path)
st, raw = post("/v1/messages", {"model": "claude-4.6-sonnet", "max_tokens": 4096, "stream": True,
               "system": SYSTEM, "messages": [{"role": "user", "content": SCAN_PROMPT}],
               "tools": TASK_TOOLS}, stream=True)
evs = claude_events(raw)
types = [e.get("type") for e in evs]
tool_starts = [e for e in evs if e.get("type") == "content_block_start" and e.get("content_block", {}).get("type") == "tool_use"]
stop_reason = next((e.get("delta", {}).get("stop_reason") for e in evs if e.get("type") == "message_delta" and e.get("delta", {}).get("stop_reason")), None)
seq_ok = (types.count("message_start") == 1 and "message_stop" in types
          and types.index("message_start") == 0 and types[-1] == "message_stop")
show("Z2 auto-switch STREAM w/ tools: full event sequence + tool_use",
     st == 200 and seq_ok and bool(tool_starts) and stop_reason == "tool_use",
     f"status={st} n_events={len(types)} tool_use_blocks={len(tool_starts)} stop={stop_reason} seq_head={types[:3]} seq_tail={types[-3:]}")

# Z3: multi-turn with FOREIGN (client-rewritten) tool ids, non-stream, grok-4.6
foreign_id = "call-8c94efa7-7abd-4682-bd00-818f1b157cca-5_fc_zcode-test-manual_0"
msgs = [
    {"role": "user", "content": SCAN_PROMPT},
    {"role": "assistant", "content": [
        {"type": "text", "text": "I'll scan the workspace now."},
        {"type": "tool_use", "id": foreign_id, "name": "Shell",
         "input": {"command": "ls -la /workspace"}}]},
    {"role": "user", "content": [
        {"type": "tool_result", "tool_use_id": foreign_id,
         "content": "total 12\ndrwxr-xr-x app\ndrwxr-xr-x web\n-rw-r--r-- README.md\n-rw-r--r-- go.mod"}]},
]
st, r = post("/v1/messages", {"model": "grok-4.6", "max_tokens": 4096, "system": SYSTEM,
             "messages": msgs, "tools": TASK_TOOLS})
blocks = r.get("content", []) if st == 200 and isinstance(r, dict) else []
text = "".join(b.get("text", "") for b in blocks if b.get("type") == "text")
tus3 = [b for b in blocks if b.get("type") == "tool_use"]
ok3 = st == 200 and len(blocks) > 0 and (bool(text.strip()) or bool(tus3))
show("Z3 foreign tool id -> rebuild returns real content (non-stream)", ok3,
     f"status={st} blocks={[b.get('type') for b in blocks]} text={json.dumps(text[:100])} tool_use={[b.get('name') for b in tus3]}")

# Z4: same as Z3 but STREAM
st, raw = post("/v1/messages", {"model": "grok-4.6", "max_tokens": 4096, "stream": True,
               "system": SYSTEM, "messages": msgs, "tools": TASK_TOOLS}, stream=True)
evs = claude_events(raw)
types = [e.get("type") for e in evs]
text4 = "".join(e.get("delta", {}).get("text", "") for e in evs
                if e.get("type") == "content_block_delta" and e.get("delta", {}).get("type") == "text_delta")
tool_starts4 = [e for e in evs if e.get("type") == "content_block_start" and e.get("content_block", {}).get("type") == "tool_use"]
seq_ok4 = types.count("message_start") == 1 and "message_stop" in types
show("Z4 foreign tool id -> rebuild returns real content (stream)",
     st == 200 and seq_ok4 and (bool(text4.strip()) or bool(tool_starts4)),
     f"status={st} n_events={len(types)} text={json.dumps(text4[:100])} tool_use_blocks={len(tool_starts4)}")

# Z5: exact ZCode probe replication
st, r = post("/v1/messages?beta=true", {"model": "claude-4.6-sonnet", "max_tokens": 50,
             "messages": [{"role": "user", "content": "Calculate and respond with ONLY the number, nothing else.\n\nQ: 39 - 28 = ?\nA:"}]})
txt = "".join(b.get("text", "") for b in r.get("content", []) if b.get("type") == "text") if st == 200 and isinstance(r, dict) else ""
show("Z5 ZCode probe (claude-4.6-sonnet, non-stream)", st == 200 and "11" in txt,
     f"status={st} text={json.dumps(txt[:50]) if txt else str(r)[:120]}")

# Z6: continue a REAL tool round-trip with OUR ids through claude-4.6-sonnet (auto-switch multi-turn)
st, r = post("/v1/messages", {"model": "claude-4.6-sonnet", "max_tokens": 4096, "system": SYSTEM,
             "messages": [{"role": "user", "content": SCAN_PROMPT}], "tools": TASK_TOOLS})
tus6 = [b for b in r.get("content", []) if b.get("type") == "tool_use"] if st == 200 and isinstance(r, dict) else []
if tus6:
    msgs6 = [{"role": "user", "content": SCAN_PROMPT},
             {"role": "assistant", "content": r["content"]},
             {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tus6[0]["id"],
                                           "content": "app/\nweb/\nREADME.md\ngo.mod"}]}]
    st2, r2 = post("/v1/messages", {"model": "claude-4.6-sonnet", "max_tokens": 4096, "system": SYSTEM,
                   "messages": msgs6, "tools": TASK_TOOLS})
    blocks2 = r2.get("content", []) if st2 == 200 and isinstance(r2, dict) else []
    text2 = "".join(b.get("text", "") for b in blocks2 if b.get("type") == "text")
    tus62 = [b for b in blocks2 if b.get("type") == "tool_use"]
    show("Z6 auto-switch multi-turn tool round-trip", st2 == 200 and len(blocks2) > 0 and (bool(text2.strip()) or bool(tus62)),
         f"status={st2} blocks={[b.get('type') for b in blocks2]} text={json.dumps(text2[:100])}")
else:
    show("Z6 auto-switch multi-turn tool round-trip", False, f"skipped: no tool_use in first turn (status={st})")

print()
print(f"{sum(results)}/{len(results)} checks passed")
sys.exit(0 if all(results) else 1)
