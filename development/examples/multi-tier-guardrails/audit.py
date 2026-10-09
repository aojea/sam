# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Reads a sam-node log on stdin and prints one line per authorization or task decision."""

import json
import re
import sys

AUDIT = re.compile(r"Audit Traceability\s+(\{.*\})\s*$")
TAR_DENY = re.compile(r'task authorization denied tool "([^"]+)".*tar_block\[1\] \("([^"]+)"\)')
KEEP = re.compile(r"\[Egress\] (Serving|Withdrawn)|peer banned|SAM Node Online|PeerID:")

for raw_line in sys.stdin:
    line = raw_line.rstrip("\n")
    m = AUDIT.search(line)
    if m:
        try:
            d = json.loads(m.group(1))
        except json.JSONDecodeError:
            continue
        who = f"{d.get('role') or '-'} {d.get('peer_id', '')[:12]}…"
        what = " ".join(x for x in (d.get("method"), d.get("path")) if x) or d.get("protocol", "")
        print(f"{d.get('decision', '?').upper():<5} {who:<22} {what:<34} {d.get('target', '')}", flush=True)
        continue
    tm = TAR_DENY.search(line)
    if tm:
        print(f"DENY  tar_block ({tm.group(2)})     tools/call {tm.group(1):<23} mcp://orders-db", flush=True)
        continue
    if "peer banned" in line:
        peer = re.search(r'"peer":\s*"([^"]+)"', line)
        print(f"BANNED {peer.group(1) if peer else ''}: control plane revoked peer", flush=True)
        continue
    if KEEP.search(line):
        print((line.split("\t")[-1] if "\t" in line else line).strip(), flush=True)
