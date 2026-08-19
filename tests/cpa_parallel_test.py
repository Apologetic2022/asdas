#!/usr/bin/env python3
"""Parallel tool-call + Anthropic /v1/messages protocol tests against the gateway."""
import json
import urllib.request
import urllib.error
import sys

HOST = "http://127.0.0.1:8317"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODEL = "grok-4.6"

OPENAI_TOOLS = [
    {"type": "function", "function": {"name": "get_weather", "description": "Get current weather for a city",
     "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}},
    {"type": "function", "function": {"name": "get_time", "description": "Get the current local time for a city",
     "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}},
    {"type": "function", "function": {"name": "get_population", "description": "Get the population of a city",
     "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}},
]
CLAUDE_TOOLS = [
    {"name": "get_weather", "description": "Get current weather for a city",
     "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}},
    {"name": "get_time", "description": "Get the current local time for a city",
     "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}},
    {"name": "get_population", "description": "Get the population of a city",
     "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}},
]

PARALLEL_PROMPT = ("For the city of Tokyo I need the weather, the local time, and the population. "
                   "Call all three tools get_weather, get_time and get_population in parallel "
                   "(all in one single response) before answering.")


def post(path, payload, stream=False, timeout=240, anthropic=False):
    headers = {"Content-Type": "application/json"}
    if anthropic:
        headers["x-api-key"] = KEY
        headers["anthropic-version"] = "2023-06-01"
    else:
        headers["Authorization"] = f"Bearer {KEY}"
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


# ---------- P1: OpenAI parallel tool_calls (non-stream) ----------
st, r = post("/v1/chat/completions", {"model": MODEL, "messages": [{"role": "user", "content": PARALLEL_PROMPT}],
                                      "tools": OPENAI_TOOLS, "tool_choice": "auto"})
tcs = []
if st == 200:
    tcs = r["choices"][0]["message"].get("tool_calls") or []
ids = [t.get("id") for t in tcs]
args_ok = all(isinstance(json.loads(t["function"]["arguments"]), dict) for t in tcs) if tcs else False
show("P1 openai parallel tool_calls (>=2, unique ids, valid args)",
     st == 200 and len(tcs) >= 2 and len(set(ids)) == len(ids) and args_ok,
     f"status={st} n={len(tcs)} names={[t['function']['name'] for t in tcs]} ids_unique={len(set(ids)) == len(ids)}")

# ---------- P2: feed ALL results back at once ----------
if len(tcs) >= 2:
    msgs = [{"role": "user", "content": PARALLEL_PROMPT},
            {"role": "assistant", "content": "", "tool_calls": tcs}]
    data = {"get_weather": {"temp_c": 21, "condition": "cloudy"},
            "get_time": {"time": "18:42"}, "get_population": {"population": 13960000}}
    for t in tcs:
        msgs.append({"role": "tool", "tool_call_id": t["id"], "name": t["function"]["name"],
                     "content": json.dumps(data.get(t["function"]["name"], {}))})
    st2, r2 = post("/v1/chat/completions", {"model": MODEL, "messages": msgs, "tools": OPENAI_TOOLS})
    c2 = r2["choices"][0]["message"].get("content") or "" if st2 == 200 else ""
    hits = sum(x in c2 for x in ("21", "18:42")) + ("13" in c2 or "million" in c2.lower())
    show("P2 all parallel results consumed in one turn", st2 == 200 and bool(c2.strip()) and hits >= 2,
         f"status={st2} content={json.dumps(c2[:140])}")
else:
    show("P2 all parallel results consumed in one turn", False, "skipped: <2 tool calls in P1")

# ---------- P3: OpenAI streaming parallel tool_calls: index sequence ----------
st, raw = post("/v1/chat/completions", {"model": MODEL, "stream": True,
               "messages": [{"role": "user", "content": PARALLEL_PROMPT}], "tools": OPENAI_TOOLS}, stream=True)
sids, sidx, snames, sargs_ok = [], [], [], True
finish = None
for line in raw.splitlines():
    if not line.startswith("data: ") or line[6:].strip() == "[DONE]":
        continue
    try:
        chunk = json.loads(line[6:])
    except json.JSONDecodeError:
        continue
    for ch in chunk.get("choices", []):
        if ch.get("finish_reason"):
            finish = ch["finish_reason"]
        for tc in ch.get("delta", {}).get("tool_calls") or []:
            sidx.append(tc.get("index"))
            sids.append(tc.get("id"))
            snames.append(tc.get("function", {}).get("name"))
            try:
                json.loads(tc.get("function", {}).get("arguments") or "{}")
            except json.JSONDecodeError:
                sargs_ok = False
show("P3 stream parallel tool_calls (indices 0..n-1, unique ids, valid args)",
     st == 200 and len(sids) >= 2 and sidx == list(range(len(sids))) and len(set(sids)) == len(sids) and sargs_ok and finish == "tool_calls",
     f"status={st} n={len(sids)} idx={sidx} names={snames} finish={finish}")

# ---------- P4: Claude /v1/messages parallel tool_use (non-stream) ----------
st, r = post("/v1/messages", {"model": MODEL, "max_tokens": 2048,
             "messages": [{"role": "user", "content": PARALLEL_PROMPT}], "tools": CLAUDE_TOOLS}, anthropic=True)
tus = []
if st == 200 and isinstance(r, dict):
    tus = [b for b in r.get("content", []) if b.get("type") == "tool_use"]
tids = [b.get("id") for b in tus]
inputs_ok = all(isinstance(b.get("input"), dict) for b in tus) if tus else False
show("P4 claude parallel tool_use (>=2, unique ids, dict input, stop=tool_use)",
     st == 200 and len(tus) >= 2 and len(set(tids)) == len(tids) and inputs_ok and r.get("stop_reason") == "tool_use",
     f"status={st} n={len(tus)} names={[b.get('name') for b in tus]} stop={r.get('stop_reason') if isinstance(r, dict) else r[:80]}")
claude_usage = r.get("usage", {}) if isinstance(r, dict) else {}
show("P4b claude tool_use usage sane", claude_usage.get("input_tokens", 0) > 0, f"usage={claude_usage}")

# ---------- P5: Claude multi tool_result feedback in one user msg ----------
if len(tus) >= 2:
    data = {"get_weather": {"temp_c": 21, "condition": "cloudy"},
            "get_time": {"time": "18:42"}, "get_population": {"population": 13960000}}
    result_blocks = [{"type": "tool_result", "tool_use_id": b["id"],
                      "content": json.dumps(data.get(b["name"], {}))} for b in tus]
    msgs = [{"role": "user", "content": PARALLEL_PROMPT},
            {"role": "assistant", "content": r["content"]},
            {"role": "user", "content": result_blocks}]
    st2, r2 = post("/v1/messages", {"model": MODEL, "max_tokens": 2048, "messages": msgs, "tools": CLAUDE_TOOLS}, anthropic=True)
    text2 = "".join(b.get("text", "") for b in r2.get("content", []) if b.get("type") == "text") if st2 == 200 and isinstance(r2, dict) else ""
    hits = sum(x in text2 for x in ("21", "18:42")) + ("13" in text2 or "million" in text2.lower())
    show("P5 claude parallel tool_results consumed", st2 == 200 and bool(text2.strip()) and hits >= 2,
         f"status={st2} text={json.dumps(text2[:140])}")
else:
    show("P5 claude parallel tool_results consumed", False, "skipped: <2 tool_use in P4")

# ---------- P6: Claude streaming tool_use block sequencing ----------
st, raw = post("/v1/messages", {"model": MODEL, "max_tokens": 2048, "stream": True,
               "messages": [{"role": "user", "content": PARALLEL_PROMPT}], "tools": CLAUDE_TOOLS}, stream=True, anthropic=True)
blocks = {}
order_ok = True
stop_reason = None
usage_out = None
open_blocks = set()
for line in raw.splitlines():
    if not line.startswith("data: "):
        continue
    try:
        ev = json.loads(line[6:])
    except json.JSONDecodeError:
        continue
    t = ev.get("type")
    if t == "content_block_start":
        i = ev.get("index")
        if i in open_blocks:
            order_ok = False
        open_blocks.add(i)
        cb = ev.get("content_block", {})
        blocks[i] = {"type": cb.get("type"), "id": cb.get("id"), "name": cb.get("name"), "json": ""}
    elif t == "content_block_delta":
        i = ev.get("index")
        d = ev.get("delta", {})
        if i not in open_blocks:
            order_ok = False
        if d.get("type") == "input_json_delta":
            blocks.setdefault(i, {"json": ""})["json"] = blocks[i].get("json", "") + d.get("partial_json", "")
    elif t == "content_block_stop":
        i = ev.get("index")
        if i not in open_blocks:
            order_ok = False
        open_blocks.discard(i)
    elif t == "message_delta":
        stop_reason = ev.get("delta", {}).get("stop_reason") or stop_reason
        if ev.get("usage"):
            usage_out = ev["usage"]
tool_blocks = [b for b in blocks.values() if b.get("type") == "tool_use"]
tjson_ok = True
for b in tool_blocks:
    try:
        parsed = json.loads(b["json"]) if b["json"] else {}
        if not isinstance(parsed, dict):
            tjson_ok = False
    except json.JSONDecodeError:
        tjson_ok = False
tids = [b.get("id") for b in tool_blocks]
show("P6 claude stream tool_use blocks (>=2, ordered, valid input_json)",
     st == 200 and len(tool_blocks) >= 2 and order_ok and tjson_ok and len(set(tids)) == len(tids) and stop_reason == "tool_use" and not open_blocks,
     f"status={st} n={len(tool_blocks)} names={[b.get('name') for b in tool_blocks]} stop={stop_reason} order_ok={order_ok} usage={usage_out}")

# ---------- P7: big schema (Cursor-like Task/Shell tools) survives ----------
big_tools = [{"type": "function", "function": {
    "name": "Task",
    "description": "Launch a subagent to autonomously handle a complex multi-step task. " * 20,
    "parameters": {"type": "object", "properties": {
        "description": {"type": "string", "description": "Short description. " * 30},
        "prompt": {"type": "string", "description": "Full prompt for the agent. " * 30},
        "subagent_type": {"type": "string", "enum": ["explore", "code", "search", "plan", "debug"],
                          "description": "The type of agent. " * 20},
        "model": {"type": "string", "description": "Model override. " * 10},
        "settings": {"type": "object", "properties": {
            "max_turns": {"type": "integer"}, "timeout_s": {"type": "integer"},
            "allow_write": {"type": "boolean"}, "workdir": {"type": "string"}}},
    }, "required": ["description", "prompt", "subagent_type"]}}}] + OPENAI_TOOLS
st, r = post("/v1/chat/completions", {"model": MODEL, "messages": [
    {"role": "user", "content": "Use the Task tool to launch an explore subagent that scans the home directory for projects. Call the Task tool now."}],
    "tools": big_tools, "tool_choice": "required"})
tcs7 = r["choices"][0]["message"].get("tool_calls") or [] if st == 200 else []
task_calls = [t for t in tcs7 if t["function"]["name"] == "Task"]
t7ok = False
t7detail = f"status={st} tools={[t['function']['name'] for t in tcs7]}"
if task_calls:
    args = json.loads(task_calls[0]["function"]["arguments"])
    t7ok = all(k in args for k in ("description", "prompt", "subagent_type"))
    t7detail += f" args_keys={sorted(args.keys())}"
show("P7 big Task schema -> valid Task call with required args", t7ok, t7detail)

# ---------- P8: subagent model name claude-4.6-sonnet (Cursor Task probe) ----------
st, r = post("/v1/messages?beta=true", {"model": "claude-4.6-sonnet", "max_tokens": 50,
             "messages": [{"role": "user", "content": "Calculate and respond with ONLY the number, nothing else.\n\nQ: 39 - 28 = ?\nA:"}]}, anthropic=True)
txt = ""
if st == 200 and isinstance(r, dict):
    txt = "".join(b.get("text", "") for b in r.get("content", []) if b.get("type") == "text")
show("P8 claude-4.6-sonnet alias resolves (Cursor Task probe)", st == 200 and "11" in txt,
     f"status={st} resp={json.dumps(txt[:60]) if txt else str(r)[:120]}")

print()
print(f"{sum(results)}/{len(results)} checks passed")
sys.exit(0 if all(results) else 1)
