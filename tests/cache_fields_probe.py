#!/usr/bin/env python3
"""Check that cache creation is reported separately from cache read.

The two mean opposite things: a read was served from the cache, a creation
paid to fill it. Reporting only the read makes a cold turn look identical to
one that never cached at all, which is what the monitor showed as rows with no
cache information.

The probe sends one unique long prompt twice. The first turn must write the
cache, the second must read it.

Usage: cache_fields_probe.py [port] [model]
"""
import json
import sys
import urllib.request
import uuid

PORT = sys.argv[1] if len(sys.argv) > 1 else "8317"
MODEL = sys.argv[2] if len(sys.argv) > 2 else "claude-sonnet-4-5"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
CHAT = f"http://127.0.0.1:{PORT}/v1/chat/completions"
MSGS = f"http://127.0.0.1:{PORT}/v1/messages"

fails = []


def post(url, body, headers):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), headers=headers)
    with urllib.request.urlopen(req, timeout=180) as resp:
        return json.loads(resp.read().decode())


def unique_prompt():
    nonce = uuid.uuid4().hex[:8]
    return f"{('unique-' + nonce + ' ') * 3000} Reply with only: OK"


print(f"model={MODEL}")

print("=== /v1/chat/completions ===")
messages = [{"role": "user", "content": unique_prompt()}]
for turn in ("cold", "warm"):
    d = post(CHAT, {"model": MODEL, "stream": False, "messages": messages, "max_tokens": 20},
             {"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"})
    details = (d.get("usage") or {}).get("prompt_tokens_details") or {}
    read = details.get("cached_tokens")
    created = details.get("cache_creation_tokens")
    print(f"  {turn:<5} prompt={d['usage']['prompt_tokens']:<8} "
          f"cached_tokens={read} cache_creation_tokens={created}")
    if created is None:
        fails.append("chat: cache_creation_tokens missing entirely")
    elif turn == "cold" and not created:
        fails.append("chat: a cold turn reported no cache creation")
    elif turn == "warm" and not read:
        fails.append("chat: a warm turn reported no cache read")

print("=== /v1/messages ===")
messages = [{"role": "user", "content": unique_prompt()}]
for turn in ("cold", "warm"):
    d = post(MSGS, {"model": MODEL, "messages": messages, "max_tokens": 20},
             {"Content-Type": "application/json", "x-api-key": KEY,
              "anthropic-version": "2023-06-01"})
    u = d.get("usage") or {}
    print(f"  {turn:<5} {json.dumps(u)}")
    if turn == "cold" and not u.get("cache_creation_input_tokens"):
        fails.append("messages: a cold turn reported no cache_creation_input_tokens")
    if turn == "warm" and not u.get("cache_read_input_tokens"):
        fails.append("messages: a warm turn reported no cache_read_input_tokens")

print()
if fails:
    print("FAIL:")
    for f in fails:
        print("  -", f)
else:
    print("PASS: both protocols report cache creation and cache read separately")
