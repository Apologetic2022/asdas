#!/usr/bin/env python3
"""Reproduce the executor's char/4 prompt estimate for a logged request.

Prints the estimate next to the usage the gateway actually reported, so a
reported count can be identified as an estimate rather than an upstream number.
"""
import json
import re
import sys

BODY_RE = re.compile(r"=== REQUEST BODY ===\n(.*?)\n\n\n", re.S)
USAGE_RE = re.compile(r'"usage"\s*:\s*(\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\})')


def anthropic_to_chat(body):
    """Mirror the anthropic->openai translation closely enough for a char count."""
    msgs = []
    system = body.get("system")
    if isinstance(system, str):
        msgs.append(("system", system, []))
    elif isinstance(system, list):
        text = "".join(b.get("text", "") for b in system if isinstance(b, dict))
        if text:
            msgs.append(("system", text, []))
    for m in body.get("messages") or []:
        role = m.get("role", "user")
        content = m.get("content")
        calls = []
        if isinstance(content, str):
            msgs.append((role, content, []))
            continue
        text_parts = []
        for block in content or []:
            btype = block.get("type")
            if btype == "text":
                text_parts.append(block.get("text", ""))
            elif btype == "thinking":
                text_parts.append(block.get("thinking", ""))
            elif btype == "tool_use":
                calls.append((block.get("name", ""), block.get("input", {})))
            elif btype == "tool_result":
                c = block.get("content")
                if isinstance(c, str):
                    text_parts.append(c)
                else:
                    for cb in c or []:
                        if isinstance(cb, dict) and cb.get("type") == "text":
                            text_parts.append(cb.get("text", ""))
        msgs.append((role, "".join(text_parts), calls))
    return msgs


def estimate(msgs, tools):
    chars = 0
    for role, content, calls in msgs:
        chars += len(role) + len(content) + 8
        for name, args in calls:
            chars += len(name) + 16 + len(json.dumps(args, separators=(",", ":")))
    for t in tools:
        chars += len(t.get("name", "")) + len(t.get("description", "")) + 16
        chars += len(json.dumps(t.get("input_schema", t.get("parameters", {})), separators=(",", ":")))
    return chars // 4, chars


for path in sys.argv[1:]:
    text = open(path, errors="replace").read()
    m = BODY_RE.search(text)
    if not m:
        print(f"{path}: no body")
        continue
    body = json.loads(m.group(1))
    msgs = anthropic_to_chat(body)
    tools = body.get("tools") or []
    est, chars = estimate(msgs, tools)
    reported = None
    for mm in USAGE_RE.finditer(text):
        try:
            u = json.loads(mm.group(1))
        except ValueError:
            continue
        if u.get("input_tokens"):
            reported = u
    print(f"{path.split('/')[-1]}")
    print(f"  chars={chars} estimate={est} reported={json.dumps(reported)}")
    if reported and reported.get("input_tokens"):
        print(f"  estimate/reported = {est / reported['input_tokens']:.4f}")
