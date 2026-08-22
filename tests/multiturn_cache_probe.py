"""Print the four usage columns per turn of a growing conversation.

A single request only ever shows a cold cache, so cache regressions hide from
one-shot probes. This walks three turns of one conversation, streaming and
non-streaming, and prints prompt/read/create/fresh for each. A healthy provider
shows the read climbing to nearly the whole prompt by the second turn, and
prompt = read + create + fresh on every row.

    python3 multiturn_cache_probe.py http://127.0.0.1:8317 <key> grok-4.6,claude-sonnet-4-5
"""

import json
import sys
import urllib.request

HOST = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8317"
KEY = sys.argv[2] if len(sys.argv) > 2 else "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODELS = sys.argv[3].split(",") if len(sys.argv) > 3 else ["grok-4.6"]

FILLER = "Reference material section: the quick brown fox jumps over the lazy dog.\n" * 1500


def call(model, msgs, stream):
    body = {"model": model, "messages": msgs, "max_tokens": 16, "stream": stream}
    req = urllib.request.Request(
        HOST + "/v1/chat/completions",
        json.dumps(body).encode(),
        {"Content-Type": "application/json", "Authorization": "Bearer " + KEY},
    )
    raw = urllib.request.urlopen(req, timeout=300).read().decode()
    if not stream:
        j = json.loads(raw)
        return j["usage"], (j["choices"][0]["message"].get("content") or "ok")
    usage, text = {}, ""
    for line in raw.splitlines():
        if not line.startswith("data: ") or line.endswith("[DONE]"):
            continue
        chunk = json.loads(line[6:])
        if chunk.get("usage"):
            usage = chunk["usage"]
        for choice in chunk.get("choices", []):
            text += choice.get("delta", {}).get("content") or ""
    return usage, (text or "ok")


for model in MODELS:
    for stream in (False, True):
        msgs = [{"role": "system", "content": FILLER}]
        label = "stream" if stream else "block "
        for turn in (1, 2, 3):
            msgs.append({"role": "user", "content": "Say the number %d and nothing else." % turn})
            try:
                usage, reply = call(model, msgs, stream)
            except Exception as exc:
                print("%-18s %s t%d ERR %s" % (model, label, turn, exc))
                continue
            msgs.append({"role": "assistant", "content": reply})
            details = usage.get("prompt_tokens_details") or {}
            prompt = usage.get("prompt_tokens", 0)
            read = details.get("cached_tokens", 0)
            create = details.get("cache_creation_tokens", 0)
            print(
                "%-18s %s t%d prompt=%6d read=%6d create=%6d fresh=%6d out=%4d"
                % (model, label, turn, prompt, read, create, prompt - read - create, usage.get("completion_tokens", 0))
            )
