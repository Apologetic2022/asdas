#!/usr/bin/env python3
"""Replay ZCode/Claude-Code-shaped traffic against the CPA gateway and strictly
validate the Anthropic SSE / JSON responses for the failure modes seen in the
field: empty tool_use input, missing stop markers, broken parallel tool calls,
empty continuation segments, and hangs."""

import json
import sys
import time
import uuid
import threading
import http.client

HOST = "127.0.0.1"
PORT = 80
API_KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"

SYSTEM_PROMPT = [
    {"type": "text", "text": "You are ZCode, an agentic coding CLI. " + ("You operate in a workspace and use tools to explore and edit code. " * 60)},
    {"type": "text", "text": "IMPORTANT: prefer tools over prose. When asked to inspect the workspace, call tools immediately. " + ("Follow the user's instructions precisely. " * 40)},
]

TOOLS = [
    {
        "name": "Task",
        "description": "Launch a subagent to handle complex multi-step tasks autonomously. Provide a short description and a detailed prompt. The subagent runs with its own context and returns a final report.",
        "input_schema": {
            "type": "object",
            "properties": {
                "description": {"type": "string", "description": "A short (3-5 word) description of the task"},
                "prompt": {"type": "string", "description": "The full task prompt for the subagent"},
                "subagent_type": {"type": "string", "description": "The type of subagent", "enum": ["explore", "general-purpose", "code-reviewer"]},
            },
            "required": ["description", "prompt", "subagent_type"],
        },
    },
    {
        "name": "Bash",
        "description": "Executes a given bash command in a persistent shell session with optional timeout. Before executing, verify parent directories exist. Always quote paths with spaces.",
        "input_schema": {
            "type": "object",
            "properties": {
                "command": {"type": "string", "description": "The command to execute"},
                "timeout": {"type": "number", "description": "Optional timeout in milliseconds (max 600000)"},
                "description": {"type": "string", "description": "Clear, concise description of what this command does in 5-10 words"},
            },
            "required": ["command"],
        },
    },
    {
        "name": "Glob",
        "description": "Fast file pattern matching tool that works with any codebase size. Supports glob patterns like **/*.js. Returns matching file paths sorted by modification time.",
        "input_schema": {
            "type": "object",
            "properties": {
                "pattern": {"type": "string", "description": "The glob pattern to match files against"},
                "path": {"type": "string", "description": "The directory to search in. Defaults to the current working directory."},
            },
            "required": ["pattern"],
        },
    },
    {
        "name": "Grep",
        "description": "A powerful search tool built on ripgrep. Supports full regex syntax. Filter files with glob or type parameters.",
        "input_schema": {
            "type": "object",
            "properties": {
                "pattern": {"type": "string"},
                "path": {"type": "string"},
                "glob": {"type": "string"},
                "output_mode": {"type": "string", "enum": ["content", "files_with_matches", "count"]},
                "-i": {"type": "boolean"},
                "multiline": {"type": "boolean"},
            },
            "required": ["pattern"],
        },
    },
    {
        "name": "Read",
        "description": "Reads a file from the local filesystem. Lines are numbered starting at 1. Supports text, images, and PDFs.",
        "input_schema": {
            "type": "object",
            "properties": {
                "file_path": {"type": "string", "description": "The absolute path to the file to read"},
                "offset": {"type": "number"},
                "limit": {"type": "number"},
            },
            "required": ["file_path"],
        },
    },
    {
        "name": "Edit",
        "description": "Performs exact string replacements in files. The edit will fail if old_string is not unique unless replace_all is set.",
        "input_schema": {
            "type": "object",
            "properties": {
                "file_path": {"type": "string"},
                "old_string": {"type": "string"},
                "new_string": {"type": "string"},
                "replace_all": {"type": "boolean"},
            },
            "required": ["file_path", "old_string", "new_string"],
        },
    },
    {
        "name": "Write",
        "description": "Writes a file to the local filesystem, overwriting if it exists.",
        "input_schema": {
            "type": "object",
            "properties": {
                "file_path": {"type": "string"},
                "content": {"type": "string"},
            },
            "required": ["file_path", "content"],
        },
    },
    {
        "name": "TodoWrite",
        "description": "Create and manage a structured task list for the current session.",
        "input_schema": {
            "type": "object",
            "properties": {
                "todos": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "content": {"type": "string"},
                            "status": {"type": "string", "enum": ["pending", "in_progress", "completed"]},
                            "id": {"type": "string"},
                        },
                        "required": ["content", "status", "id"],
                    },
                }
            },
            "required": ["todos"],
        },
    },
]

