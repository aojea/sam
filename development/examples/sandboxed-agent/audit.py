"""Reads a sam-node log on stdin and prints the lines an admin watches: what
the node serves, and one line per authorization decision.

    make pep URL=https://... 2>&1 | python3 audit.py
"""

import json
import os
import re
import sys
from contextlib import nullcontext

AUDIT = re.compile(r"Audit Traceability\s+(\{.*\})\s*$")
KEEP = re.compile(r"\[Egress\] (Serving|Withdrawn|Assignments)|peer banned|SAM Node Online|PeerID:")


def verdict(line):
    m = AUDIT.search(line)
    if m:
        try:
            d = json.loads(m.group(1))
        except json.JSONDecodeError:
            return None
        who = f"{d.get('role') or '-'} {d.get('peer_id', '')[:12]}…"
        what = " ".join(x for x in (d.get("method"), d.get("path")) if x) or d.get("protocol", "")
        return f"{d.get('decision', '?').upper():<5} {who:<22} {what:<40} {d.get('target', '')}"
    if "peer banned" in line:
        peer = re.search(r'"peer":\s*"([^"]+)"', line)
        return f"BANNED {peer.group(1) if peer else ''}: the control plane cut this member off"
    if KEEP.search(line):
        # Drop the timestamp and logger columns; keep the message.
        return (line.split("\t")[-1] if "\t" in line else line).strip()
    return None


# AUDIT_RAW=<file> keeps the unfiltered log next to the filtered view.
raw_path = os.environ.get("AUDIT_RAW")
with (open(raw_path, "a") if raw_path else nullcontext()) as raw:
    for raw_line in sys.stdin:
        if raw:
            raw.write(raw_line)
            raw.flush()
        out = verdict(raw_line.rstrip("\n"))
        if out:
            print(out, flush=True)
