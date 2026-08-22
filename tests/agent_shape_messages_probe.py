"""Drive /v1/messages the way a real agent client does, not the way a probe does.

Probes send one plain string system prompt and no tools. Real work sends a
system array with cache_control breakpoints, a tools array, and assistant turns
carrying tool_use blocks answered by user turns of tool_result blocks. This
sends the second shape and prints the four usage numbers, so a failure or a
cache miss that only appears under the real shape shows up here.
"""

import json
import sys
import urllib.error
import urllib.request

HOST = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8317"
KEY = sys.argv[2] if len(sys.argv) > 2 else "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODEL = sys.argv[3] if len(sys.argv) > 3 else "claude-sonnet-4-5"
SHAPE = sys.argv[4] if len(sys.argv) > 4 else "all"

PREAMBLE = (
    "You are an interactive CLI tool that helps users with software engineering tasks.\n"
    "Reference material section: the quick brown fox jumps over the lazy dog.\n" * 900
)

TOOLS = [
    {
        "name": "Bash",
        "description": "Run a shell command. " + ("Detailed usage notes. " * 200),
        "input_schema": {
            "type": "object",
            "properties": {"command": {"type": "string"}},
            "required": ["command"],
        },
    },
    {
        "name": "Read",
        "description": "Read a file from disk. " + ("Detailed usage notes. " * 200),
        "input_schema": {
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"],
        },
    },
]


def post(body, beta):
    url = HOST + "/v1/messages" + ("?beta=true" if beta else "")
    req = urllib.request.Request(
        url,
        json.dumps(body).encode(),
        {
            "Content-Type": "application/json",
            "x-api-key": KEY,
            "anthropic-version": "2023-06-01",
            "anthropic-beta": "prompt-caching-2024-07-31",
        },
    )
    try:
        return json.loads(urllib.request.urlopen(req, timeout=300).read()), None
    except urllib.error.HTTPError as exc:
        return None, "HTTP %d %s" % (exc.code, exc.read()[:400].decode("utf8", "replace"))
    except Exception as exc:  # noqa: BLE001
        return None, repr(exc)


def show(label, result, err):
    if err:
        print("%-28s FAIL %s" % (label, err))
        return None
    u = result.get("usage", {})
    inp = u.get("input_tokens", 0)
    read = u.get("cache_read_input_tokens", 0)
    create = u.get("cache_creation_input_tokens", 0)
    print(
        "%-28s input=%6d read=%6d create=%6d out=%4d stop=%s"
        % (label, inp, read, create, u.get("output_tokens", 0), result.get("stop_reason"))
    )
    return result


def plain_system():
    return {
        "model": MODEL,
        "max_tokens": 32,
        "system": PREAMBLE,
        "messages": [{"role": "user", "content": "Say ready."}],
    }


def block_system():
    return {
        "model": MODEL,
        "max_tokens": 32,
        "system": [
            {"type": "text", "text": "You are Claude Code."},
            {"type": "text", "text": PREAMBLE, "cache_control": {"type": "ephemeral"}},
        ],
        "messages": [{"role": "user", "content": [{"type": "text", "text": "Say ready."}]}],
    }


def with_tools():
    body = block_system()
    tools = json.loads(json.dumps(TOOLS))
    tools[-1]["cache_control"] = {"type": "ephemeral"}
    body["tools"] = tools
    return body


def tool_loop():
    body = with_tools()
    body["messages"] = [
        {"role": "user", "content": [{"type": "text", "text": "Run `ls` then tell me the first file."}]},
        {
            "role": "assistant",
            "content": [
                {"type": "text", "text": "I'll list the directory."},
                {"type": "tool_use", "id": "toolu_01A", "name": "Bash", "input": {"command": "ls"}},
            ],
        },
        {
            "role": "user",
            "content": [
                {
                    "type": "tool_result",
                    "tool_use_id": "toolu_01A",
                    "content": [{"type": "text", "text": "README.md\nmain.go\n"}],
                }
            ],
        },
    ]
    return body


SHAPES = {
    "plain": plain_system,
    "blocks": block_system,
    "tools": with_tools,
    "toolloop": tool_loop,
}

for name, build in SHAPES.items():
    if SHAPE not in ("all", name):
        continue
    for beta in (False, True):
        label = "%s%s" % (name, "?beta=true" if beta else "")
        # Two passes: the second should read what the first wrote.
        for attempt in (1, 2):
            result, err = post(build(), beta)
            show("%s pass%d" % (label, attempt), result, err)
