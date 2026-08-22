#!/usr/bin/env python3
"""Send one request repeatedly and report the answer alongside the cache read.

A repeated request is the case the checkpoint cache gets wrong most easily. The
stored checkpoint covers the whole request, so a resume has nothing left to ask
and can only nudge the model to carry on — which answers the finished
conversation instead of this request, and rewrites the prompt so the upstream
cache misses too. Every repeat here must answer the question, keep a flat
prompt, and keep its cache read.

Usage: repeat_request_probe.py [port] [repeats] [model]
"""
import json
import sys
import urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8317"
REPEATS = int(sys.argv[2]) if len(sys.argv) > 2 else 6
MODEL = sys.argv[3] if len(sys.argv) > 3 else "claude-sonnet-4-5"
BASE = f"http://127.0.0.1:{PORT}/v1/chat/completions"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"

PROMPT = "Reply with ONLY the number and nothing else. What is 3 + 35?"
EXPECT = "38"


def call():
    req = urllib.request.Request(
        BASE,
        data=json.dumps({
            "model": MODEL,
            "stream": False,
            "messages": [{"role": "user", "content": PROMPT}],
            "max_tokens": 40,
        }).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
    )
    with urllib.request.urlopen(req, timeout=180) as resp:
        return json.loads(resp.read().decode())


print(f"model={MODEL} repeats={REPEATS} expecting {EXPECT!r} every time")
print(f"{'#':>3} {'prompt':>8} {'cached':>8} {'ok':>4}  answer")

wrong = 0
prompts = []
for i in range(1, REPEATS + 1):
    try:
        d = call()
    except Exception as exc:
        print(f"{i:>3} {'ERR':>8} {'-':>8} {'no':>4}  {exc}")
        wrong += 1
        continue
    usage = d.get("usage") or {}
    prompt = usage.get("prompt_tokens", 0)
    cached = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
    answer = " ".join((d["choices"][0]["message"].get("content") or "").split())
    ok = EXPECT in answer
    wrong += 0 if ok else 1
    prompts.append(prompt)
    print(f"{i:>3} {prompt:>8} {cached:>8} {'yes' if ok else 'NO':>4}  {answer[:44]}")

print()
if wrong:
    print(f"FAIL: {wrong}/{REPEATS} repeats did not answer the question asked")
elif len(set(prompts)) > 1:
    print(f"FAIL: prompt grew across repeats ({prompts}); the turn is being replayed")
else:
    print(f"PASS: every repeat answered {EXPECT!r} at a flat {prompts[0]}-token prompt")
