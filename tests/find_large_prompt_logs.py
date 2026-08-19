#!/usr/bin/env python3
"""Locate request logs with large prompts and report their usage shape."""
import json
import os
import re
import sys

LOG_DIR = sys.argv[1] if len(sys.argv) > 1 else "/opt/cli-proxy/logs"
MIN_PROMPT = int(sys.argv[2]) if len(sys.argv) > 2 else 40000

USAGE_RE = re.compile(
    rb'"usage":\{"input_tokens":(\d+),"output_tokens":(\d+)'
    rb'(?:,"cache_read_input_tokens":(\d+))?'
)

found = []
for name in os.listdir(LOG_DIR):
    if not name.startswith(("v1-messages-", "v1-chat-completions-")):
        continue
    path = os.path.join(LOG_DIR, name)
    try:
        with open(path, "rb") as fh:
            blob = fh.read()
    except OSError:
        continue
    best = None
    for m in USAGE_RE.finditer(blob):
        prompt, out, cached = int(m.group(1)), int(m.group(2)), int(m.group(3) or 0)
        if best is None or prompt > best[0]:
            best = (prompt, out, cached)
    if best and best[0] >= MIN_PROMPT:
        has_tool_use = b'"type":"tool_use"' in blob or b'"tool_calls"' in blob
        found.append((best[0], best[1], best[2], has_tool_use, name))

found.sort(reverse=True)
print("%-8s %-7s %-8s %-9s %s" % ("prompt", "out", "cached", "toolcall", "file"))
for prompt, out, cached, tool, name in found[:25]:
    print("%-8d %-7d %-8d %-9s %s" % (prompt, out, cached, tool, name))
print("\ntotal files with prompt >= %d: %d" % (MIN_PROMPT, len(found)))
tool_rows = [f for f in found if f[3]]
print("of those, responses containing tool calls: %d" % len(tool_rows))
if tool_rows:
    print("cached>0 among tool-call responses: %d" % sum(1 for f in tool_rows if f[2] > 0))
non_tool = [f for f in found if not f[3]]
if non_tool:
    print("cached>0 among non-tool responses:  %d/%d" % (sum(1 for f in non_tool if f[2] > 0), len(non_tool)))
