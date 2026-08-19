#!/usr/bin/env python3
"""Summarise prompt-cache effectiveness from CPA request logs.

Reads the per-request v1-messages / v1-chat-completions logs and reports, for
every response that carried usage, how much of the prompt upstream served from
cache. Run on the gateway host: analyze_cache_logs.py [log-dir] [hours]
"""
import os
import re
import sys
import time

LOG_DIR = sys.argv[1] if len(sys.argv) > 1 else "/opt/cli-proxy/logs"
HOURS = float(sys.argv[2]) if len(sys.argv) > 2 else 24.0

USAGE_RE = re.compile(
    rb'"usage":\{"input_tokens":(\d+),"output_tokens":(\d+)'
    rb'(?:,"cache_read_input_tokens":(\d+))?'
)
OPENAI_RE = re.compile(
    rb'"prompt_tokens":(\d+).{0,200}?"cached_tokens":(\d+)', re.S
)

cutoff = time.time() - HOURS * 3600
rows = []
for name in os.listdir(LOG_DIR):
    if not name.startswith(("v1-messages-", "v1-chat-completions-")):
        continue
    path = os.path.join(LOG_DIR, name)
    try:
        if os.path.getmtime(path) < cutoff:
            continue
        with open(path, "rb") as fh:
            blob = fh.read()
    except OSError:
        continue
    best = None
    for match in USAGE_RE.finditer(blob):
        prompt = int(match.group(1))
        cached = int(match.group(3) or 0)
        if prompt <= 0:
            continue
        if best is None or prompt > best[0]:
            best = (prompt, cached)
    if best is None:
        for match in OPENAI_RE.finditer(blob):
            prompt, cached = int(match.group(1)), int(match.group(2))
            if prompt > 0 and (best is None or prompt > best[0]):
                best = (prompt, cached)
    if best:
        rows.append(best)

if not rows:
    print("no usage rows found in %s over the last %.1fh" % (LOG_DIR, HOURS))
    raise SystemExit(0)

rows.sort(key=lambda r: -r[0])
total_prompt = sum(r[0] for r in rows)
total_cached = sum(r[1] for r in rows)
big = [r for r in rows if r[0] >= 10000]

print("requests with usage : %d (last %.1fh, %s)" % (len(rows), HOURS, LOG_DIR))
print("prompt tokens       : %d" % total_prompt)
print("cached tokens       : %d" % total_cached)
print("overall cache share : %.1f%%" % (100.0 * total_cached / total_prompt))


def bucket(label, subset):
    if not subset:
        print("%-20s: none" % label)
        return
    hits = sum(1 for p, c in subset if c >= 0.5 * p)
    share = 100.0 * sum(c for _, c in subset) / sum(p for p, _ in subset)
    print(
        "%-20s: n=%-5d cache share %5.1f%%  requests >=50%% cached: %d (%.0f%%)"
        % (label, len(subset), share, hits, 100.0 * hits / len(subset))
    )


bucket("all requests", rows)
bucket("prompts >=10k", big)

print("\nlargest 15 prompts (prompt / cached / share):")
for prompt, cached in rows[:15]:
    print("  %-8d %-8d %5.1f%%" % (prompt, cached, 100.0 * cached / prompt))
