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
"""Render a member-journey result directory as one table and a verdict.

Usage: member-journey-summary.py DIR [JOURNEY_P50_MS JOURNEY_P95_MS OUTAGE_MAX_MS CP_P99_S]

Exit status 1 when any check fails, so the harness and CI can use it as a gate.
"""
import glob
import json
import os
import re
import sys

out = sys.argv[1]
t_p50, t_p95, t_outage, t_cp_p99 = (float(v) for v in (sys.argv[2:6] + ["5000", "15000", "60000", "1"][len(sys.argv) - 2:]))

verdicts = []


def check(name, ok, detail):
    verdicts.append((name, ok, detail))


def load(phase):
    path = os.path.join(out, phase + ".json")
    return json.load(open(path))["join"] if os.path.exists(path) else None


print()
print("| phase | members | ready | completed | failed | ready p50/p95/max ms | journey p50/p95/max ms | calls/member |")
print("| --- | --- | --- | --- | --- | --- | --- | --- |")
left_behind = []
for phase in ("burst", "late", "late-after-rollout"):
    j = load(phase)
    if not j:
        continue
    r, jo = j["ready_ms"], j["journey_ms"]
    per = j["call_attempts"] / j["completed"] if j["completed"] else 0
    print(f"| {phase} | {j['count']} | {j['ready']} | {j['completed']} | {j['failed']} | "
          f"{r['p50']:.0f}/{r['p95']:.0f}/{r['max']:.0f} | {jo['p50']:.0f}/{jo['p95']:.0f}/{jo['max']:.0f} | {per:.2f} |")
    for stage, n in sorted(j.get("errors", {}).items()):
        left_behind.append(f"{phase}: {n} x {stage}")
    for msg, n in sorted(j.get("call_errors", {}).items()):
        left_behind.append(f"{phase}: {n} call attempt(s) failed with: {msg}")
    check(f"{phase}: every member completed", j["failed"] == 0, f"{j['failed']} of {j['count']} failed")
    if j["completed"]:
        check(f"{phase}: journey p50 <= {t_p50:.0f} ms", jo["p50"] <= t_p50, f"{jo['p50']:.0f} ms")
        check(f"{phase}: journey p95 <= {t_p95:.0f} ms", jo["p95"] <= t_p95, f"{jo['p95']:.0f} ms")
for line_ in left_behind:
    print(line_)

burst = load("burst")
h = burst.get("hold") if burst else None
if h:
    print()
    print(f"hold: {h['seconds']:.0f} s, {h['samples']} samples; resident {h['resident']}, fewest ready {h['min_ready']}; "
          f"{h['flaps']} flaps across {h['members_flapped']} members, outage p50/max "
          f"{h['outage_ms']['p50']:.0f}/{h['outage_ms']['max']:.0f} ms; "
          f"{h['members_not_ready_at_end']} not ready at the end, {h['members_exited']} exited")
    check("hold: every member ready at the end", h["members_not_ready_at_end"] == 0, f"{h['members_not_ready_at_end']} not ready")
    check("hold: no member process exited", h["members_exited"] == 0, f"{h['members_exited']} exited")
    check(f"hold: longest outage <= {t_outage/1000:.0f} s", h["outage_ms"]["max"] <= t_outage, f"{h['outage_ms']['max']/1000:.1f} s")

# What the mesh reported, as differences across the run.
line = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})?\s+(\S+)')


def parse(path):
    series = {}
    if not os.path.exists(path):
        return series
    for raw in open(path):
        m = line.match(raw)
        if not m:
            continue
        labels = tuple(sorted(re.findall(r'(\w+)="((?:[^"\\]|\\.)*)"', m.group(2) or "")))
        try:
            series[(m.group(1), labels)] = float(m.group(3))
        except ValueError:
            pass
    return series


def parse_all(prefix, tag):
    # One exposition per pod; counters are summed across pods with the pod
    # name folded into the key, so a replica's own counter is only ever
    # differenced against itself.
    series = {}
    for path in sorted(glob.glob(os.path.join(out, f"{prefix}-*-{tag}.prom"))):
        pod = os.path.basename(path)[: -len(f"-{tag}.prom")]
        for (name, labels), v in parse(path).items():
            series[(name, labels + (("pod", pod),))] = v
    return series


def total(series, name, **match):
    return sum(v for (n, labels), v in series.items()
               if n == name and all(dict(labels).get(k) == val for k, val in match.items()))


def delta_by(before, after, name, label):
    keys = set()
    for (n, labels) in list(before) + list(after):
        if n == name:
            keys.add(dict(labels).get(label))
    return {k: round(total(after, name, **{label: k}) - total(before, name, **{label: k})) for k in sorted(keys, key=str)}


def p99_of_delta(before, after, name):
    # Histogram buckets are cumulative counts; the difference across the run
    # is the run's own histogram, and p99 is the first bound holding 99%.
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


cp_before, cp_after = parse_all("sam-control-plane", "before"), parse_all("sam-control-plane", "after")
if cp_before and cp_after:
    print()
    codes = delta_by(cp_before, cp_after, "sam_control_plane_http_requests_total", "code")
    errors = sum(v for c, v in codes.items() if c and c.startswith("5"))
    p99 = p99_of_delta(cp_before, cp_after, "sam_control_plane_http_request_duration_seconds")
    # Both replicas export the same store-derived gauge; take one.
    nodes = tuple(max([v for (n, labels), v in s.items() if n == "sam_control_plane_enrolled_nodes" and dict(labels).get("role") == "mesh:role:node"] or [0])
                  for s in (cp_before, cp_after))
    print(f"control plane: requests by code {codes}; p99 <= {p99} s; enrolled nodes {nodes[0]:.0f} -> {nodes[1]:.0f}")
    check("control plane: no 5xx during the run", errors == 0, f"{errors:.0f} responses")
    if p99 is not None:
        check(f"control plane: request p99 <= {t_cp_p99:g} s", p99 <= t_cp_p99, f"<= {p99} s bucket")

routers = sorted({re.sub(r"-(before|after|resident|rolled)\.prom$", "", os.path.basename(p))
                  for p in glob.glob(os.path.join(out, "sam-router-*.prom"))})
refused_total = 0
for r in routers:
    b, a = parse(os.path.join(out, f"{r}-before.prom")), parse(os.path.join(out, f"{r}-after.prom"))
    if not (b and a):
        continue
    refused = delta_by(b, a, "sam_router_inbound_connections_refused_total", "reason")
    handshakes = delta_by(b, a, "sam_router_auth_handshakes_total", "result")
    rss = (total(b, "process_resident_memory_bytes") / 2**20, total(a, "process_resident_memory_bytes") / 2**20)
    peers = total(a, "sam_router_authenticated_peers")
    refused_total += sum(refused.values())
    print(f"{r}: refused {refused}; handshakes {handshakes}; rss {rss[0]:.0f} -> {rss[1]:.0f} MiB; authenticated peers now {peers:.0f}")
if routers:
    check("routers: no inbound connection refused", refused_total == 0,
          f"{refused_total:.0f} refused (a per-source-IP limit shows here first)")

print()
failed = 0
for name, ok, detail in verdicts:
    print(f"{'PASS' if ok else 'FAIL'}  {name}  ({detail})")
    failed += 0 if ok else 1
print()
print(f"{len(verdicts) - failed} of {len(verdicts)} checks passed; observations under {out}")
sys.exit(1 if failed else 0)
