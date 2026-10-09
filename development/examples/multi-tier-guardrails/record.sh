#!/bin/bash
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

# Records SCRIPT.md for the Multi-Tier Guardrails & Quality Gates example.
# Every config file (node-v1.yaml, node-caller.yaml, node-v2.yaml,
# node-contractor.yaml, policy.json) and every command (sam-node join,
# make node-v1-staging, curl, python3 a2a_client.py, mcp-client,
# sam-one admin ban) is typed directly in the panes — no hidden wrapper
# functions.

set -o errexit
set -o nounset
set -o pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$HERE/../../.." && pwd)
OUT=$HERE/out
SESSION=samguardrails
COLS=${COLS:-190}
ROWS=${ROWS:-54}
SPEED=${SPEED:-1.0}
IDLE_MAX=${IDLE_MAX:-3.0}
CAPTION_CPS=${CAPTION_CPS:-22}
CAPTION_FILE=/tmp/samguardrails-caption.txt
CAPTIONS=$OUT/captions.tsv
SRT=$OUT/demo.srt
WORK_DIR=/tmp/sam-record-guardrails
VENV_DIR=${VENV_DIR:-/tmp/sam-multi-tier-venv}
PYTHON=$VENV_DIR/bin/python
export ORDERS_DB_PATH=$WORK_DIR/orders.db
export LLM_MODEL=${LLM_MODEL:-gemma3:1b}

SAM_ONE=$REPO_ROOT/bin/sam-one
SAM_NODE=$REPO_ROOT/bin/sam-node
MCP_CLIENT=$REPO_ROOT/bin/mcp-client

if [[ ! -x "$SAM_ONE" || ! -x "$SAM_NODE" || ! -x "$MCP_CLIENT" ]]; then
  make -C "$REPO_ROOT" build
fi

if [[ ! -x "$PYTHON" ]]; then
  python3 -m venv "$VENV_DIR"
fi
"$PYTHON" -c "import a2a.server, a2a.client, mcp.server.mcpserver" 2>/dev/null || \
  "$VENV_DIR/bin/pip" install -q -r "$HERE/requirements.txt"

if ! curl -sf http://127.0.0.1:11434/v1/models >/dev/null 2>&1; then
  docker start sam-demo-ollama >/dev/null 2>&1 || \
    docker run -d --name sam-demo-ollama -p 127.0.0.1:11434:11434 -v sam-demo-ollama:/root/.ollama ollama/ollama >/dev/null
  for _ in $(seq 1 30); do curl -sf http://127.0.0.1:11434/ >/dev/null && break; sleep 1; done
  docker exec sam-demo-ollama ollama pull "$LLM_MODEL" >/dev/null
fi

mkdir -p "$OUT"
rm -f "$SRT" "$CAPTIONS"
: > "$CAPTION_FILE"

