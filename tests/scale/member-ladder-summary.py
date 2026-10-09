#!/usr/bin/env python3
#
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
"""Render a member-ladder result directory as one table per step.

Usage: member-ladder-summary.py DIR "minion ..." "minion ..." ...

Each step's join reports (one per minion, written when its fleet became
resident) are merged member by member, so the percentiles are exact across
the step rather than an average of averages. The control plane and router
expositions taken around each step give what the mesh did to carry it.
"""
import glob
import json
import os
import re
import sys

out = sys.argv[1]
steps = [s.split() for s in sys.argv[2:]]


def pct(sorted_values, p):
    if not sorted_values:
        return 0.0
    k = max(0, min(len(sorted_values) - 1, int(round(p / 100 * len(sorted_values) + 0.5)) - 1))
    return sorted_values[k]


line = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})?\s+(\S+)')


def parse_all(prefix, tag):
    series = {}
    for path in sorted(glob.glob(os.path.join(out, f"{prefix}-*-{tag}.prom"))):
        pod = os.path.basename(path)[: -len(f"-{tag}.prom")]
        for raw in open(path):
            m = line.match(raw)
            if not m:
                continue
            labels = tuple(sorted(re.findall(r'(\w+)="((?:[^"\\]|\\.)*)"', m.group(2) or ""))) + (("pod", pod),)
            try:
                series[(m.group(1), labels)] = float(m.group(3))
            except ValueError:
                pass
    return series


def total(series, name, **match):
    return sum(v for (n, labels), v in series.items()
               if n == name and all(dict(labels).get(k) == val for k, val in match.items()))


def delta_by(before, after, name, label):
    keys = {dict(labels).get(label) for (n, labels) in list(before) + list(after) if n == name}
    return {k: round(total(after, name, **{label: k}) - total(before, name, **{label: k})) for k in sorted(keys, key=str)}


def p99_of_delta(before, after, name):
    buckets = {}
    for (n, labels), v in after.items():
        if n == name + "_bucket":
            le = dict(labels)["le"]
            buckets[le] = buckets.get(le, 0) + v - before.get((n, labels), 0)
    if not buckets or buckets.get("+Inf", 0) <= 0:
        return None
    target = 0.99 * buckets["+Inf"]
    for le in sorted(buckets, key=lambda s: float(s)):
        if buckets[le] >= target:
            return float(le)
    return float("inf")


print()
print("| step | minions | resident before | joining | ready | completed | failed | ready p50/p95/max ms | journey p50/p95/max ms | calls/member | elapsed s |")
print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
notes = []
resident = 0
for i, minions in enumerate(steps, 1):
    reports = []
    for m in minions:
        path = os.path.join(out, f"step{i}-{m}.json")
        if os.path.exists(path):
            reports.append(json.load(open(path))["join"])
    if not reports:
        break
    members = [mem for r in reports for mem in r["members"]]
    ready = sorted(mem["ready_ms"] for mem in members if mem.get("ready_ms"))
    journey = sorted(mem["ready_ms"] + mem["first_call_ms"] for mem in members if mem.get("first_call_ms"))
    count = sum(r["count"] for r in reports)
    completed = sum(r["completed"] for r in reports)
    failed = sum(r["failed"] for r in reports)
    attempts = sum(r["call_attempts"] for r in reports)
    elapsed = max(r["elapsed_seconds"] for r in reports)
    print(f"| {i} | {len(reports)}/{len(minions)} | {resident} | {count} | {len(ready)} | {completed} | {failed} | "
          f"{pct(ready,50):.0f}/{pct(ready,95):.0f}/{(ready[-1] if ready else 0):.0f} | "
          f"{pct(journey,50):.0f}/{pct(journey,95):.0f}/{(journey[-1] if journey else 0):.0f} | "
          f"{(attempts / completed if completed else 0):.2f} | {elapsed:.0f} |")
    for r in reports:
        for stage, n in sorted(r.get("errors", {}).items()):
            notes.append(f"step {i}: {n} x {stage}")
        for msg, n in sorted(r.get("call_errors", {}).items()):
            notes.append(f"step {i}: {n} call attempt(s) failed with: {msg}")
    resident += count
for n in notes:
    print(n)

for i in range(1, len(steps) + 1):
    b, a = parse_all("agentmesh-control-plane", f"step{i}-before"), parse_all("agentmesh-control-plane", f"step{i}-after")
    if not (b and a):
        continue
    codes = delta_by(b, a, "agentmesh_control_plane_http_requests_total", "code")
    p99 = p99_of_delta(b, a, "agentmesh_control_plane_http_request_duration_seconds")
    nodes = max([v for (n, labels), v in a.items() if n == "agentmesh_control_plane_enrolled_nodes" and dict(labels).get("role") == "mesh:role:node"] or [0])
    peers = max([v for (n, labels), v in a.items() if n == "agentmesh_control_plane_mesh_connected_peers"] or [0])
    print(f"\nstep {i} control plane: requests {codes}; p99 <= {p99} s; enrolled nodes {nodes:.0f}; mesh peers seen {peers:.0f}")
    for r in sorted({re.sub(r"-step\d+-(before|after)\.prom$", "", os.path.basename(p)) for p in glob.glob(os.path.join(out, f"agentmesh-router-*-step{i}-after.prom"))}):
        rb = {k: v for k, v in parse_all("agentmesh-router", f"step{i}-before").items() if dict(k[1]).get("pod") == r}
        ra = {k: v for k, v in parse_all("agentmesh-router", f"step{i}-after").items() if dict(k[1]).get("pod") == r}
        refused = delta_by(rb, ra, "agentmesh_router_inbound_connections_refused_total", "reason")
        hs = delta_by(rb, ra, "agentmesh_router_auth_handshakes_total", "result")
        print(f"  {r}: authenticated peers {total(ra, 'agentmesh_router_authenticated_peers'):.0f}, connected {total(ra, 'agentmesh_router_connected_peers'):.0f}, "
              f"rss {total(rb, 'process_resident_memory_bytes')/2**20:.0f} -> {total(ra, 'process_resident_memory_bytes')/2**20:.0f} MiB, "
              f"handshakes {dict((k, v) for k, v in hs.items() if v)}, refused {dict((k, v) for k, v in refused.items() if v) or 'none'}")

# Hold reports, when the fleets were interrupted.
holds = []
for path in sorted(glob.glob(os.path.join(out, "sam-ladder-*.json"))):
    j = json.load(open(path))["join"]
    if j.get("hold"):
        holds.append((os.path.basename(path)[:-5], j["hold"]))
if holds:
    print()
    print("| minion | hold s | resident | fewest ready | flaps | members flapped | outage p50/max ms | not ready at end | exited |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for name, h in holds:
        print(f"| {name} | {h['seconds']:.0f} | {h['resident']} | {h['min_ready']} | {h['flaps']} | {h['members_flapped']} | "
              f"{h['outage_ms']['p50']:.0f}/{h['outage_ms']['max']:.0f} | {h['members_not_ready_at_end']} | {h['members_exited']} |")
