#!/usr/bin/env python3
"""Probe upstream prompt-cache behaviour of the CPA Cursor path.

Sends the *same* large prompt several times and reports prompt/cached tokens for
each attempt. A healthy provider cache reports cached_tokens close to the shared
prefix size from the second attempt onward.
"""
import json
import os
import sys
import time
import urllib.request

BASE = os.environ.get("CPA_BASE", "http://127.0.0.1:8317")
KEY = os.environ.get("CPA_KEY", "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a")
MODEL = os.environ.get("CPA_MODEL", "claude-4.5-haiku")


def post(path, payload, timeout=180):
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + KEY},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def usage_of(doc):
    u = doc.get("usage", {}) or {}
    prompt = u.get("prompt_tokens", 0)
    cached = (u.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
    return prompt, cached, u.get("completion_tokens", 0)


def filler(n_words, seed):
    # Deterministic pseudo-text so the prompt is large but byte-identical between runs.
    words = []
    x = seed
    for _ in range(n_words):
        x = (x * 1103515245 + 12345) & 0x7FFFFFFF
        words.append("w%05d" % (x % 100000))
    return " ".join(words)


BIG = filler(6000, 42)


def base_messages():
    return [
        {"role": "system", "content": "You are a terse assistant. Answer in one short sentence."},
        {"role": "user", "content": "Reference document:\n" + BIG + "\n\nRemember this document."},
        {"role": "assistant", "content": "Noted. I have the document."},
    ]


def main():
    label = sys.argv[1] if len(sys.argv) > 1 else "identical-prompt"
    rounds = int(sys.argv[2]) if len(sys.argv) > 2 else 4
    print("== cache probe (%s) model=%s ==" % (label, MODEL))
    rows = []
    for i in range(rounds):
        msgs = base_messages() + [{"role": "user", "content": "Reply with the single word: ping"}]
        t0 = time.time()
        try:
            doc = post("/v1/chat/completions", {"model": MODEL, "messages": msgs, "stream": False})
        except Exception as exc:  # noqa: BLE001
            print("  attempt %d FAILED: %s" % (i + 1, exc))
            continue
        prompt, cached, out = usage_of(doc)
        rows.append((prompt, cached))
        pct = (100.0 * cached / prompt) if prompt else 0.0
        print(
            "  attempt %d: prompt=%-7d cached=%-7d (%5.1f%%) out=%-5d %.1fs"
            % (i + 1, prompt, cached, pct, out, time.time() - t0)
        )
        time.sleep(2)
    if len(rows) >= 2:
        best = max(c for _, c in rows[1:])
        pr = rows[0][0] or 1
        print("  -> best cached after first attempt: %d / %d (%.1f%%)" % (best, pr, 100.0 * best / pr))
    return 0


if __name__ == "__main__":
    sys.exit(main())