type_in() {
  local pane=$1 text=$2 i c
  for ((i = 0; i < ${#text}; i++)); do
    c=${text:i:1}
    tmux send-keys -t "$pane" -l -- "$c"
    sleep "0.0$((RANDOM % 4 + 2))"
  done
  sleep 0.4
  tmux send-keys -t "$pane" Enter
  sleep 0.7
}

wait_for() {
  local pane=$1 re=$2 timeout=${3:-60} i
  for ((i = 0; i < timeout * 2; i++)); do
    if tmux capture-pane -J -p -t "$pane" -S -300 | grep -qE -- "$re"; then
      return 0
    fi
    sleep 0.5
  done
  echo "timed out waiting for /$re/ in $pane" >&2
  tmux capture-pane -J -p -t "$pane" -S -40 >&2
  return 1
}

count() {
  tmux capture-pane -J -p -t "$1" -S -300 | grep -cE -- "$2" || true
}

wait_prompt() {
  local pane=$1 timeout=${2:-30} i
  for ((i = 0; i < timeout * 4; i++)); do
    if tmux capture-pane -p -t "$pane" | sed -e :a -e '/^\s*$/{$d;N;ba' -e '}' | tail -1 | grep -qE '\$ ?$'; then
      return 0
    fi
    sleep 0.25
  done
  return 0
}

expect() {
  local pane=$1 cmd=$2 re=$3 timeout=${4:-60} before i
  before=$(count "$pane" "$re")
  type_in "$pane" "$cmd"
  for ((i = 0; i < timeout * 2; i++)); do
    if (( $(count "$pane" "$re") > before )); then
      wait_prompt "$pane"
      return 0
    fi
    sleep 0.5
  done
  echo "timed out waiting for new /$re/ in $pane after: $cmd" >&2
  tmux capture-pane -J -p -t "$pane" -S -40 >&2
  return 1
}

caption() {
  printf '%s\t%s\n' "$(date +%s.%N)" "$1" >> "$CAPTIONS"
  printf '%s\n' "$1" > "$CAPTION_FILE"
  sleep "$(python3 -c 'import sys; print(len(sys.argv[1]) / float(sys.argv[2]) + 1.5)' "$1" "$CAPTION_CPS")"
}

pause() { sleep "${1:-2.5}"; }

cleanup() {
  tmux kill-session -t "$SESSION" 2>/dev/null || true
  tmux kill-session -t "${SESSION}-rec" 2>/dev/null || true
  if [[ -d "$WORK_DIR" ]]; then
    for pidfile in "$WORK_DIR"/*.pid; do
      [[ -f "$pidfile" ]] && kill -9 "$(cat "$pidfile")" 2>/dev/null || true
    done
    rm -rf "$WORK_DIR"
  fi
}
trap cleanup EXIT

cleanup
mkdir -p "$WORK_DIR/secrets"
if [[ -n "${GITHUB_TOKEN_FILE:-}" && -s "${GITHUB_TOKEN_FILE}" ]]; then
  install -m 600 "${GITHUB_TOKEN_FILE}" "$WORK_DIR/secrets/github-ro"
elif command -v gh >/dev/null 2>&1 && gh auth token >/dev/null 2>&1; then
  gh auth token > "$WORK_DIR/secrets/github-ro"
  chmod 600 "$WORK_DIR/secrets/github-ro"
fi

echo "caller-secret" > "$WORK_DIR/caller-api.token"
echo "dev-secret" > "$WORK_DIR/dev-api.token"
echo "dev2-secret" > "$WORK_DIR/dev2-api.token"
echo "contractor-secret" > "$WORK_DIR/contractor-api.token"
chmod 600 "$WORK_DIR"/*.token
printf 'header = "X-Sam-Authentication: Bearer caller-secret"\n' > "$WORK_DIR/.curlrc"

# Start real SQLite MCP server (orders_mcp.py) and LLM-backed A2A agents (support_agent.py)
"$PYTHON" "$HERE/orders_mcp.py" > "$WORK_DIR/orders-mcp.log" 2>&1 &
echo $! > "$WORK_DIR/orders-mcp.pid"

"$PYTHON" "$HERE/support_agent.py" v1-laptop 17771 > "$WORK_DIR/a2a-v1.log" 2>&1 &
echo $! > "$WORK_DIR/a2a-v1.pid"

"$PYTHON" "$HERE/support_agent.py" v2-cloudrun 17772 > "$WORK_DIR/a2a-v2.log" 2>&1 &
echo $! > "$WORK_DIR/a2a-v2.pid"

for _ in $(seq 1 120); do
  if curl -sf http://127.0.0.1:17771/.well-known/agent-card.json >/dev/null && \
     curl -sf http://127.0.0.1:17772/.well-known/agent-card.json >/dev/null; then
    break
  fi
  sleep 0.25
done

# Start sam-one control plane + router in background
CP_URL="http://127.0.0.1:18190"
"$SAM_ONE" \
  --data-dir "$WORK_DIR/one" \
  --port 18190 \
  --policy-file "$HERE/policy.json" \
  --no-join-token \
  --enroll-qr=false \
  --log-level warn > "$WORK_DIR/sam-one.log" 2>&1 &
echo $! > "$WORK_DIR/sam-one.pid"

for _ in $(seq 1 80); do
  if [[ -s "$WORK_DIR/one/admin-token" ]] && curl -sf "$CP_URL/readyz" >/dev/null; then
    break
  fi
  sleep 0.25
done
ADMIN_TOKEN=$(cat "$WORK_DIR/one/admin-token")

mint_tok() {
  "$SAM_ONE" token create --server "$CP_URL" --data-dir "$WORK_DIR/one" \
    --max-usages 1 --description "$1" 2>/dev/null | awk '/^Token:/ {print $2}' > "$2"
  chmod 600 "$2"
}
mint_tok "caller-input-node" "$WORK_DIR/caller.token"
mint_tok "dev-spoof" "$WORK_DIR/dev-spoof.token"
mint_tok "dev-v1-staging" "$WORK_DIR/dev-v1.token"
mint_tok "dev-v1-prod" "$WORK_DIR/dev-v1-prod.token"
mint_tok "cloudrun-v2-prod" "$WORK_DIR/dev-v2.token"
mint_tok "contractor-node" "$WORK_DIR/contractor.token"

# Start Input / Caller Node with node-caller.yaml (egress.require_labels: {env: prod})
"$SAM_NODE" run \
  --control-plane "$CP_URL" --insecure-control-plane \
  --bootstrap-token-path "$WORK_DIR/caller.token" \
  --config "$HERE/node-caller.yaml" \
  --data-dir "$WORK_DIR/node-caller" \
  --bind-addr 127.0.0.1:19102 --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "$WORK_DIR/caller-api.token" \
  --allow-loopback --control-plane-sync-interval 5s > "$WORK_DIR/node-caller.log" 2>&1 &
echo $! > "$WORK_DIR/node-caller.pid"

# Start Contractor Node with node-contractor.yaml (labels: {team: contractor})
"$SAM_NODE" run \
  --control-plane "$CP_URL" --insecure-control-plane \
  --bootstrap-token-path "$WORK_DIR/contractor.token" \
  --config "$HERE/node-contractor.yaml" \
  --data-dir "$WORK_DIR/node-contractor" \
  --bind-addr 127.0.0.1:19104 --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "$WORK_DIR/contractor-api.token" \
  --allow-loopback --control-plane-sync-interval 5s > "$WORK_DIR/node-contractor.log" 2>&1 &
echo $! > "$WORK_DIR/node-contractor.pid"

for _ in $(seq 1 40); do
  if grep -q "SAM Node Online" "$WORK_DIR/node-caller.log" 2>/dev/null && \
     grep -q "SAM Node Online" "$WORK_DIR/node-contractor.log" 2>/dev/null; then
    break
  fi
  sleep 0.25
done
CONTRACTOR_PEER=$(awk '/PeerID:/ {print $2; exit}' "$WORK_DIR/node-contractor.log")

# --- tmux window ------------------------------------------------------------

tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" -c "$HERE"
tmux set -t "$SESSION" -g pane-border-status top
tmux set -t "$SESSION" -g pane-border-format ' #{pane_title} '
tmux set -t "$SESSION" -g status off

NODE=$(tmux display -p -t "$SESSION:0.0" '#{pane_id}')
CAPTION=$(tmux split-window -P -F '#{pane_id}' -v -l 4 -t "$NODE" -c "$HERE")
CALLER=$(tmux split-window -P -F '#{pane_id}' -h -l 54% -t "$NODE" -c "$HERE")
ADMIN=$(tmux split-window -P -F '#{pane_id}' -v -l 18 -t "$NODE" -c "$HERE")

tmux select-pane -t "$NODE" -T 'support node (Alice · node-v1.yaml · gemma3:1b A2A + SQLite MCP + GitHub egress)'
tmux select-pane -t "$ADMIN" -T 'platform & security admin · policy.json & quality gate'
tmux select-pane -t "$CALLER" -T 'input node & caller (node-caller.yaml · curl · a2a_client.py · mcp-client)'
tmux select-pane -t "$CAPTION" -T ''

cat > /tmp/samguardrails-caption.py <<'EOF'
import sys, textwrap, time
path, cols, cps = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
last = None
while True:
    try:
        text = open(path).read().strip()
    except FileNotFoundError:
        text = ""
    if text != last:
        last = text
        sys.stdout.write("\033[2J\033[H")
        for line in textwrap.wrap(text, cols - 4)[:3]:
            sys.stdout.write("  \033[1m")
            for ch in line:
                sys.stdout.write(ch)
                sys.stdout.flush()
                time.sleep(1 / cps)
            sys.stdout.write("\033[0m\n")
        sys.stdout.flush()
    time.sleep(0.1)
EOF
tmux send-keys -t "$CAPTION" "python3 /tmp/samguardrails-caption.py $CAPTION_FILE $COLS $CAPTION_CPS" Enter

ENV_SETUP="export PATH=$REPO_ROOT/bin:$VENV_DIR/bin:\$PATH CP_URL=$CP_URL ADMIN_TOKEN=$ADMIN_TOKEN WORK_DIR=$WORK_DIR NODE=http://127.0.0.1:19102 SAM_API_TOKEN=caller-secret CONTRACTOR_PEER=$CONTRACTOR_PEER"
tmux send-keys -t "$NODE" "$ENV_SETUP; PS1='node-v1\$ '; clear" Enter
tmux send-keys -t "$ADMIN" "$ENV_SETUP; PS1='admin\$ '; clear" Enter
tmux send-keys -t "$CALLER" "$ENV_SETUP CURL_HOME=$WORK_DIR; PS1='caller\$ '; clear" Enter
wait_for "$NODE" 'node-v1\$' 10
wait_for "$ADMIN" 'admin\$' 10
wait_for "$CALLER" 'caller\$' 10

# --- record -----------------------------------------------------------------

tmux new-session -d -s "${SESSION}-rec" -x "$COLS" -y "$ROWS" \
  "asciinema rec -q --overwrite --cols $COLS --rows $ROWS -c 'tmux attach -t $SESSION' '$OUT/demo.cast'"
sleep 2

caption "Everyone is building AI agents, and every team wants guardrails at its own layer: Developer, Platform/Networking, Central Security, Department Leads, Individual End-Users, and Day-2 Ops."
pause 2.5

# --- Act 1: Developer Onboarding & Label Points 1 and 2a --------------------

caption "Act 1 (Developer Onboarding): Alice writes node-v1.yaml to share her gemma3:1b A2A agent (a2a://support.acme) and SQLite MCP server (mcp://orders-db) in staging (env=staging)."
expect "$NODE" "cat node-v1.yaml" 'attenuation:' 15
pause 2.5

caption "Why can't Alice just put 'env: prod' in node-v1.yaml? Point 1 is Mesh Enforcement (policy.json allowed_labels): the Control Plane rejects ungranted labels at enrollment."
expect "$ADMIN" "jq -c '.roles[0].allowed_labels' policy.json" 'env=staging' 15
expect "$ADMIN" "sed 's/staging/prod/' node-v1.yaml > \$WORK_DIR/node-v1-prod.yaml && sam-node join \$CP_URL --insecure-control-plane --bootstrap-token-path \$WORK_DIR/dev-spoof.token --config \$WORK_DIR/node-v1-prod.yaml --data-dir \$WORK_DIR/spoof" 'Label not permitted' 30
pause 3

caption "Alice starts her node with node-v1.yaml in env=staging. Meanwhile, on the caller side (Point 2a — Input Node Enforcement), node-caller.yaml requires egress.require_labels: env=prod."
type_in "$NODE" "make node-v1-staging 2>&1 | python3 audit.py"
wait_for "$NODE" 'SAM Node Online' 30
V1_STAGING=$(tmux capture-pane -J -p -t "$NODE" -S -50 | awk '/PeerID:/ {print $2; exit}')
tmux send-keys -t "$CALLER" "V1_PEER=$V1_STAGING; clear" Enter
wait_for "$CALLER" 'caller\$' 10
expect "$CALLER" "cat node-caller.yaml" 'env: prod' 15
expect "$CALLER" "curl -i -s \$NODE/sam/\$V1_PEER/a2a/support.acme/.well-known/agent-card.json | head -n 5" '403 Forbidden' 30
pause 3

caption "Now the Automated Quality Gate validates Alice's A2A 1.0 AgentCard and updates policy.json to allow env=prod. Alice restarts in env=prod, and the exact same call returns 200 OK."
expect "$ADMIN" "jq '.roles[0].allowed_labels += [\"env=prod\"]' policy.json | curl -s -X POST \$CP_URL/policies -H \"Authorization: Bearer \$ADMIN_TOKEN\" -H 'Content-Type: application/json' --data @-" 'success' 30
pkill -f "127.0.0.1:19101" 2>/dev/null || true
sleep 0.5
rm -rf "$WORK_DIR/node-v1"
wait_prompt "$NODE"
type_in "$NODE" "make node-v1-prod 2>&1 | python3 audit.py"
wait_for "$NODE" 'SAM Node Online' 30
V1_PROD=$(tmux capture-pane -J -p -t "$NODE" -S -30 | awk '/PeerID:/ {p=$2} END {print p}')
tmux send-keys -t "$CALLER" "V1_PEER=$V1_PROD" Enter
wait_for "$CALLER" 'caller\$' 10
for _ in $(seq 1 20); do
  curl -sf -H "X-Sam-Authentication: Bearer caller-secret" \
    "http://127.0.0.1:19102/sam/$V1_PROD/a2a/support.acme/.well-known/agent-card.json" >/dev/null 2>&1 && break
  sleep 0.5
done
expect "$CALLER" "curl -s \$NODE/sam/\$V1_PEER/a2a/support.acme/.well-known/agent-card.json | jq '{name, url: .supportedInterfaces[0].url}'" 'Acme Support' 30
pause 3

# --- Act 2: Zero-Downtime Replica Swap & Label Point 3 (Agent Intent) -------

caption "Act 2 (Platform / Networking & Point 3 — Agent Intent): Within the Input Node's env=prod floor, the caller passes X-Sam-Required-Labels: replica=v1-laptop on Turn 1."
expect "$CALLER" "python3 a2a_client.py \$NODE/sam/\$V1_PEER/a2a/support.acme ctx-acme-1042 'Check order #1042 for alice@acme.com' replica=v1-laptop" 'v1-laptop' 45
pause 3

caption "A Cloud Run replica (node-v2.yaml, replica=v2-cloudrun) joins the mesh. On Turn 2, the caller passes X-Sam-Required-Labels: replica=v2-cloudrun and keeps full conversation context."
expect "$CALLER" "cat node-v2.yaml" 'v2-cloudrun' 15
"$SAM_NODE" run \
  --control-plane "$CP_URL" --insecure-control-plane \
  --bootstrap-token-path "$WORK_DIR/dev-v2.token" --config "$HERE/node-v2.yaml" \
  --data-dir "$WORK_DIR/node-v2" --bind-addr 127.0.0.1:19103 --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "$WORK_DIR/dev2-api.token" --allow-loopback --control-plane-sync-interval 5s > "$WORK_DIR/node-v2.log" 2>&1 &
echo $! > "$WORK_DIR/node-v2.pid"
for _ in $(seq 1 40); do
  grep -q "SAM Node Online" "$WORK_DIR/node-v2.log" 2>/dev/null && break
  sleep 0.25
done
V2_PEER=$(awk '/PeerID:/ {print $2; exit}' "$WORK_DIR/node-v2.log")
for _ in $(seq 1 20); do
  curl -sf -H "X-Sam-Authentication: Bearer caller-secret" \
    "http://127.0.0.1:19102/sam/$V2_PEER/a2a/support.acme/.well-known/agent-card.json" >/dev/null 2>&1 && break
  sleep 0.5
done
tmux send-keys -t "$CALLER" "V2_PEER=$V2_PEER" Enter
wait_for "$CALLER" 'caller\$' 10
expect "$CALLER" "python3 a2a_client.py \$NODE/sam/\$V2_PEER/a2a/support.acme ctx-acme-1042 'What item did Alice order and has it shipped?' replica=v2-cloudrun" 'v2-cloudrun' 60
pause 3

# --- Act 3: 3-Tier Guardrails -----------------------------------------------

caption "Act 3, Layer 1 (Central Security): policy.json grants GET /repos/google/sam/pulls* on egress://api.github.com and injects secrets/github-ro. POST is blocked with 403."
expect "$ADMIN" "jq -c '.roles[0].http[0], .egress[0]' policy.json" 'github-ro' 15
expect "$CALLER" "curl -s \"\$NODE/sam/\$V1_PEER/egress/api.github.com/repos/google/sam/pulls?state=open&per_page=1\" | jq -c '.[0] | {number, title}'" 'number' 30
expect "$CALLER" "curl -i -s -X POST \$NODE/sam/\$V1_PEER/egress/api.github.com/repos/google/sam/pulls -d '{}' | head -n 5" '403 Forbidden' 30
pause 3

caption "Act 3, Layer 2 (Department Lead — Point 2b, Egress Node Label Check): node-v1.yaml requires positive 'check if label(\"team\", \"support\")', so node-contractor.yaml (team=contractor) is rejected."
expect "$ADMIN" "cat node-contractor.yaml" 'team: contractor' 15
expect "$CALLER" "mcp-client -url http://127.0.0.1:19104/mcp -token contractor-secret -tool call_remote_tool -args '{\"peer_id\":\"'\$V1_PEER'\",\"tool_name\":\"mcp://orders-db/get_order_status\",\"arguments\":{\"order_id\":\"1042\"}}'" 'check if label' 30
pause 3

caption "Act 3, Layer 3 (Individual User): RFC 8693 POST /oauth/token mints a sealed Task Biscuit scoped only to tool:get_order_status. get_order_status succeeds; a \$5,000 issue_refund is blocked."
expect "$CALLER" "TASK_BISCUIT=\$(curl -s -X POST \$NODE/oauth/token -H 'Authorization: Bearer caller-secret' -d 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange&resource=mcp://orders-db&scope=tool:get_order_status&seal=true' | jq -r .access_token) && echo \"TASK_BISCUIT=\${TASK_BISCUIT:0:36}...\"" 'TASK_BISCUIT=' 20
expect "$CALLER" "mcp-client -url \$NODE/mcp -token \$TASK_BISCUIT -tool call_remote_tool -args '{\"peer_id\":\"'\$V1_PEER'\",\"tool_name\":\"mcp://orders-db/get_order_status\",\"arguments\":{\"order_id\":\"1042\"}}'" 'Mechanical Keyboard Pro' 30
expect "$CALLER" "mcp-client -url \$NODE/mcp -token \$TASK_BISCUIT -tool call_remote_tool -args '{\"peer_id\":\"'\$V1_PEER'\",\"tool_name\":\"mcp://orders-db/issue_refund\",\"arguments\":{\"order_id\":\"1042\",\"amount_dollars\":5000}}'" 'task authorization denied' 30
pause 3.5

# --- Act 4: Day-2 Ops & Ban -------------------------------------------------

caption "Act 4 (Day-2 Operations): Every ALLOW and DENY across all three layers is logged in the left pane. Finally, the admin bans the contractor peer with sam-one admin ban."
expect "$ADMIN" "sam-one admin ban \$CONTRACTOR_PEER --server \$CP_URL --data-dir \$WORK_DIR/one" 'banned' 30
pause 3.5

caption "sam-mesh.dev"
pause 3

END=$(date +%s.%N)
tmux kill-session -t "$SESSION"
for _ in $(seq 1 30); do tmux has-session -t "${SESSION}-rec" 2>/dev/null || break; sleep 0.5; done

# --- render -----------------------------------------------------------------

python3 - "$OUT/demo.cast" "$CAPTIONS" "$SRT" "$END" "$IDLE_MAX" "$SPEED" <<'EOF'
import bisect, json, sys
cast, captions, srt, end, idle_max, speed = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4]), float(sys.argv[5]), float(sys.argv[6])
lines = open(cast).read().splitlines()
header = json.loads(lines[0])
events = [json.loads(l) for l in lines[1:]]
caps = [(float(w), text) for w, text in (l.split("\t", 1) for l in open(captions).read().splitlines())]
first_typing = next((e[0] for prev, e in zip([[0.0]] + events, events) if e[0] - prev[0] > 0.8 and e[0] < 10), 1.0)
t0 = caps[0][0] - first_typing
events = [e for e in events if e[0] <= end - t0 - 0.2]

knots_old, knots_new = [], []
prev_old = prev_new = 0.0
for e in events:
    prev_new += min(e[0] - prev_old, idle_max)
    prev_old = e[0]
    knots_old.append(prev_old)
    knots_new.append(prev_new)
    e[0] = round(prev_new, 6)

def remap(t):
    i = bisect.bisect_right(knots_old, t) - 1
    if i < 0:
        return 0.0
    return knots_new[i] + min(t - knots_old[i], idle_max)

def stamp(t):
    t = max(t, 0) / speed
    h, m, s = int(t // 3600), int(t % 3600 // 60), t % 60
    return f"{h:02d}:{m:02d}:{int(s):02d},{int((s - int(s)) * 1000):03d}"

caps = [(w - t0, text) for w, text in caps]
last = (events[-1][0] if events else 0.0) + idle_max
with open(srt, "w") as out:
    for i, (t, text) in enumerate(caps):
        stop = remap(caps[i + 1][0]) if i + 1 < len(caps) else last
        out.write(f"{i + 1}\n{stamp(remap(t))} --> {stamp(stop)}\n{text}\n\n")
with open(cast, "w") as out:
    out.write(json.dumps(header) + "\n")
    for e in events:
        out.write(json.dumps(e) + "\n")
print(f"take {knots_old[-1]:.0f}s, cut to {last:.0f}s, plays in {last / speed:.0f}s")
EOF

agg --cols "$COLS" --rows "$ROWS" --font-size 16 --idle-time-limit 30 --speed "$SPEED" --last-frame-duration 3 "$OUT/demo.cast" "$OUT/demo.gif"
ffmpeg -y -loglevel error -i "$OUT/demo.gif" \
  -movflags faststart -pix_fmt yuv420p \
  -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" \
  "$OUT/demo.mp4"

cp "$OUT/demo.mp4" "$REPO_ROOT/site/static/demo-multi-tier-guardrails.mp4"
echo "recorded and published: $REPO_ROOT/site/static/demo-multi-tier-guardrails.mp4"
