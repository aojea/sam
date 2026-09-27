"""Reads a sam-node log on stdin and prints the lines an admin watches: what
the node serves, and one line per authorization decision.

    make pep URL=https://... 2>&1 | python3 audit.py
"""

import json
import os
import re
import sys

AUDIT = re.compile(r"Audit Traceability\s+(\{.*\})\s*$")
KEEP = re.compile(r"\[Egress\] (Serving|Withdrawn|Assignments)|peer banned|SAM Node Online|PeerID:")
# AUDIT_RAW=<file> keeps the unfiltered log next to the filtered view.
raw = open(os.environ["AUDIT_RAW"], "a") if os.environ.get("AUDIT_RAW") else None

for raw_line in sys.stdin:
    if raw:
        raw.write(raw_line)
        raw.flush()
    line = raw_line.rstrip("\n")
    m = AUDIT.search(line)
    if m:
        try:
            d = json.loads(m.group(1))
        except json.JSONDecodeError:
            continue
        verdict = d.get("decision", "?").upper()
        who = f"{d.get('role') or '-'} {d.get('peer_id', '')[:12]}…"
        what = " ".join(x for x in (d.get("method"), d.get("path")) if x) or d.get("protocol", "")
        print(f"{verdict:<5} {who:<22} {what:<40} {d.get('target', '')}", flush=True)
    elif "peer banned" in line:
        peer = re.search(r'"peer":\s*"([^"]+)"', line)
        print(f"BANNED {peer.group(1) if peer else ''}: the control plane cut this member off", flush=True)
    elif KEEP.search(line):
        # Drop the timestamp and logger columns; keep the message.
        msg = line.split("\t")[-1] if "\t" in line else line
        print(msg.strip(), flush=True)
