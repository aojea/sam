#!/usr/bin/env bash
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
#
# What does the Nth member experience?
#
# The density and fleet runs say how many agents one member can carry. This
# run is about members: N sam-node processes join a real mesh from one host,
# each is timed from start to its first call of a service through the mesh,
# the fleet then stays resident while more members join and, if asked, while
# the routers are restarted under it. Every member on this host shares one
# source address, which is what a classroom or an office looks like to the
# router.
#
# Phases, each written as its own observation under --out:
#   burst     N members start at --ramp per second            burst.json
#   late      --late more join the populated mesh             late.json
#   rollout   (--rollout) restart the router StatefulSet,      events.jsonl
#             then --late more join behind it                 late-after-rollout.json
#   hold      the burst fleet stays for --hold, sampled        burst.json (hold section)
#
# Then one table and a verdict against the thresholds below, which the
# environment overrides (T_JOURNEY_P95_MS=20000 ...).
#
# Usage:
#   tests/scale/member-journey.sh --env bananas --count 500 --late 20 --hold 1h --rollout --out /var/log/journey
#   tests/scale/member-journey.sh --control-plane http://127.0.0.1:8080 --bootstrap-token-path ./join-token \
#       --node-arg=--insecure-control-plane --node-arg=--allow-loopback --count 20 --out /tmp/journey
#
# The members call an MCP service named --service, so some node on the mesh
# has to advertise one; on a testnet that is the everything canary. For a
# local mesh, tests/scale/stdio-mcp.py (Python SDK) and tests/scale/stdio-mcp
# (Go SDK) are real servers a provider node can run as a command backend;
# .github/workflows/member-journey.yaml shows the whole local setup.
#
# --env mints the bootstrap token from the control plane's admin secret and
# snapshots the control plane's and routers' metrics around the run through
# the API server. Without it, bring your own token; the verdict then covers
# only what the members themselves saw.

set -euo pipefail

COUNT=50
LATE=10
RAMP=0
HOLD=10m
SERVICE=everything
ENV_NAME=""
CONTROL_PLANE=""
BOOTSTRAP_TOKEN_PATH=""
OUT=""
ROLLOUT=0
NODE_ARGS=()

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SAM_NODE="${SAM_NODE:-${REPO_ROOT}/bin/sam-node}"
SAM_BENCH="${SAM_BENCH:-${REPO_ROOT}/bin/sam-bench}"
# May carry arguments, e.g. KUBECTL="kubectl --context my-cluster".
KUBECTL="${KUBECTL:-kubectl}"

# Thresholds. The journey bound is the SDKs' dial timeout: a member that
# takes longer than that would have failed as an SDK agent.
T_JOURNEY_P50_MS="${T_JOURNEY_P50_MS:-5000}"
T_JOURNEY_P95_MS="${T_JOURNEY_P95_MS:-15000}"
T_OUTAGE_MAX_MS="${T_OUTAGE_MAX_MS:-60000}"
T_CP_P99_S="${T_CP_P99_S:-1}"

usage() {
  sed -n '17,46p' "$0" | sed 's/^# \{0,1\}//'
  cat <<EOF

Options:
  --env NAME                 testnet (hub, bananas): mints the token, snapshots metrics, enables --rollout
  --control-plane URL        control plane to enroll with (default https://NAME.sam-mesh.dev with --env)
  --bootstrap-token-path F   bootstrap token to enroll with; minted from the admin secret with --env if omitted
  --count N                  members in the burst (default ${COUNT})
  --late N                   members that join once the burst is resident, per late phase (default ${LATE}; 0 skips)
  --ramp N                   members started per second (default ${RAMP}: all at once)
  --hold DURATION            how long the burst fleet stays resident (default ${HOLD})
  --rollout                  restart the router StatefulSet while the fleet is resident (needs --env)
  --service NAME             MCP service each member finds and calls (default ${SERVICE})
  --node-arg ARG             extra sam-node argument, repeatable
  --out DIR                  where observations, logs and member state land (required)
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env) ENV_NAME="$2"; shift 2 ;;
    --control-plane) CONTROL_PLANE="$2"; shift 2 ;;
    --bootstrap-token-path) BOOTSTRAP_TOKEN_PATH="$2"; shift 2 ;;
    --count) COUNT="$2"; shift 2 ;;
    --late) LATE="$2"; shift 2 ;;
    --ramp) RAMP="$2"; shift 2 ;;
    --hold) HOLD="$2"; shift 2 ;;
    --rollout) ROLLOUT=1; shift ;;
    --service) SERVICE="$2"; shift 2 ;;
    --node-arg) NODE_ARGS+=("$2"); shift 2 ;;
    --node-arg=*) NODE_ARGS+=("${1#--node-arg=}"); shift ;;
    --out) OUT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown flag: $1" >&2; usage >&2; exit 2 ;;
  esac