FAILURES = []


def fail(name, msg):
    FAILURES.append((name, msg))
    print(f"  [FAIL] {name}: {msg}")


def ok(name, msg=""):
    print(f"  [ok] {name} {msg}")


def post(path, body, stream, timeout=180):
    conn = http.client.HTTPConnection(HOST, PORT, timeout=timeout)
    payload = json.dumps(body)
    headers = {
        "Content-Type": "application/json",
        "x-api-key": API_KEY,
        "anthropic-version": "2023-06-01",
        "accept": "text/event-stream" if stream else "application/json",
        "User-Agent": "ZCode/3.6.5 ai-sdk/provider-utils/4.0.27 runtime/node.js/24",
    }
    conn.request("POST", path, payload, headers)
    resp = conn.getresponse()
    raw = resp.read()
    conn.close()
    return resp.status, dict(resp.getheaders()), raw


class SSE:
    """Parses and strictly validates an Anthropic SSE stream."""

    def __init__(self, raw: bytes, name: str):
        self.name = name
        self.events = []  # (event_name, data_dict)
        self.parse(raw)

    def parse(self, raw):
        cur_event = None
        for line in raw.decode("utf-8", "replace").split("\n"):
            line = line.rstrip("\r")
            if line.startswith("event:"):
                cur_event = line[6:].strip()
            elif line.startswith("data:"):
                data = line[5:].strip()
                if not data:
                    continue
                try:
                    self.events.append((cur_event, json.loads(data)))
                except json.JSONDecodeError:
                    fail(self.name, f"unparseable SSE data line: {data[:200]}")

    def validate(self):
        n = self.name
        kinds = [e for e, _ in self.events]
        if "message_start" not in kinds:
            fail(n, "missing message_start")
        if "message_stop" not in kinds:
            fail(n, f"missing message_stop (events={kinds})")
        if "message_delta" not in kinds:
            fail(n, "missing message_delta")
        # block bookkeeping
        open_blocks = {}
        blocks = {}
        stop_reason = None
        usage = None
        for ev, d in self.events:
            t = d.get("type")
            if ev and t and ev != t:
                fail(n, f"event name {ev} != data type {t}")
            if t == "content_block_start":
                idx = d.get("index")
                if idx in open_blocks:
                    fail(n, f"content_block_start for already-open index {idx}")
                blk = d.get("content_block", {})
                open_blocks[idx] = blk
                blocks[idx] = {"type": blk.get("type"), "id": blk.get("id"), "name": blk.get("name"), "json": "", "text": ""}
                if blk.get("type") == "tool_use":
                    if not blk.get("id"):
                        fail(n, f"tool_use block index {idx} missing id")
                    if not blk.get("name"):
                        fail(n, f"tool_use block index {idx} missing name")
            elif t == "content_block_delta":
                idx = d.get("index")
                if idx not in open_blocks:
                    fail(n, f"content_block_delta for unopened index {idx}")
                    continue
                delta = d.get("delta", {})
                dt = delta.get("type")
                if dt == "input_json_delta":
                    blocks[idx]["json"] += delta.get("partial_json", "")
                elif dt == "text_delta":
                    blocks[idx]["text"] += delta.get("text", "")
                elif dt == "thinking_delta":
                    blocks[idx]["text"] += delta.get("thinking", "")
            elif t == "content_block_stop":
                idx = d.get("index")
                if idx not in open_blocks:
                    fail(n, f"content_block_stop for unopened index {idx}")
                else:
                    del open_blocks[idx]
            elif t == "message_delta":
                stop_reason = d.get("delta", {}).get("stop_reason")
                usage = d.get("usage")
        if open_blocks:
            fail(n, f"blocks never stopped: {list(open_blocks)}")
        tool_uses = []
        for idx, b in sorted(blocks.items()):
            if b["type"] == "tool_use":
                inp = None
                if b["json"]:
                    try:
                        inp = json.loads(b["json"])
                    except json.JSONDecodeError:
                        fail(n, f"tool_use idx {idx} input_json not parseable: {b['json'][:200]}")
                else:
                    inp = {}
                if not isinstance(inp, dict):
                    fail(n, f"tool_use idx {idx} input is not an object: {type(inp)}")
                tool_uses.append({"id": b["id"], "name": b["name"], "input": inp, "index": idx})
        return {"stop_reason": stop_reason, "usage": usage, "tool_uses": tool_uses, "blocks": blocks}


