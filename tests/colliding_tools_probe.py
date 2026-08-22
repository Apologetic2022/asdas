#!/usr/bin/env python3
"""Does the model reach for a built-in when an MCP tool shadows its name?

A real client registers tools called Bash / Write / Read, which are also the
names of Cursor's own workspace built-ins. The built-ins are not backed here, so
every call to one is a wasted segment and a confusing refusal in the user's
transcript. This probe counts how many turns it takes the model to use the MCP
tools, and fails if it ever calls a built-in.
"""
import json
import re
import sys
import urllib.request

HOST = "http://127.0.0.1:8317"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODEL = sys.argv[1] if len(sys.argv) > 1 else "claude-sonnet-5"

TOOLS = [
    {"type": "function", "function": {"name": "Bash", "description": "Run a shell command.",
     "parameters": {"type": "object", "properties": {"command": {"type": "string"}},
                    "required": ["command"]}}},
    {"type": "function", "function": {"name": "Write", "description": "Write a file.",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"},
                                                     "content": {"type": "string"}},
                    "required": ["path", "content"]}}},
    {"type": "function", "function": {"name": "Read", "description": "Read a file.",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                    "required": ["path"]}}},
]

BUILTIN_REFUSAL = re.compile(r"not available in this environment", re.I)


def post(messages):
    body = {"model": MODEL, "messages": messages, "tools": TOOLS, "max_tokens": 512}
    req = urllib.request.Request(HOST + "/v1/chat/completions", json.dumps(body).encode(),
                                 {"Content-Type": "application/json",
                                  "Authorization": f"Bearer {KEY}"})
    with urllib.request.urlopen(req, timeout=300) as r:
        return json.loads(r.read().decode())


def main():
    messages = [{"role": "user", "content":
                 "Run the shell command `date -u +%Y`, then write the output to "
                 "/tmp/year.txt, then read it back. Use your tools."}]
    used, refusals, ok = [], 0, True
    for turn in range(5):
        choice = post(messages)["choices"][0]
        msg = choice["message"]
        calls = msg.get("tool_calls") or []
        text = msg.get("content") or ""
        if BUILTIN_REFUSAL.search(text):
            refusals += 1
            print(f"  turn{turn}: model reported a built-in refusal in its answer")
            ok = False
        if not calls:
            print(f"  turn{turn}: finish={choice.get('finish_reason')} text={text.strip()[:90]!r}")
            break
        messages.append({"role": "assistant", "content": msg.get("content"),
                         "tool_calls": calls})
        for c in calls:
            name = c["function"]["name"]
            used.append(name)
            print(f"  turn{turn}: called {name} {c['function']['arguments'][:60]}")
            result = {"Bash": "2026\n", "Write": "written", "Read": "2026\n"}.get(name, "ok")
            messages.append({"role": "tool", "tool_call_id": c["id"], "content": result})

    print(f"\ntools used: {used}")
    if not used:
        print("FAIL: the model never called a tool at all")
        return 1
    if refusals:
        print("FAIL: a built-in refusal leaked into the conversation")
        return 1
    print("PASS: the model used the registered tools without touching a built-in")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