done

fail() { echo "member-journey: $*" >&2; exit 1; }
log() { echo "$(date -u +%FT%TZ) $*"; }

[[ -n "$OUT" ]] || fail "--out is required"
[[ -x "$SAM_NODE" ]] || fail "sam-node not found at $SAM_NODE; run make or set SAM_NODE"
[[ -x "$SAM_BENCH" ]] || fail "sam-bench not found at $SAM_BENCH; run make or set SAM_BENCH"
command -v python3 >/dev/null || fail "python3 is needed for the summary"
if [[ -n "$ENV_NAME" ]]; then
  NS="sam-${ENV_NAME}"
  command -v "${KUBECTL%% *}" >/dev/null || fail "kubectl is needed with --env"
  $KUBECTL get namespace "$NS" >/dev/null 2>&1 || fail "namespace $NS is not reachable with the current kubectl context"
  CONTROL_PLANE="${CONTROL_PLANE:-https://${ENV_NAME}.sam-mesh.dev}"
else
  [[ -n "$CONTROL_PLANE" ]] || fail "--control-plane is required without --env"
  [[ -n "$BOOTSTRAP_TOKEN_PATH" ]] || fail "--bootstrap-token-path is required without --env"
  [[ "$ROLLOUT" -eq 0 ]] || fail "--rollout needs --env"
fi

mkdir -p "$OUT"
for phase in burst late late-after-rollout; do
  [[ -e "$OUT/$phase" ]] && fail "$OUT/$phase exists; a member directory with an identity would resume rather than join. Use a fresh --out"
done

# Every member is a process with its own sockets and a libp2p host. The soft
# limit is per process, so this only has to cover one node, but it is often 1024.
ulimit -n 65536 2>/dev/null || log "could not raise the file descriptor limit; a member may run out"

