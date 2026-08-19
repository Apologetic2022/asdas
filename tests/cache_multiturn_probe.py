#!/usr/bin/env python3
"""Measure prompt-cache reuse across a growing multi-turn conversation.

This is the shape that drives the bill: a client replays the whole transcript
every turn, so from turn 2 onward almost all of the prompt should be served
from the provider cache. Reports cached_tokens as a share of the prompt.
"""
import json
import os
import sys
import time
import urllib.request

BASE = os.environ.get("CPA_BASE", "http://127.0.0.1:8318")
KEY = os.environ.get("CPA_KEY", "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a")
MODEL = os.environ.get("CPA_MODEL", "grok-4.5")
TURNS = int(os.environ.get("CPA_TURNS", "5"))


def post(path, payload, timeout=240):
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + KEY},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def filler(n_words, seed):
    words = []
    x = seed
    for _ in range(n_words):
        x = (x * 1103515245 + 12345) & 0x7FFFFFFF
        words.append("w%05d" % (x % 100000))
    return " ".join(words)


def main():
    label = sys.argv[1] if len(sys.argv) > 1 else "multiturn"
    print("== multi-turn cache probe (%s) model=%s turns=%d ==" % (label, MODEL, TURNS))
    messages = [
        {"role": "system", "content": "You are a terse assistant. Always answer in one short sentence."},
        {"role": "user", "content": "Reference document:\n" + filler(5000, 7) + "\n\nAcknowledge with one word."},
    ]
    hits = 0
    measured = 0
    for turn in range(1, TURNS + 1):
        t0 = time.time()
        try:
            doc = post("/v1/chat/completions", {"model": MODEL, "messages": messages, "stream": False})
        except Exception as exc:  # noqa: BLE001
            print("  turn %d FAILED: %s" % (turn, exc))
            return 1
        usage = doc.get("usage", {}) or {}
        prompt = usage.get("prompt_tokens", 0)
        cached = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
        reply = (doc["choices"][0]["message"].get("content") or "").strip()
        pct = (100.0 * cached / prompt) if prompt else 0.0
        print(
            "  turn %d: prompt=%-7d cached=%-7d (%5.1f%%) %.1fs  reply=%.48r"
            % (turn, prompt, cached, pct, time.time() - t0, reply)
        )
        if turn > 1:
            measured += 1
            if pct >= 50:
                hits += 1
        messages.append({"role": "assistant", "content": reply or "ok"})
        messages.append({"role": "user", "content": "Turn %d: reply with the single word 'ack'." % (turn + 1)})
        time.sleep(2)
    if measured:
        print("  -> %d/%d follow-up turns served mostly from cache" % (hits, measured))
    return 0


if __name__ == "__main__":
    sys.exit(main())
