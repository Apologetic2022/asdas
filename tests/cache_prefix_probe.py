#!/usr/bin/env python3
"""Distinguish exact-match caching from prefix caching on the Cursor path.

Phase A sends one prompt twice unchanged (exact match). Phase B keeps the same
large history and only varies the final question (shared prefix, different
tail). If B caches roughly as much as A, upstream does prefix caching and a
growing conversation benefits; if only A caches, reuse requires an exact match.
"""
import json
import os
import sys
import time
import urllib.request

BASE = os.environ.get("CPA_BASE", "http://127.0.0.1:8318")
KEY = os.environ.get("CPA_KEY", "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a")
MODEL = os.environ.get("CPA_MODEL", "grok-4.5")


def post(payload, timeout=240):
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
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


DOC = filler(5000, 11)
HISTORY = [
    {"role": "system", "content": "You are a terse assistant. Answer in one short sentence."},
    {"role": "user", "content": "Reference document:\n" + DOC + "\n\nAcknowledge."},
    {"role": "assistant", "content": "Acknowledged."},
]


def run(tag, question):
    payload = {"model": MODEL, "messages": HISTORY + [{"role": "user", "content": question}], "stream": False}
    t0 = time.time()
    doc = post(payload)
    usage = doc.get("usage", {}) or {}
    prompt = usage.get("prompt_tokens", 0)
    cached = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
    pct = (100.0 * cached / prompt) if prompt else 0.0
    print("  %-28s prompt=%-7d cached=%-7d (%5.1f%%) %.1fs" % (tag, prompt, cached, pct, time.time() - t0))
    return pct


def main():
    rounds = int(sys.argv[1]) if len(sys.argv) > 1 else 3
    print("== prefix vs exact cache probe model=%s ==" % MODEL)
    exact, prefix = [], []
    for i in range(rounds):
        print(" round %d" % (i + 1))
        exact.append(run("A exact repeat", "Reply with the single word: ping"))
        time.sleep(2)
        prefix.append(run("B shared prefix, new tail", "Reply with the single word: pong%d" % (i + 1)))
        time.sleep(2)
    print("  -> exact-repeat  best %.1f%%  (all: %s)" % (max(exact), ", ".join("%.1f" % v for v in exact)))
    print("  -> shared-prefix best %.1f%%  (all: %s)" % (max(prefix), ", ".join("%.1f" % v for v in prefix)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
