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
# How many members does a testnet hold?
#
# member-journey.sh asks what the Nth member experiences from one host. This
# climbs: minions join the mesh in steps and every fleet stays resident, so
# each step lands on a mesh already carrying the ones before it. One minion
# carries one address and, by the router's per-address cap, about 500
# members; the ladder is therefore in minions, and a step of two minions is
# a thousand members more.
#
# Each minion runs `sam-bench join` with a long --hold; the join report it
# writes the moment its fleet is resident is pulled back here, so a step is
# judged while its members stay. The control plane and every router are read
# before and after each step, through the API server. At the end, or on
# Ctrl-C, every minion's run is interrupted so its hold report is written,
# and those are pulled back too.
#
# Usage:
#   tests/scale/member-ladder.sh --env bananas --per-minion 500 \
#       --steps "sam-ladder-01" "sam-ladder-02" "sam-ladder-03 sam-ladder-04 sam-ladder-05" \
#               "sam-ladder-06 sam-ladder-07 sam-ladder-08 sam-ladder-09 sam-ladder-10" \
#       --out /tmp/ladder
#
# The minions need sam-node and sam-bench on their PATH and a bootstrap token
# at --remote-token-path; the token is minted here with enough usages for the
# whole ladder and revoked on exit, as member-journey.sh does. The minions are
# reached with gcloud over IAP in $ZONE (default us-central1-a) and $PROJECT
# (default: the gcloud configuration's project); $KUBECTL and $GCLOUD may
# carry arguments such as a kubectl context.

set -euo pipefail

ENV_NAME=""
PER_MINION=500
RAMP=20
HOLD=6h
SERVICE=everything
OUT=""
ZONE="${ZONE:-us-central1-a}"
PROJECT="${PROJECT:-}"
REMOTE_TOKEN_PATH=/var/lib/ladder/bootstrap-token
STEPS=()
KUBECTL="${KUBECTL:-kubectl}"
GCLOUD="${GCLOUD:-gcloud}"
SSH_OPTS=(--zone "$ZONE" --tunnel-through-iap --quiet)
if [[ -n "$PROJECT" ]]; then SSH_OPTS+=(--project "$PROJECT"); fi

usage() { sed -n '17,44p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env) ENV_NAME="$2"; shift 2 ;;
    --per-minion) PER_MINION="$2"; shift 2 ;;
    --ramp) RAMP="$2"; shift 2 ;;
    --hold) HOLD="$2"; shift 2 ;;
    --service) SERVICE="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --steps) shift; while [[ $# -gt 0 && "$1" != --* ]]; do STEPS+=("$1"); shift; done ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown flag: $1" >&2; usage >&2; exit 2 ;;
  esac
done

