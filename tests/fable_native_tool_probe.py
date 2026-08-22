#!/usr/bin/env python3
"""Verify fable emits native Anthropic tool_use, including after session loss.

Set CPA_RESTART_BETWEEN=1 on the gateway host to restart the service after the
first tool call. The second request must then resume from the persisted Cursor
checkpoint using native ConversationStep/McpToolResult protobufs.
"""

import json
import os
import subprocess
import sys
import time
import urllib.request


BASE = os.environ.get("CPA_BASE", "http://127.0.0.1:8317")
KEY = os.environ.get("CPA_KEY", "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a")
MODEL = os.environ.get("CPA_MODEL", "claude-fable-5")
RESTART = os.environ.get("CPA_RESTART_BETWEEN", "").lower() in {"1", "true", "yes"}

TOOLS = [{
    "name": "Write",
    "description": "Write a complete text file at the requested path.",
    "input_schema": {
        "type": "object",
        "properties": {
            "file_path": {"type": "string"},
            "contents": {"type": "string"},
        },
        "required": ["file_path", "contents"],
    },
}]


def post(payload, timeout=240):
    request = urllib.request.Request(
        BASE + "/v1/messages",
        data=json.dumps(payload).encode(),
        headers={
            "Content-Type": "application/json",
            "X-Api-Key": KEY,
            "Anthropic-Version": "2023-06-01",
        },
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode())


def leaked_text(document):
    text = "\n".join(
        block.get("text", "")
        for block in document.get("content", [])
        if block.get("type") == "text"
    )
    lowered = text.lower()
    return text, "[called tool " in lowered or "mcp_write id=" in lowered


def wait_healthy():
    for _ in range(30):
        try:
            with urllib.request.urlopen(BASE + "/healthz", timeout=2) as response:
                if response.status == 200:
                    return
        except Exception:  # noqa: BLE001
            pass
        time.sleep(1)
    raise RuntimeError("gateway did not become healthy after restart")


def usage_line(document):
    usage = document.get("usage") or {}
    return "input=%d read=%d create=%d output=%d" % (
        usage.get("input_tokens", 0),
        usage.get("cache_read_input_tokens", 0),
        usage.get("cache_creation_input_tokens", 0),
        usage.get("output_tokens", 0),
    )


def main():
    messages = [{
        "role": "user",
        "content": "Create outputs/native-tool-probe.html containing a minimal HTML page. Use Write now.",
    }]
    first = post({
        "model": MODEL,
        "max_tokens": 2048,
        "messages": messages,
        "tools": TOOLS,
        "tool_choice": {"type": "tool", "name": "Write"},
    })
    first_text, first_leak = leaked_text(first)
    calls = [block for block in first.get("content", []) if block.get("type") == "tool_use"]
    print("first stop=%s calls=%d %s" % (first.get("stop_reason"), len(calls), usage_line(first)))
    if first_leak:
        print("FAIL: textual tool marker leaked on first turn: %r" % first_text, file=sys.stderr)
        return 2
    if not calls or calls[0].get("name") != "Write":
        print("FAIL: fable did not return native Write tool_use: %s" % first, file=sys.stderr)
        return 3

    messages.append({"role": "assistant", "content": first["content"]})
    messages.append({
        "role": "user",
        "content": [{
            "type": "tool_result",
            "tool_use_id": calls[0]["id"],
            "content": "File written successfully.",
        }],
    })

    if RESTART:
        subprocess.run(["sudo", "systemctl", "restart", "cli-proxy-api.service"], check=True)
        wait_healthy()

    second = post({
        "model": MODEL,
        "max_tokens": 1024,
        "messages": messages,
        "tools": TOOLS,
    })
    second_text, second_leak = leaked_text(second)
    second_calls = [block for block in second.get("content", []) if block.get("type") == "tool_use"]
    print("second stop=%s calls=%d %s" % (second.get("stop_reason"), len(second_calls), usage_line(second)))
    if second_leak:
        print("FAIL: textual tool marker leaked after native resume: %r" % second_text, file=sys.stderr)
        return 4
    print("PASS: fable returned native tool_use; no pseudo-tool text reached the client")
    return 0


if __name__ == "__main__":
    sys.exit(main())
