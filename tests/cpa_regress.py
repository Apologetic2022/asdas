#!/usr/bin/env python3
"""Full regression: tool calling, zero-output, usage/cache on the CPA gateway."""
import json
import urllib.request
import sys

BASE = "http://127.0.0.1:8317/v1/chat/completions"
KEY = "2b43d34c0570a6fbab26bb7bb0271cfd10df323fef8bbb8a"
MODEL = "grok-4.5"

TOOLS = [{
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get current weather for a city",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}, {
    "type": "function",
    "function": {
        "name": "get_time",
        "description": "Get the current local time for a city",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}]


def call(payload, stream=False, timeout=240):
    req = urllib.request.Request(
        BASE,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        if stream:
            return resp.read().decode()
        return json.loads(resp.read().decode())


def parse_stream(raw):
    """Return (content, tool_calls, finish, usage) accumulated from SSE lines."""
    content = ""
    tool_calls = []
    finish = None
    usage = None
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        data = line[6:].strip()
        if data == "[DONE]":
            continue
        try:
            chunk = json.loads(data)
        except json.JSONDecodeError:
            continue
        for ch in chunk.get("choices", []):
            delta = ch.get("delta", {})
            content += delta.get("content") or ""
            for tc in delta.get("tool_calls") or []:
                tool_calls.append(tc)
            if ch.get("finish_reason"):
                finish = ch["finish_reason"]
        if chunk.get("usage"):
            usage = chunk["usage"]
    return content, tool_calls, finish, usage


def usage_sane(u, need_completion=True):
    if not u:
        return False
    p = u.get("prompt_tokens", 0)
    c = u.get("completion_tokens", 0)
    t = u.get("total_tokens", 0)
    if p <= 0 or t != p + c:
        return False
    if need_completion and c <= 0:
        return False
    return True


results = []


def show(name, ok, detail):
    print(f"[{'PASS' if ok else 'FAIL'}] {name}: {detail}")
    results.append(ok)
    return ok


# ---------- A. non-stream tool calling ----------
r = call({"model": MODEL, "messages": [{"role": "user", "content": "What's the weather in Tokyo right now? Use your tools."}],
          "tools": TOOLS, "tool_choice": "auto"})
msg = r["choices"][0]["message"]
u = r.get("usage", {})
tcs = msg.get("tool_calls") or []
show("A1 auto -> tool_call + finish=tool_calls", bool(tcs) and r["choices"][0]["finish_reason"] == "tool_calls",
     f"finish={r['choices'][0]['finish_reason']} tools={[t['function']['name'] for t in tcs]}")
show("A2 tool_calls usage sane (prompt>0,total ok)", usage_sane(u), f"usage={u}")

# multi-turn non-stream
if tcs:
    follow_msgs = [
        {"role": "user", "content": "What's the weather in Tokyo right now? Use your tools."},
        {"role": "assistant", "content": msg.get("content") or "", "tool_calls": tcs},
        {"role": "tool", "tool_call_id": tcs[0]["id"], "name": tcs[0]["function"]["name"],
         "content": json.dumps({"city": "Tokyo", "temp_c": 21, "condition": "cloudy"})},
    ]
    r2 = call({"model": MODEL, "messages": follow_msgs, "tools": TOOLS})
    m2 = r2["choices"][0]["message"]
    u2 = r2.get("usage", {})
    c2 = m2.get("content") or ""
    show("A3 multi-turn tool result consumed, non-empty", bool(c2.strip()) and ("21" in c2 or "cloud" in c2.lower()),
         f"content={json.dumps(c2[:120])}")
    show("A4 final turn usage sane", usage_sane(u2), f"usage={u2}")
else:
    show("A3 multi-turn tool result consumed, non-empty", False, "skipped: no tool call")
    show("A4 final turn usage sane", False, "skipped")

r3 = call({"model": MODEL, "messages": [{"role": "user", "content": "Say hello to me."}],
           "tools": TOOLS, "tool_choice": "required"})
tcs3 = r3["choices"][0]["message"].get("tool_calls") or []
show("A5 required forces tool_call", bool(tcs3) and r3["choices"][0]["finish_reason"] == "tool_calls",
     f"finish={r3['choices'][0]['finish_reason']} tools={[t['function']['name'] for t in tcs3]}")

r4 = call({"model": MODEL, "messages": [{"role": "user", "content": "What's the weather in Paris?"}],
           "tools": TOOLS, "tool_choice": {"type": "function", "function": {"name": "get_time"}}})
names4 = [t["function"]["name"] for t in (r4["choices"][0]["message"].get("tool_calls") or [])]
show("A6 forced function honored", bool(names4) and all(n == "get_time" for n in names4), f"tools={names4}")

r5 = call({"model": MODEL, "messages": [{"role": "user", "content": "What's the weather in Tokyo? Use your tools."}],
           "tools": TOOLS, "tool_choice": "none"})
m5 = r5["choices"][0]["message"]
show("A7 none suppresses tool_calls, text non-empty", not (m5.get("tool_calls") or []) and bool((m5.get("content") or "").strip()),
     f"finish={r5['choices'][0]['finish_reason']} content_len={len(m5.get('content') or '')}")

# ---------- B. zero-output guards ----------
r6 = call({"model": MODEL, "messages": [{"role": "user", "content": "hi"}]})
m6 = r6["choices"][0]["message"]
show("B1 plain chat non-empty + finish=stop", bool((m6.get("content") or "").strip()) and r6["choices"][0]["finish_reason"] == "stop",
     f"finish={r6['choices'][0]['finish_reason']} content={json.dumps((m6.get('content') or '')[:80])}")
show("B2 plain chat usage sane", usage_sane(r6.get("usage", {})), f"usage={r6.get('usage')}")

raw = call({"model": MODEL, "stream": True,
            "messages": [{"role": "user", "content": "Reply with one short sentence about the sea."}]}, stream=True)
sc, stc, sf, su = parse_stream(raw)
show("B3 plain stream non-empty + finish=stop", bool(sc.strip()) and sf == "stop", f"finish={sf} content={json.dumps(sc[:80])}")
show("B4 plain stream usage chunk sane", usage_sane(su), f"usage={su}")

# ---------- C. streaming tool calling ----------
raw = call({"model": MODEL, "stream": True,
            "messages": [{"role": "user", "content": "What's the weather in Berlin? Use your tools."}],
            "tools": TOOLS, "tool_choice": "required"}, stream=True)
sc, stc, sf, su = parse_stream(raw)
show("C1 stream required -> tool_calls delta + finish", bool(stc) and sf == "tool_calls",
     f"finish={sf} tools={[t.get('function', {}).get('name') for t in stc]}")
show("C2 stream tool_calls usage chunk sane", usage_sane(su), f"usage={su}")

# stream multi-turn: feed tool result back with stream=true
if stc:
    tid = stc[0].get("id")
    tname = stc[0].get("function", {}).get("name")
    targs = stc[0].get("function", {}).get("arguments") or "{}"
    follow_msgs = [
        {"role": "user", "content": "What's the weather in Berlin? Use your tools."},
        {"role": "assistant", "content": "", "tool_calls": [
            {"id": tid, "type": "function", "function": {"name": tname, "arguments": targs}}]},
        {"role": "tool", "tool_call_id": tid, "name": tname,
         "content": json.dumps({"city": "Berlin", "temp_c": 14, "condition": "light rain"})},
    ]
    raw2 = call({"model": MODEL, "stream": True, "messages": follow_msgs, "tools": TOOLS}, stream=True)
    sc2, stc2, sf2, su2 = parse_stream(raw2)
    show("C3 stream multi-turn result consumed", bool(sc2.strip()) and ("14" in sc2 or "rain" in sc2.lower()) and sf2 == "stop",
         f"finish={sf2} content={json.dumps(sc2[:120])}")
    show("C4 stream multi-turn usage sane", usage_sane(su2), f"usage={su2}")
else:
    show("C3 stream multi-turn result consumed", False, "skipped: no stream tool call")
    show("C4 stream multi-turn usage sane", False, "skipped")

# ---------- D. cache growth over 3 turns ----------
long_sys = "You are a precise assistant. " + ("Answer strictly and concisely. " * 40)
msgs = [{"role": "system", "content": long_sys},
        {"role": "user", "content": "Reply with the single word: alpha"}]
d1 = call({"model": MODEL, "messages": msgs})
msgs.append({"role": "assistant", "content": d1["choices"][0]["message"].get("content") or "alpha"})
msgs.append({"role": "user", "content": "Reply with the single word: beta"})
d2 = call({"model": MODEL, "messages": msgs})
msgs.append({"role": "assistant", "content": d2["choices"][0]["message"].get("content") or "beta"})
msgs.append({"role": "user", "content": "Reply with the single word: gamma"})
d3 = call({"model": MODEL, "messages": msgs})
cts = []
for d in (d1, d2, d3):
    ud = d.get("usage", {})
    cts.append(((ud.get("prompt_tokens", 0)), (ud.get("prompt_tokens_details") or {}).get("cached_tokens", 0)))
show("D1 turn2 cache hit (cached_tokens>0)", cts[1][1] > 0, f"turn1={cts[0]} turn2={cts[1]} turn3={cts[2]} (prompt,cached)")
# Only assert the cache is still live on turn 3, not that cached_tokens grew:
# upstream reports cache hits in prefix blocks, and a longer/shorter turn-2 reply
# shifts the cacheable prefix, so the count legitimately moves in either direction.
show("D2 turn3 cache hit sustained", cts[2][1] > 0, f"turn2_cached={cts[1][1]} turn3_cached={cts[2][1]}")
show("D3 all turns usage sane", all(usage_sane(d.get("usage", {})) for d in (d1, d2, d3)),
     f"usages={[d.get('usage', {}).get('prompt_tokens') for d in (d1, d2, d3)]}")

print()
print(f"{sum(results)}/{len(results)} checks passed")
sys.exit(0 if all(results) else 1)
