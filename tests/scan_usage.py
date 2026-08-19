#!/usr/bin/env python3
"""Scan CPA request logs and print the usage payload each response reported."""
import glob
import json
import os
import re
import sys

LOG_DIR = sys.argv[1] if len(sys.argv) > 1 else "/opt/cli-proxy/logs"
LIMIT = int(sys.argv[2]) if len(sys.argv) > 2 else 40

USAGE_RE = re.compile(r'"usage"\s*:\s*(\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\})')
BODY_RE = re.compile(r"=== REQUEST BODY ===\n(.*?)\n\n\n", re.S)

rows = []
for path in glob.glob(os.path.join(LOG_DIR, "v1-*.log")):
    try:
        text = open(path, errors="replace").read()
    except OSError:
        continue
    model, nmsg, chars = "", 0, 0
    m = BODY_RE.search(text)
    if m:
        try:
            body = json.loads(m.group(1))
            model = body.get("model", "")
            msgs = body.get("messages") or []
            nmsg = len(msgs)
            chars = len(json.dumps(msgs))
        except ValueError:
            pass
    usages = []
    for mm in USAGE_RE.finditer(text):
        try:
            usages.append(json.loads(mm.group(1)))
        except ValueError:
            pass
    if not usages:
        continue
    rows.append((os.path.getmtime(path), os.path.basename(path), model, nmsg, chars, usages[-1]))

rows.sort()
for _, name, model, nmsg, chars, usage in rows[-LIMIT:]:
    print(f"{name[:46]:48s} {model[:26]:28s} msgs={nmsg:<3d} chars={chars:<8d} {json.dumps(usage)}")
