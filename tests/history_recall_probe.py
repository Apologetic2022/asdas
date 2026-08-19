#!/usr/bin/env python3
"""Verify the model still sees conversation history and tool results.

Guards the turn-structured history change: if the server ignored the turn list
the transcript would vanish silently, so each check plants a fact earlier in the
conversation and asks for it back.
"""
import json
import os
import sys
import time
import urllib.request

BASE = os.environ.get("CPA_BASE", "http://127.0.0.1:8318")
KEY = os.environ.get("CPA_KEY", "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a")
MODEL = os.environ.get("CPA_MODEL", "grok-4.5")

PASS = FAIL = 0


def post(payload, timeout=240):
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + KEY},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def check(name, ok, detail=""):
    global PASS, FAIL
    if ok:
        PASS += 1
        print("  PASS %-42s %s" % (name, detail))
    else:
        FAIL += 1
        print("  FAIL %-42s %s" % (name, detail))


def content_of(doc):
    return (doc["choices"][0]["message"].get("content") or "").strip()


def recall_plain():
    messages = [
        {"role": "system", "content": "Answer with the single requested token and nothing else."},
        {"role": "user", "content": "Remember this passphrase: HALIBUT-7731. Reply 'stored'."},
        {"role": "assistant", "content": "stored"},
        {"role": "user", "content": "What passphrase did I give you? Reply with it exactly."},
    ]
    reply = content_of(post({"model": MODEL, "messages": messages, "stream": False}))
    check("recalls a fact from an earlier turn", "HALIBUT-7731" in reply.upper(), repr(reply[:70]))


def recall_multi_turn():
    messages = [
        {"role": "system", "content": "Answer tersely."},
        {"role": "user", "content": "My favourite colour is chartreuse. Reply 'ok'."},
        {"role": "assistant", "content": "ok"},
        {"role": "user", "content": "My favourite number is 4291. Reply 'ok'."},
        {"role": "assistant", "content": "ok"},
        {"role": "user", "content": "My favourite city is Reykjavik. Reply 'ok'."},
        {"role": "assistant", "content": "ok"},
        {"role": "user", "content": "List my favourite colour, number and city in one line."},
    ]
    reply = content_of(post({"model": MODEL, "messages": messages, "stream": False})).lower()
    ok = "chartreuse" in reply and "4291" in reply and "reykjavik" in reply
    check("recalls facts across three turns", ok, repr(reply[:90]))


def recall_tool_result():
    messages = [
        {"role": "system", "content": "Answer tersely."},
        {"role": "user", "content": "What is the weather in Zurich?"},
        {
            "role": "assistant",
            "content": "",
            "tool_calls": [{
                "id": "call_zur1",
                "type": "function",
                "function": {"name": "get_weather", "arguments": json.dumps({"city": "Zurich"})},
            }],
        },
        {"role": "tool", "tool_call_id": "call_zur1", "name": "get_weather",
         "content": json.dumps({"temp_c": 17, "condition": "hailstorm"})},
        {"role": "user", "content": "Using only the tool result above, state the temperature and condition."},
    ]
    tools = [{
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Get current weather for a city",
            "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]},
        },
    }]
    reply = content_of(post({"model": MODEL, "messages": messages, "tools": tools, "stream": False})).lower()
    check("recalls a tool result from history", "17" in reply and "hail" in reply, repr(reply[:90]))


def main():
    print("== history recall probe model=%s base=%s ==" % (MODEL, BASE))
    for fn in (recall_plain, recall_multi_turn, recall_tool_result):
        try:
            fn()
        except Exception as exc:  # noqa: BLE001
            check(fn.__name__, False, "error: %s" % exc)
        time.sleep(2)
    print("  -> %d passed, %d failed" % (PASS, FAIL))
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