def nonstream_summary(raw, name):
    try:
        d = json.loads(raw)
    except json.JSONDecodeError:
        fail(name, f"non-stream body not JSON: {raw[:300]}")
        return None
    tool_uses = [c for c in d.get("content", []) if c.get("type") == "tool_use"]
    texts = [c for c in d.get("content", []) if c.get("type") == "text"]
    return {"stop_reason": d.get("stop_reason"), "usage": d.get("usage"), "tool_uses": tool_uses,
            "texts": texts, "content": d.get("content", []), "raw": d}


def rewrite_id(orig):
    """ZCode-style client id rewrite seen in production logs."""
    return f"call-{uuid.uuid4()}-13_{orig}"


def base_body(model, messages, stream, max_tokens=4000):
    return {
        "model": model,
        "max_tokens": max_tokens,
        "stream": stream,
        "system": SYSTEM_PROMPT,
        "tools": TOOLS,
        "messages": messages,
        "metadata": {"user_id": json.dumps({"device_id": "test-device", "session_id": str(uuid.uuid4())})},
    }


def t1_stream_task(model):
    name = f"T1-stream-task-{model}"
    print(f"== {name}")
    msgs = [{"role": "user", "content": [{"type": "text", "text": "Use the Task tool RIGHT NOW to launch an explore subagent that lists the workspace contents. Do not answer in prose."}]}]
    st, hdrs, raw = post("/v1/messages", base_body(model, msgs, True), True)
    if st != 200:
        fail(name, f"status {st}: {raw[:300]}")
        return None
    r = SSE(raw, name).validate()
    if r["stop_reason"] != "tool_use":
        fail(name, f"stop_reason={r['stop_reason']} expected tool_use (tool_uses={len(r['tool_uses'])})")
    if not r["tool_uses"]:
        fail(name, "no tool_use blocks emitted")
        return None
    tu = r["tool_uses"][0]
    if tu["name"] != "Task":
        ok(name, f"(model chose {tu['name']} instead of Task; acceptable)")
    if not tu["input"]:
        fail(name, f"tool_use input is EMPTY for {tu['name']} — this is the No exec result condition")
    else:
        ok(name, f"tool={tu['name']} input_keys={sorted(tu['input'])} id={tu['id'][:40]}")
    if not r["usage"] or (r["usage"].get("input_tokens", 0) == 0 and r["usage"].get("output_tokens", 0) == 0):
        fail(name, f"usage empty: {r['usage']}")
    return tu


def t2_nonstream_task(model):
    name = f"T2-nonstream-task-{model}"
    print(f"== {name}")
    msgs = [{"role": "user", "content": [{"type": "text", "text": "Use the Task tool RIGHT NOW to launch an explore subagent that lists the workspace contents. Do not answer in prose."}]}]
    st, hdrs, raw = post("/v1/messages", base_body(model, msgs, False), False)
    if st != 200:
        fail(name, f"status {st}: {raw[:300]}")
        return None
    r = nonstream_summary(raw, name)
    if not r:
        return None
    if not r["tool_uses"]:
        fail(name, f"no tool_use in content; stop_reason={r['stop_reason']} content={json.dumps(r['content'])[:300]}")
        return None
    tu = r["tool_uses"][0]
    if not tu.get("input"):
        fail(name, f"tool_use input EMPTY: {json.dumps(tu)[:300]}")
    else:
        ok(name, f"tool={tu['name']} input_keys={sorted(tu['input'])} stop={r['stop_reason']}")
    if r["stop_reason"] != "tool_use":
        fail(name, f"stop_reason={r['stop_reason']} expected tool_use")
    return tu


