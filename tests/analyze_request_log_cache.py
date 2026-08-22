"""Summarise real /v1/messages traffic from the gateway's per-request logs.

Groups requests by size band so probe traffic and real work are separated, and
reports how much of each prompt was served from cache. A shape that never hits
the cache shows up as a band with a near-zero read share.
"""

import glob
import json
import os
import re
import sys

LOGS = sorted(glob.glob(sys.argv[1] if len(sys.argv) > 1 else "/opt/cli-proxy/logs/v1-messages-*.log"))

usage_re = re.compile(r'"usage"\s*:\s*(\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\})')
model_re = re.compile(r'"model"\s*:\s*"([^"]+)"')

rows = []
for path in LOGS:
    try:
        with open(path, "rb") as fh:
            blob = fh.read().decode("utf8", "replace")
    except OSError:
        continue
    parts = blob.split("=== RESPONSE ===")
    if len(parts) < 2:
        continue
    request, response = parts[0], parts[1]

    model = ""
    m = model_re.search(request)
    if m:
        model = m.group(1)

    has_tools = '"tools"' in request
    system_blocks = '"system"' in request and re.search(r'"system"\s*:\s*\[', request) is not None
    cache_control = '"cache_control"' in request
    beta = "?beta=true" in request[:400]

    best = None
    for m in usage_re.finditer(response):
        try:
            u = json.loads(m.group(1))
        except ValueError:
            continue
        if not isinstance(u, dict):
            continue
        score = (u.get("input_tokens") or 0) + (u.get("cache_read_input_tokens") or 0) + (u.get("cache_creation_input_tokens") or 0)
        if best is None or score > best[0]:
            best = (score, u)
    if best is None:
        continue
    u = best[1]
    read = u.get("cache_read_input_tokens") or 0
    create = u.get("cache_creation_input_tokens") or 0
    inp = u.get("input_tokens") or 0
    total = read + create + inp
    if total <= 0:
        continue
    rows.append(
        {
            "file": os.path.basename(path),
            "model": model,
            "tools": has_tools,
            "sysblocks": system_blocks,
            "cc": cache_control,
            "beta": beta,
            "input": inp,
            "read": read,
            "create": create,
            "total": total,
        }
    )

print("requests with usage: %d of %d logs" % (len(rows), len(LOGS)))


def band(total):
    if total < 5000:
        return "  <5k"
    if total < 20000:
        return " 5-20k"
    if total < 60000:
        return "20-60k"
    return "  >60k"


groups = {}
for r in rows:
    key = (band(r["total"]), r["tools"], r["cc"], r["beta"])
    groups.setdefault(key, []).append(r)

print()
print("%-8s %-6s %-6s %-6s %6s %10s %10s %10s %7s" % ("band", "tools", "cachec", "beta", "n", "avg total", "avg read", "avg create", "read%"))
for key in sorted(groups):
    g = groups[key]
    n = len(g)
    at = sum(x["total"] for x in g) / n
    ar = sum(x["read"] for x in g) / n
    ac = sum(x["create"] for x in g) / n
    print(
        "%-8s %-6s %-6s %-6s %6d %10.0f %10.0f %10.0f %6.1f%%"
        % (key[0], key[1], key[2], key[3], n, at, ar, ac, 100.0 * ar / at if at else 0)
    )

print()
print("=== zero-read requests, biggest first ===")
zero = [r for r in rows if r["read"] == 0]
zero.sort(key=lambda r: -r["total"])
print("count: %d of %d" % (len(zero), len(rows)))
for r in zero[:12]:
    print(
        "  %-34s %-20s tools=%-5s cc=%-5s beta=%-5s total=%7d create=%7d"
        % (r["file"][11:30], r["model"], r["tools"], r["cc"], r["beta"], r["total"], r["create"])
    )