fail() { echo "member-ladder: $*" >&2; exit 1; }
log() { echo "$(date -u +%FT%TZ) $*"; }
[[ -n "$ENV_NAME" && -n "$OUT" && ${#STEPS[@]} -gt 0 ]] || fail "--env, --out and --steps are required"
NS="sam-${ENV_NAME}"
CONTROL_PLANE="https://${ENV_NAME}.sam-mesh.dev"
mkdir -p "$OUT"
ALL_MINIONS=()
for s in "${STEPS[@]}"; do for m in $s; do ALL_MINIONS+=("$m"); done; done
TOTAL=$(( ${#ALL_MINIONS[@]} * PER_MINION ))

rssh() { # minion cmd...
  local m="$1"; shift
  $GCLOUD compute ssh "$m" "${SSH_OPTS[@]}" --command "$*" 2>/dev/null
}
rscp_to() { $GCLOUD compute scp "${SSH_OPTS[@]}" "$1" "$2:$3" 2>/dev/null; }
rscp_from() { $GCLOUD compute scp "${SSH_OPTS[@]}" "$1:$2" "$3" 2>/dev/null; }

PF_PID=""
TOKEN_ID=""
admin_api() {
  local method="$1" path="$2"; shift 2
  local port
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
  if [[ ! -s "$OUT/admin-header" ]]; then
    $KUBECTL get secret "sam-control-plane-secret-${ENV_NAME}" -n "$NS" -o json \
      | python3 -c 'import json,sys,base64,os; d=json.load(sys.stdin)["data"]["admin-token"]; p=sys.argv[1]; open(p,"w").write("Authorization: Bearer "+base64.b64decode(d).decode()+"\n"); os.chmod(p,0o600)' "$OUT/admin-header"
  fi
  $KUBECTL port-forward "deployment/sam-control-plane-${ENV_NAME}" -n "$NS" "${port}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do curl -fsS -o /dev/null "http://127.0.0.1:${port}/healthz" 2>/dev/null && break; sleep 0.5; done
  local status=0
  curl -fsS -X "$method" -H "@${OUT}/admin-header" "$@" "http://127.0.0.1:${port}${path}" || status=$?
  kill "$PF_PID" 2>/dev/null || true; PF_PID=""
  return "$status"
}

snapshot() {
  local tag="$1" pod name
  for pod in $($KUBECTL get pods -n "$NS" -l "app=sam-control-plane-${ENV_NAME}" -o name 2>/dev/null); do
    name="${pod#pod/}"
    $KUBECTL get --raw "/api/v1/namespaces/${NS}/pods/${name}:8080/proxy/metrics" > "$OUT/${name}-${tag}.prom" 2>/dev/null || log "could not read ${name} (${tag})"
  done
  for pod in $($KUBECTL get pods -n "$NS" -l "app=sam-router-${ENV_NAME}" -o name 2>/dev/null); do
    name="${pod#pod/}"
    $KUBECTL get --raw "/api/v1/namespaces/${NS}/pods/${name}:9090/proxy/metrics" > "$OUT/${name}-${tag}.prom" 2>/dev/null || log "could not read ${name} (${tag})"
  done
}

STARTED_MINIONS=()
collect_holds() {
  local m
  for m in "${STARTED_MINIONS[@]}"; do
    rssh "$m" "pkill -INT -x sam-bench || true" || true
  done
  for m in "${STARTED_MINIONS[@]}"; do
    for _ in $(seq 1 60); do
      rssh "$m" "pgrep -x sam-bench >/dev/null" || break
      sleep 2
    done
    rscp_from "$m" "/var/log/ladder/${m}.json" "$OUT/${m}.json" || log "no final report from $m"
    rscp_from "$m" "/var/log/ladder/${m}.log" "$OUT/${m}.log" || true
  done
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ ${#STARTED_MINIONS[@]} -gt 0 ]]; then
    log "interrupting the resident fleets for their hold reports"
    collect_holds
    snapshot final
    summarize || true
  fi
  [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null || true
  if [[ -n "$TOKEN_ID" ]]; then
    admin_api DELETE "/admin/bootstrap-tokens/${TOKEN_ID}" -o /dev/null && log "revoked bootstrap token ${TOKEN_ID}" || log "could not revoke token ${TOKEN_ID}; revoke it by hand"
  fi
  rm -f "$OUT/admin-header" "$OUT/bootstrap-token"
  exit "$status"
}
trap cleanup EXIT INT TERM

summarize() {
  python3 "$(dirname "$0")/member-ladder-summary.py" "$OUT" "${STEPS[@]}"
}

# One token for the whole ladder.
uses=$(( TOTAL + TOTAL / 10 + 20 ))
admin_api POST /admin/bootstrap-tokens -H 'Content-Type: application/json' \
  -d "{\"role\":\"sam:role:node\",\"max_usages\":${uses},\"ttl_hours\":48,\"description\":\"member-ladder ${TOTAL} from ${#ALL_MINIONS[@]} minions\"}" > "$OUT/token.json" || fail "minting the bootstrap token failed"
python3 -c 'import json,sys; t=json.load(open(sys.argv[1])); open(sys.argv[2],"w").write(t["token"])' "$OUT/token.json" "$OUT/bootstrap-token"
TOKEN_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$OUT/token.json")
rm -f "$OUT/token.json"; chmod 600 "$OUT/bootstrap-token"
log "minted bootstrap token ${TOKEN_ID} for ${uses} enrollments"

log "placing the token on ${#ALL_MINIONS[@]} minions"
for m in "${ALL_MINIONS[@]}"; do
  rssh "$m" "test -x /usr/local/bin/sam-bench && test -x /usr/local/bin/sam-node" || fail "$m has no binaries yet (startup script still running?)"
  rscp_to "$OUT/bootstrap-token" "$m" /tmp/bootstrap-token
  rssh "$m" "sudo install -m 0644 /tmp/bootstrap-token ${REMOTE_TOKEN_PATH} && rm -f /tmp/bootstrap-token" || fail "could not place the token on $m"
done

: > "$OUT/events.jsonl"
event() { printf '{"event":"%s","at":"%s"}\n' "$1" "$(date -u +%FT%TZ)" >> "$OUT/events.jsonl"; }

resident=0
step_no=0
for step in "${STEPS[@]}"; do
  step_no=$((step_no + 1))
  read -r -a minions <<< "$step"
  target=$(( resident + ${#minions[@]} * PER_MINION ))
  log "step ${step_no}: ${#minions[@]} minion(s) x ${PER_MINION} join a mesh holding ${resident} -> ${target}"
  snapshot "step${step_no}-before"
  event "step${step_no}-start"
  for m in "${minions[@]}"; do
    # The run detaches from the ssh session; its stderr is the progress log.
    rssh "$m" "ulimit -n 1048576; rm -rf /var/lib/ladder/${m} /var/log/ladder/${m}.json /var/log/ladder/resident; nohup sam-bench join --node-bin /usr/local/bin/sam-node --control-plane ${CONTROL_PLANE} --bootstrap-token-path ${REMOTE_TOKEN_PATH} --service ${SERVICE} --count ${PER_MINION} --ramp ${RAMP} --hold ${HOLD} --dir /var/lib/ladder/${m} --label minion=${m} --label step=${step_no} --label resident_before=${resident} --resident-marker /var/log/ladder/resident --out /var/log/ladder/${m}.json > /var/log/ladder/${m}.log 2>&1 < /dev/null &" || fail "could not start the run on $m"
    STARTED_MINIONS+=("$m")
  done
  # Resident means every member's journey has ended, one way or the other.
  deadline=$(( $(date +%s) + 900 ))
  for m in "${minions[@]}"; do
    until rssh "$m" "test -f /var/log/ladder/resident"; do
      [[ $(date +%s) -lt $deadline ]] || fail "$m was not resident within 15 minutes; see /var/log/ladder/${m}.log on it"
      rssh "$m" "pgrep -x sam-bench >/dev/null" || fail "the run on $m ended before its fleet was resident"
      sleep 10
    done
    rscp_from "$m" "/var/log/ladder/${m}.json" "$OUT/step${step_no}-${m}.json" || fail "could not fetch the join report from $m"
  done
  event "step${step_no}-resident"
  # Let the mesh settle on the new population before it is read.
  sleep 60
  snapshot "step${step_no}-after"
  resident=$target
  summarize || true
  log "step ${step_no} done: ${resident} members resident"
done

log "ladder complete: ${resident} members resident; holding until Ctrl-C or ${HOLD}"
# Stay until the first minion's hold ends or the operator interrupts.
while rssh "${STARTED_MINIONS[0]}" "pgrep -x sam-bench >/dev/null"; do sleep 60; done