JOIN_PID=""
PF_PID=""
TOKEN_ID=""
SUMMARIZED=0
cleanup() {
  local status=$?
  if [[ -n "$JOIN_PID" ]] && kill -0 "$JOIN_PID" 2>/dev/null; then
    # SIGINT ends the hold; the run still writes its report and stops its members.
    kill -INT "$JOIN_PID" 2>/dev/null || true
    wait "$JOIN_PID" 2>/dev/null || true
  fi
  JOIN_PID=""
  [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null || true
  PF_PID=""
  if [[ "$SUMMARIZED" -eq 0 && -f "$OUT/burst.json" ]]; then
    summarize || true
  fi
  [[ -n "$TOKEN_ID" ]] && revoke_token
  rm -f "$OUT/admin-header"
  exit "$status"
}
trap cleanup EXIT

# admin_api runs curl against the control plane's admin API over a
# port-forward, with the admin token in a header file rather than on a
# command line. Arguments are curl's, after the method and path.
admin_api() {
  local method="$1" path="$2"
  shift 2
  local port
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
  if [[ ! -s "$OUT/admin-header" ]]; then
    $KUBECTL get secret "sam-control-plane-secret-${ENV_NAME}" -n "$NS" -o jsonpath='{.data.admin-token}' \
      | base64 -d | sed 's/^/Authorization: Bearer /' > "$OUT/admin-header"
    chmod 600 "$OUT/admin-header"
  fi
  $KUBECTL port-forward "deployment/sam-control-plane-${ENV_NAME}" -n "$NS" "${port}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    curl -fsS -o /dev/null "http://127.0.0.1:${port}/healthz" 2>/dev/null && break
    sleep 0.5
  done
  local status=0
  curl -fsS -X "$method" -H "@${OUT}/admin-header" "$@" "http://127.0.0.1:${port}${path}" || status=$?
  kill "$PF_PID" 2>/dev/null || true
  PF_PID=""
  return "$status"
}

# mint_token writes a bootstrap token with enough usages for every phase and
# remembers its id so the run can revoke it on the way out.
mint_token() {
  local uses=$(( COUNT + 2 * LATE + 5 ))
  admin_api POST /admin/bootstrap-tokens -H 'Content-Type: application/json' \
    -d "{\"role\":\"sam:role:node\",\"max_usages\":${uses},\"ttl_hours\":48,\"description\":\"member-journey ${COUNT}+${LATE} from $(hostname)\"}" \
    > "$OUT/token.json" || fail "minting a bootstrap token failed"
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token"])' "$OUT/token.json" > "$OUT/bootstrap-token"
  TOKEN_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$OUT/token.json")
  rm -f "$OUT/token.json"
  chmod 600 "$OUT/bootstrap-token"
  [[ -s "$OUT/bootstrap-token" && -n "$TOKEN_ID" ]] || fail "no bootstrap token came back"
  BOOTSTRAP_TOKEN_PATH="$OUT/bootstrap-token"
  log "minted bootstrap token ${TOKEN_ID} for ${uses} enrollments"
}

# revoke_token retires the token a run minted: a test must not leave a
# standing credential on a public testnet.
revoke_token() {
  if admin_api DELETE "/admin/bootstrap-tokens/${TOKEN_ID}" -o /dev/null; then
    log "revoked bootstrap token ${TOKEN_ID}"
  else
    log "could not revoke bootstrap token ${TOKEN_ID}; revoke it by hand"
  fi
  TOKEN_ID=""
  rm -f "$OUT/bootstrap-token"
}

# snapshot reads what every control plane and router pod exports, through
# the API server so no port is opened for it. Per pod, not per Service: the
# control plane has two replicas and a Service proxy alternates between
# them, which would make a before/after difference meaningless. The
# members' own numbers say what they experienced; these say what the mesh
# did to produce it.
snapshot() {
  local tag="$1" pod name
  [[ -n "$ENV_NAME" ]] || return 0
  for pod in $($KUBECTL get pods -n "$NS" -l "app=sam-control-plane-${ENV_NAME}" -o name 2>/dev/null); do
    name="${pod#pod/}"
    $KUBECTL get --raw "/api/v1/namespaces/${NS}/pods/${name}:8080/proxy/metrics" \
      > "$OUT/${name}-${tag}.prom" 2>/dev/null || log "could not read ${name} metrics (${tag})"
  done
  for pod in $($KUBECTL get pods -n "$NS" -l "app=sam-router-${ENV_NAME}" -o name 2>/dev/null); do
    name="${pod#pod/}"
    $KUBECTL get --raw "/api/v1/namespaces/${NS}/pods/${name}:9090/proxy/metrics" \
      > "$OUT/${name}-${tag}.prom" 2>/dev/null || log "could not read ${name} metrics (${tag})"
  done
}

event() {
  printf '{"event":"%s","at":"%s"}\n' "$1" "$(date -u +%FT%TZ)" >> "$OUT/events.jsonl"
}

# summarize closes the run: a last snapshot, then the table and verdict. It
# runs from the exit trap too, so an interrupted hold still gets its report.
summarize() {
  SUMMARIZED=1
  event hold-end
  snapshot after
  python3 "${REPO_ROOT}/tests/scale/member-journey-summary.py" "$OUT" \
    "$T_JOURNEY_P50_MS" "$T_JOURNEY_P95_MS" "$T_OUTAGE_MAX_MS" "$T_CP_P99_S"
}

# join_cmd fills JOIN_CMD with one phase's sam-bench invocation. It is an
# array rather than a function so the burst can be backgrounded directly:
# a backgrounded function is a subshell, and a signal to it would not reach
# the run inside.
JOIN_CMD=()
join_cmd() { # phase count base-port extra...
  local phase="$1" count="$2" base="$3" a
  shift 3
  JOIN_CMD=("$SAM_BENCH" join
    --node-bin "$SAM_NODE"
    --control-plane "$CONTROL_PLANE"
    --bootstrap-token-path "$BOOTSTRAP_TOKEN_PATH"
    --service "$SERVICE"
    --count "$count"
    --metrics-base-port "$base"
    --dir "$OUT/$phase"
    --label "phase=$phase" --label "count=$count" --label "burst=$COUNT" --label "host=$(hostname)"
    --out "$OUT/$phase.json")
  for a in "${NODE_ARGS[@]}"; do JOIN_CMD+=("--node-arg=$a"); done
  JOIN_CMD+=("$@")
}

[[ -n "$BOOTSTRAP_TOKEN_PATH" ]] || mint_token
rm -f "$OUT/resident"
: > "$OUT/events.jsonl"
{
  echo "control_plane=$CONTROL_PLANE"
  echo "host=$(hostname) nproc=$(nproc) mem_kib=$(awk '/MemTotal/{print $2}' /proc/meminfo)"
  echo "sam_node=$("$SAM_NODE" --version 2>/dev/null | head -1)"
} > "$OUT/environment.txt"

snapshot before
event burst-start
log "burst: ${COUNT} members at ${RAMP}/s, then holding ${HOLD}"
join_cmd burst "$COUNT" 20000 --ramp "$RAMP" --hold "$HOLD" --resident-marker "$OUT/resident"
"${JOIN_CMD[@]}" 2> "$OUT/burst.log" &
JOIN_PID=$!
while [[ ! -f "$OUT/resident" ]]; do
  kill -0 "$JOIN_PID" 2>/dev/null || fail "the burst ended before the fleet was resident; see $OUT/burst.log"
  sleep 2
done
event burst-resident
snapshot resident
python3 - "$OUT/burst.json" <<'PY'
import json, sys
j = json.load(open(sys.argv[1]))["join"]
print(f"burst: {j['ready']}/{j['count']} ready, {j['completed']} completed, {j['failed']} failed; "
      f"journey p50 {j['journey_ms']['p50']:.0f} ms, p95 {j['journey_ms']['p95']:.0f} ms, max {j['journey_ms']['max']:.0f} ms")
for stage, n in sorted(j.get("errors", {}).items()):
    print(f"  {n} x {stage}")
PY

if (( LATE > 0 )); then
  event late-start
  log "late: ${LATE} members join the populated mesh"
  join_cmd late "$LATE" 30000
  "${JOIN_CMD[@]}" 2> "$OUT/late.log" || log "late phase did not run cleanly; see $OUT/late.log"
  event late-end
fi

if (( ROLLOUT )); then
  event rollout-start
  log "rollout: restarting statefulset/sam-router-${ENV_NAME} under the resident fleet"
  $KUBECTL rollout restart "statefulset/sam-router-${ENV_NAME}" -n "$NS"
  $KUBECTL rollout status "statefulset/sam-router-${ENV_NAME}" -n "$NS" --timeout=600s || log "rollout did not finish in 600s"
  event rollout-end
  snapshot rolled
  if (( LATE > 0 )); then
    # The routers' addresses just changed and the DNS records catch up on a
    # timer; a long bound here measures that wait instead of hiding it.
    event late-after-rollout-start
    log "late: ${LATE} members join behind the rollout"
    join_cmd late-after-rollout "$LATE" 31000 --ready-timeout 600s
    "${JOIN_CMD[@]}" 2> "$OUT/late-after-rollout.log" \
      || log "late-after-rollout phase did not run cleanly; see $OUT/late-after-rollout.log"
    event late-after-rollout-end
  fi
fi

log "holding; the burst fleet stays until ${HOLD} after it became resident (Ctrl-C ends it early with a report)"
wait "$JOIN_PID" || log "burst run exited non-zero; see $OUT/burst.log"
JOIN_PID=""
summarize