def t3_multiturn_rewritten_ids(model, stream):
    mode = "stream" if stream else "nonstream"
    name = f"T3-multiturn-rewrittenids-{mode}-{model}"
    print(f"== {name}")
    user1 = {"role": "user", "content": [{"type": "text", "text": "List the top-level entries of the workspace. First call Bash with command 'ls -la /workspace'. After you get the result, summarize what you saw in one short sentence."}]}
    st, _, raw = post("/v1/messages", base_body(model, [user1], stream), stream)
    if st != 200:
        fail(name, f"turn1 status {st}: {raw[:300]}")
        return
    if stream:
        r = SSE(raw, name + "-turn1").validate()
        tus = r["tool_uses"]
    else:
        r = nonstream_summary(raw, name + "-turn1")
        tus = r["tool_uses"] if r else []
    if not tus:
        fail(name, "turn1 produced no tool_use")
        return
    tu = tus[0]
    ok(name, f"turn1 tool={tu['name']} id={tu['id'][:40]}")
    # Client rewrites the id (as ZCode does) both in assistant history and tool_result
    rid = rewrite_id(tu["id"])
    asst = {"role": "assistant", "content": [
        {"type": "tool_use", "id": rid, "name": tu["name"], "input": tu["input"]},
    ]}
    toolres = {"role": "user", "content": [
        {"type": "tool_result", "tool_use_id": rid, "content": [{"type": "text", "text": "total 24\ndrwxr-xr-x 5 root root 4096 Aug 19 07:00 .\ndrwxr-xr-x 1 root root 4096 Aug 19 07:00 ..\n-rw-r--r-- 1 root root 120 Aug 19 07:00 MANIFEST.txt\n-rw-r--r-- 1 root root 3210 Aug 19 07:00 LOCAL_DEPLOYMENT.md\ndrwxr-xr-x 8 root root 4096 Aug 19 07:00 services\n"}]},
    ]}
    msgs2 = [user1, asst, toolres]
    st, _, raw = post("/v1/messages", base_body(model, msgs2, stream), stream)
    if st != 200:
        fail(name, f"turn2 status {st}: {raw[:300]}")
        return
    if stream:
        r2 = SSE(raw, name + "-turn2").validate()
        text = "".join(b["text"] for b in r2["blocks"].values() if b["type"] in ("text", "thinking"))
        n_tools = len(r2["tool_uses"])
        stop = r2["stop_reason"]
    else:
        r2 = nonstream_summary(raw, name + "-turn2")
        if not r2:
            return
        text = "".join(t.get("text", "") for t in r2["texts"])
        n_tools = len(r2["tool_uses"])
        stop = r2["stop_reason"]
    if not text.strip() and n_tools == 0:
        fail(name, f"turn2 EMPTY response (no text, no tool_use) stop={stop} raw={raw[:400]}")
    else:
        ok(name, f"turn2 text_len={len(text)} tools={n_tools} stop={stop}")


