#!/usr/bin/env python3
"""Check the four token classes the usage panel shows.

The panel stores usage.Detail verbatim: input / output / cache_read /
cache_creation, totalled by addition. Cursor reports prompt_tokens as the whole
prompt with the cache counters as subsets, so the panel's input column is
prompt - read - creation. This probe reconstructs that split from the wire and
asserts it is coherent (no double counting, nothing negative, the classes add
back up to the total).
"""
import json
import sys
import urllib.request

HOST = "http://15.204.94.214:8317"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"


def post(path, body, stream=False):
    req = urllib.request.Request(
        HOST + path,
        json.dumps(body).encode(),
        {"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
    )
    with urllib.request.urlopen(req, timeout=300) as r:
        raw = r.read().decode()
    if not stream:
        return json.loads(raw)
    usage = None
    for line in raw.splitlines():
        if not line.startswith("data: ") or line[6:].strip() == "[DONE]":
            continue
        chunk = json.loads(line[6:])
        if chunk.get("usage"):
            usage = chunk["usage"]
    return {"usage": usage}


def split(usage):
    """Reproduce the panel's four columns from an OpenAI usage block."""
    prompt = usage.get("prompt_tokens", 0)
    details = usage.get("prompt_tokens_details") or {}
    read = details.get("cached_tokens", 0)
    create = details.get("cache_creation_tokens", 0)
    return {
        "input": prompt - read - create,
        "output": usage.get("completion_tokens", 0),
        "cache_read": read,
        "cache_create": create,
        "prompt": prompt,
        "total": usage.get("total_tokens", 0),
    }


def check(label, usage):
    if not usage:
        print(f"  {label}: FAIL no usage reported at all")
        return False
    f = split(usage)
    print(
        f"  {label}: input={f['input']} output={f['output']} "
        f"cache_create={f['cache_create']} cache_read={f['cache_read']} "
        f"(prompt={f['prompt']} total={f['total']})"
    )
    ok = True
    if f["input"] < 0:
        print("    FAIL input is negative: cache counters exceed the prompt")
        ok = False
    if f["cache_read"] + f["cache_create"] > f["prompt"]:
        print("    FAIL cache counters are not subsets of the prompt")
        ok = False
    if f["input"] + f["output"] + f["cache_read"] + f["cache_create"] != f["total"]:
        print("    FAIL the four classes do not add up to total_tokens")
        ok = False
    return ok


def main():
    model = sys.argv[1] if len(sys.argv) > 1 else "claude-sonnet-4-5"
    # A long shared prefix is what gets cached; the short tail keeps each turn
    # distinct so a hit is a real hit and not a deduplicated request.
    prefix = "Reference material, section %d: the quick brown fox.\n" * 900
    ok = True
    for turn in range(3):
        body = {
            "model": model,
            "messages": [
                {"role": "system", "content": prefix},
                {"role": "user", "content": f"Reply with just the number {turn}."},
            ],
            "max_tokens": 32,
        }
        ok &= check(f"turn{turn} non-stream", post("/v1/chat/completions", body).get("usage"))
        body["stream"] = True
        body["stream_options"] = {"include_usage": True}
        ok &= check(
            f"turn{turn} stream    ",
            post("/v1/chat/completions", body, stream=True).get("usage"),
        )
    print("PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
