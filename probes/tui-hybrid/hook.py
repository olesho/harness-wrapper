#!/usr/bin/env python3
"""Hook command for the probe: append {t, payload} to $HOOKLOG and exit 0."""
import json
import os
import sys
import time

t = time.time()
raw = sys.stdin.read()
try:
    payload = json.loads(raw)
except Exception:
    payload = {"_raw": raw}
with open(os.environ["HOOKLOG"], "a") as f:
    f.write(json.dumps({"t": t, "payload": payload}) + "\n")