def t4_parallel_calls(model, stream):
    mode = "stream" if stream else "nonstream"
    name = f"T4-parallel-{mode}-{model}"
    print(f"== {name}")
    msgs = [{"role": "user", "content": [{"type": "text", "text": "In a SINGLE response, make exactly two parallel Read tool calls: one for /workspace/MANIFEST.txt and one for /workspace/LOCAL_DEPLOYMENT.md. Issue both tool calls together before any results come back. Do not answer in prose."}]}]
    st, _, raw = post("/v1/messages", base_body(model, msgs, stream), stream)
    if st != 200:
        fail(name, f"status {st}: {raw[:300]}")
        return
    if stream:
        r = SSE(raw, name).validate()
        tus = r["tool_uses"]
    else:
        r = nonstream_summary(raw, name)
        tus = r["tool_uses"] if r else []
    if len(tus) == 0:
        fail(name, "no tool_use blocks at all")
        return
    empty = [t for t in tus if not t.get("input")]
    if empty:
        fail(name, f"{len(empty)}/{len(tus)} tool_use blocks have EMPTY input")
    ids = [t.get("id") for t in tus]
    if len(set(ids)) != len(ids):
        fail(name, f"duplicate tool ids: {ids}")
    if len(tus) >= 2:
        ok(name, f"parallel={len(tus)} inputs={[sorted(t['input']) for t in tus]}")
    else:
        ok(name, f"(model made {len(tus)} call(s) instead of 2; checking follow-up round)")
        # Continue the loop: return the result and see if the second call comes
        tu = tus[0]
        rid = rewrite_id(tu["id"])
        msgs2 = msgs + [
            {"role": "assistant", "content": [{"type": "tool_use", "id": rid, "name": tu["name"], "input": tu["input"]}]},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": rid, "content": [{"type": "text", "text": "file-1 contents: deploy manifest v3"}]}]},
        ]
        st, _, raw = post("/v1/messages", base_body(model, msgs2, stream), stream)
        if st != 200:
            fail(name, f"follow-up status {st}")
            return
        if stream:
            r2 = SSE(raw, name + "-round2").validate()
            n2 = len(r2["tool_uses"])
            text2 = "".join(b["text"] for b in r2["blocks"].values() if b["type"] in ("text", "thinking"))
        else:
            r2 = nonstream_summary(raw, name + "-round2")
            n2 = len(r2["tool_uses"]) if r2 else 0
            text2 = "".join(t.get("text", "") for t in (r2["texts"] if r2 else []))
        if n2 == 0 and not text2.strip():
            fail(name, "round2 EMPTY response")
        else:
            ok(name, f"round2 tools={n2} text_len={len(text2)}")


def t5_concurrent_subagents(model):
    name = f"T5-concurrent-subagents-{model}"
    print(f"== {name}")
    results = {}

    def run(i):
        msgs = [{"role": "user", "content": [{"type": "text", "text": f"You are subagent #{i}. Call Glob with pattern '**/*.md' now, then after the result summarize in one word."}]}]
        try:
            st, _, raw = post("/v1/messages", base_body(model, msgs, True), True)
            if st != 200:
                results[i] = f"status {st}"
                return
            r = SSE(raw, f"{name}-{i}").validate()
            if not r["tool_uses"] and not any(b["text"].strip() for b in r["blocks"].values()):
                results[i] = "EMPTY response"
            elif r["tool_uses"] and not r["tool_uses"][0]["input"]:
                results[i] = "EMPTY tool input"
            else:
                results[i] = "ok"
        except Exception as e:
            results[i] = f"exception {e}"

    threads = [threading.Thread(target=run, args=(i,)) for i in range(3)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=200)
    bad = {i: v for i, v in results.items() if v != "ok"}
    if len(results) < 3:
        fail(name, f"only {len(results)}/3 finished (hang?)")
    if bad:
        fail(name, f"failures: {bad}")
    else:
        ok(name, f"all 3 concurrent sessions ok")


def main():
    t0 = time.time()
    models = sys.argv[1:] or ["grok-4.6"]
    for model in models:
        t1_stream_task(model)
        t2_nonstream_task(model)
        t3_multiturn_rewritten_ids(model, True)
        t3_multiturn_rewritten_ids(model, False)
        t4_parallel_calls(model, True)
        t4_parallel_calls(model, False)
        t5_concurrent_subagents(model)
    print(f"\n== done in {time.time()-t0:.1f}s; {len(FAILURES)} failure(s)")
    for n, m in FAILURES:
        print(f"  FAIL {n}: {m}")
    sys.exit(1 if FAILURES else 0)


if __name__ == "__main__":
    main()
