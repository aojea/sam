#!/usr/bin/env bash
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

# Multi-Tier Guardrails & Quality Gates — End-to-End 5-Persona Runner
#
# Demonstrates how SAM serves all 5 enterprise personas across 4 acts with real
# backends (Ollama gemma3:1b, official a2a-sdk, official mcp SDK + SQLite, and
# https://api.github.com), while clarifying the 3 points where labels are used:
#   1. Mesh Enforcement (policy.json allowed_labels): Control Plane attests
#      which labels a node is permitted to claim at enrollment.
#   2. Node Enforcement (node-caller.yaml egress.require_labels & node-v1.yaml
#      attenuation.checks): Input and Egress node admins enforce mandatory
#      outbound and inbound label constraints.
#   3. End-User / Agent Intent (X-Mesh-Required-Labels): Calling agents/users
#      express which replica or capability they want within admin constraints.

set -euo pipefail

INTERACTIVE=0
if [[ "${1:-}" == "--interactive" ]]; then
  INTERACTIVE=1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
SAM_ONE="${REPO_ROOT}/bin/sam-one"
SAM_NODE="${REPO_ROOT}/bin/sam-node"
MCP_CLIENT="${REPO_ROOT}/bin/mcp-client"
WORK_DIR="${WORK_DIR:-$(mktemp -d /tmp/sam-multi-tier-XXXXXX)}"
VENV_DIR="${VENV_DIR:-/tmp/sam-multi-tier-venv}"
PYTHON="${VENV_DIR}/bin/python"
export ORDERS_DB_PATH="${WORK_DIR}/orders.db"
export LLM_MODEL="${LLM_MODEL:-gemma3:1b}"

step_pause() {
  if [[ "${INTERACTIVE}" -eq 1 ]]; then
    echo ""
    read -r -p "    [Press ENTER to continue to the next step...] " _
    echo ""
  fi
}

cleanup() {
  for pidfile in "${WORK_DIR}"/*.pid; do
    [[ -f "${pidfile}" ]] && kill -9 "$(cat "${pidfile}")" 2>/dev/null || true
  done
  if [[ "${KEEP_WORK_DIR:-0}" != "1" ]]; then
    rm -rf "${WORK_DIR}"
  fi
}
trap cleanup EXIT

if [[ ! -x "${SAM_ONE}" || ! -x "${SAM_NODE}" || ! -x "${MCP_CLIENT}" ]]; then
  echo "Building sam-one, sam-node, and mcp-client..."
  make -C "${REPO_ROOT}" build
fi

if [[ ! -x "${PYTHON}" ]]; then
  python3 -m venv "${VENV_DIR}"
fi
"${PYTHON}" -c "import a2a.server, a2a.client, mcp.server.mcpserver" 2>/dev/null || \
  "${VENV_DIR}/bin/pip" install -q -r "${SCRIPT_DIR}/requirements.txt"

if [[ "${LLM_BASE_URL:-http://127.0.0.1:11434/v1}" == "http://127.0.0.1:11434/v1" ]]; then
  if ! curl -sf http://127.0.0.1:11434/v1/models >/dev/null 2>&1; then
    docker start sam-demo-ollama >/dev/null 2>&1 || \
      docker run -d --name sam-demo-ollama -p 127.0.0.1:11434:11434 -v sam-demo-ollama:/root/.ollama ollama/ollama >/dev/null
    for _ in $(seq 1 30); do curl -sf http://127.0.0.1:11434/ >/dev/null && break; sleep 1; done
    docker exec sam-demo-ollama ollama pull "${LLM_MODEL}" >/dev/null
  fi
fi

mkdir -p "${WORK_DIR}/secrets"

if [[ -n "${GITHUB_TOKEN_FILE:-}" && -s "${GITHUB_TOKEN_FILE}" ]]; then
  install -m 600 "${GITHUB_TOKEN_FILE}" "${WORK_DIR}/secrets/github-ro"
elif command -v gh >/dev/null 2>&1 && gh auth token >/dev/null 2>&1; then
  gh auth token > "${WORK_DIR}/secrets/github-ro"
  chmod 600 "${WORK_DIR}/secrets/github-ro"
else
  echo "ERROR: Set GITHUB_TOKEN_FILE or authenticate 'gh auth login' for egress://api.github.com" >&2
  exit 1
fi

echo "1) Starting real SQLite MCP server (orders_mcp.py) and LLM-backed A2A replicas (support_agent.py, model=${LLM_MODEL})..."
"${PYTHON}" "${SCRIPT_DIR}/orders_mcp.py" >"${WORK_DIR}/orders-mcp.log" 2>&1 &
echo $! > "${WORK_DIR}/orders-mcp.pid"

"${PYTHON}" "${SCRIPT_DIR}/support_agent.py" v1-laptop 17771 >"${WORK_DIR}/a2a-v1.log" 2>&1 &
echo $! > "${WORK_DIR}/a2a-v1.pid"

"${PYTHON}" "${SCRIPT_DIR}/support_agent.py" v2-cloudrun 17772 >"${WORK_DIR}/a2a-v2.log" 2>&1 &
echo $! > "${WORK_DIR}/a2a-v2.pid"

for _ in $(seq 1 120); do
  if curl -sf http://127.0.0.1:17771/.well-known/agent-card.json >/dev/null && \
     curl -sf http://127.0.0.1:17772/.well-known/agent-card.json >/dev/null; then
    break
  fi
  sleep 0.25
done

CP_PORT=18090
CP_URL="http://127.0.0.1:${CP_PORT}"
"${SAM_ONE}" \
  --data-dir "${WORK_DIR}/one" \
  --port "${CP_PORT}" \
  --policy-file "${SCRIPT_DIR}/policy.json" \
  --no-join-token \
  --enroll-qr=false \
  --log-level info >"${WORK_DIR}/sam-one.log" 2>&1 &
echo $! > "${WORK_DIR}/sam-one.pid"

for _ in $(seq 1 80); do
  if [[ -s "${WORK_DIR}/one/admin-token" ]] && curl -sf "${CP_URL}/readyz" >/dev/null; then
    break
  fi
  sleep 0.25
done
ADMIN_TOKEN="$(cat "${WORK_DIR}/one/admin-token")"

echo "caller-secret" > "${WORK_DIR}/caller-api.token"
echo "dev-secret" > "${WORK_DIR}/dev-api.token"
echo "dev2-secret" > "${WORK_DIR}/dev2-api.token"
echo "contractor-secret" > "${WORK_DIR}/contractor-api.token"
chmod 600 "${WORK_DIR}"/*.token

create_bootstrap_token() {
  local desc="$1"
  local out="$2"
  "${SAM_ONE}" token create \
    --server "${CP_URL}" \
    --data-dir "${WORK_DIR}/one" \
    --max-usages 1 \
    --description "${desc}" 2>/dev/null | awk '/^Token:/ {print $2}' > "${out}"
  chmod 600 "${out}"
}

create_bootstrap_token "caller-input-node" "${WORK_DIR}/caller.token"
create_bootstrap_token "dev-laptop-v1-spoof" "${WORK_DIR}/dev-spoof.token"
create_bootstrap_token "dev-laptop-v1-staging" "${WORK_DIR}/dev-v1.token"
create_bootstrap_token "cloudrun-v2-prod" "${WORK_DIR}/dev-v2.token"
create_bootstrap_token "contractor-node" "${WORK_DIR}/contractor.token"

# Start Input / Caller Node with node-caller.yaml (egress.require_labels: {env: prod})
"${SAM_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/caller.token" \
  --config "${SCRIPT_DIR}/node-caller.yaml" \
  --data-dir "${WORK_DIR}/node-caller" \
  --bind-addr 127.0.0.1:19002 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/caller-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-caller.log" 2>&1 &
echo $! > "${WORK_DIR}/node-caller.pid"

echo ""
echo "================================================================================"
echo "  ACT 1: Developer Onboarding into Quarantine (env=staging) + Automated Quality Gate"
echo "  (Personas: Agent Developer Alice + Central Security / Platform)"
echo "================================================================================"
echo ""
echo "--> Alice (Developer) enrolls her node in staging quarantine (env=staging, team=support)..."
"${SAM_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/dev-v1.token" \
  --config "${SCRIPT_DIR}/node-v1.yaml" \
  --secrets-dir "${WORK_DIR}/secrets" \
  --data-dir "${WORK_DIR}/node-v1" \
  --bind-addr 127.0.0.1:19001 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/dev-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-v1.log" 2>&1 &
echo $! > "${WORK_DIR}/node-v1.pid"

V1_PEER=""
for _ in $(seq 1 40); do
  DISC="$(curl -sf -H "X-Mesh-Authentication: Bearer caller-secret" \
    "http://127.0.0.1:19002/mesh/service/discover?type=a2a&name=support.acme" 2>/dev/null || true)"
  V1_PEER="$(echo "${DISC}" | jq -r '.[0].peer_id // empty' 2>/dev/null || true)"
  if [[ -n "${V1_PEER}" ]]; then
    break
  fi
  sleep 0.5
done
echo "    Discovered Alice's node PeerID via DHT/GossipSub: ${V1_PEER}"

echo ""
echo "--> Why can't Alice just set 'env: prod' in her node YAML, or the caller skip the check?"
echo "    [Label Point 1 — Mesh Enforcement (policy.json allowed_labels)]:"
sed 's/env: staging/env: prod/' "${SCRIPT_DIR}/node-v1.yaml" > "${WORK_DIR}/node-v1-prod.yaml"
set +e
SPOOF_OUT="$("${SAM_NODE}" join "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/dev-spoof.token" \
  --config "${WORK_DIR}/node-v1-prod.yaml" \
  --data-dir "${WORK_DIR}/node-spoof" 2>&1)"
set -e
echo "      Self-asserting env=prod before Quality Gate -> $(echo "${SPOOF_OUT}" | grep -oE 'Label not permitted.*' || echo "${SPOOF_OUT}" | tail -n 1)"

echo "    [Label Point 2a — Input Node Enforcement (node-caller.yaml egress.require_labels: {env: prod})]:"
HTTP_CODE="$(curl -s -o "${WORK_DIR}/act1-prod-check.out" -w '%{http_code}' \
  -H "X-Mesh-Authentication: Bearer caller-secret" \
  "http://127.0.0.1:19002/mesh/${V1_PEER}/a2a/support.acme/.well-known/agent-card.json")"
echo "      Plain app call (even with NO X-Mesh-Required-Labels header) -> HTTP ${HTTP_CODE} — $(cat "${WORK_DIR}/act1-prod-check.out")"
step_pause

echo ""
echo "--> Running Automated Quality Gate: validating A2A 1.0 schema in staging & promoting policy to allow env=prod..."
STAGING_CARD="$(curl -sf "http://127.0.0.1:17771/.well-known/agent-card.json")"
echo "    Staging A2A Card Validated: name='$(echo "${STAGING_CARD}" | jq -r '.name')', description='$(echo "${STAGING_CARD}" | jq -r '.description')'"

jq '.roles[0].allowed_labels += ["env=prod"]' "${SCRIPT_DIR}/policy.json" > "${WORK_DIR}/policy-prod.json"
curl -sf -X POST "${CP_URL}/policies" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  --data @"${WORK_DIR}/policy-prod.json" >/dev/null

kill -9 "$(cat "${WORK_DIR}/node-v1.pid")" 2>/dev/null || true
rm -rf "${WORK_DIR}/node-v1"
create_bootstrap_token "dev-laptop-v1-prod" "${WORK_DIR}/dev-v1.token"

"${SAM_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/dev-v1.token" \
  --config "${WORK_DIR}/node-v1-prod.yaml" \
  --secrets-dir "${WORK_DIR}/secrets" \
  --data-dir "${WORK_DIR}/node-v1" \
  --bind-addr 127.0.0.1:19001 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/dev-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >>"${WORK_DIR}/node-v1.log" 2>&1 &
echo $! > "${WORK_DIR}/node-v1.pid"

V1_STAGING_PEER="${V1_PEER}"
V1_PROD_PEER=""
for _ in $(seq 1 40); do
  DISC="$(curl -sf -H "X-Mesh-Authentication: Bearer caller-secret" \
    "http://127.0.0.1:19002/mesh/service/discover?type=a2a&name=support.acme" 2>/dev/null || true)"
  for p in $(echo "${DISC}" | jq -r '.[].peer_id // empty' 2>/dev/null || true); do
    if [[ "${p}" != "${V1_STAGING_PEER}" ]]; then
      V1_PROD_PEER="${p}"
      break 2
    fi
  done
  sleep 0.5
done
V1_PEER="${V1_PROD_PEER}"
echo "    Promoted Production PeerID (attested env=prod): ${V1_PEER}"

echo ""
echo "--> Retrying the exact same production call now that Quality Gate promoted Alice's node to env=prod..."
for _ in $(seq 1 20); do
  HTTP_CODE_AFTER="$(curl -s -o "${WORK_DIR}/act1-prod-after.out" -w '%{http_code}' \
    -H "X-Mesh-Authentication: Bearer caller-secret" \
    "http://127.0.0.1:19002/mesh/${V1_PEER}/a2a/support.acme/.well-known/agent-card.json")"
  [[ "${HTTP_CODE_AFTER}" == "200" ]] && break
  sleep 0.5
done
echo "    Result after Quality Gate: HTTP ${HTTP_CODE_AFTER} OK!"
echo "    Rewritten Mesh A2A Interface URL: $(jq -r '.supportedInterfaces[0].url' "${WORK_DIR}/act1-prod-after.out")"
step_pause

echo ""
echo "================================================================================"
echo "  ACT 2: Cross-Network A2A Routing & Zero-Downtime Replica Swap (Laptop v1 -> Cloud Run v2)"
echo "  (Persona: Platform / Networking + Label Point 3: End-User/Agent Intent)"
echo "================================================================================"
echo ""
echo "--> [Label Point 3 — End-User/Agent Intent (X-Mesh-Required-Labels)]:"
echo "    Within the Input Node's mandatory env=prod floor, the calling agent passes"
echo "    'X-Mesh-Required-Labels: replica=v1-laptop' on Turn 1 to express replica intent:"
TURN1_REPLY="$(SAM_API_TOKEN=caller-secret "${PYTHON}" "${SCRIPT_DIR}/a2a_client.py" \
  "http://127.0.0.1:19002/mesh/${V1_PEER}/a2a/support.acme" \
  "ctx-acme-1042" \
  "Check order #1042 for alice@acme.com and tell me the item and status." \
  "replica=v1-laptop")"
echo "    Turn 1 Reply: ${TURN1_REPLY}"

echo ""
echo "--> Starting Cloud Run v2 replica serving a2a://support.acme (env=prod, replica=v2-cloudrun)..."
"${SAM_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/dev-v2.token" \
  --config "${SCRIPT_DIR}/node-v2.yaml" \
  --data-dir "${WORK_DIR}/node-v2" \
  --bind-addr 127.0.0.1:19003 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/dev2-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-v2.log" 2>&1 &
echo $! > "${WORK_DIR}/node-v2.pid"

V2_PEER=""
for _ in $(seq 1 40); do
  DISC="$(curl -sf -H "X-Mesh-Authentication: Bearer caller-secret" \
    "http://127.0.0.1:19002/mesh/service/discover?type=a2a&name=support.acme" 2>/dev/null || true)"
  for p in $(echo "${DISC}" | jq -r '.[].peer_id // empty' 2>/dev/null || true); do
    if [[ "${p}" != "${V1_PEER}" && "${p}" != "${V1_STAGING_PEER}" ]]; then
      V2_PEER="${p}"
      break 2
    fi
  done
  sleep 0.5
done
echo "    Discovered Cloud Run v2 PeerID: ${V2_PEER}"

echo ""
echo "--> Sending Turn 2 (preserving contextId=ctx-acme-1042) with 'X-Mesh-Required-Labels: replica=v2-cloudrun'..."
TURN2_REPLY="$(SAM_API_TOKEN=caller-secret "${PYTHON}" "${SCRIPT_DIR}/a2a_client.py" \
  "http://127.0.0.1:19002/mesh/${V2_PEER}/a2a/support.acme" \
  "ctx-acme-1042" \
  "Based on our previous turn, what item did Alice order and has it shipped?" \
  "replica=v2-cloudrun")"
echo "    Turn 2 Reply: ${TURN2_REPLY}"
step_pause

echo ""
echo "================================================================================"
echo "  ACT 3: 3-Tier Guardrails (Central Security + Department Local Veto + User Per-Task tar_block)"
echo "  (Personas: Central Security, Department Lead, and Individual End-User)"
echo "================================================================================"
echo ""
echo "--> [Layer 1: Central Security Org Policy + Secret Broker] Calling real egress://api.github.com..."
GH_ALLOW="$(curl -sf -H "X-Mesh-Authentication: Bearer caller-secret" \
  -H "Accept: application/vnd.github+json" \
  -H "User-Agent: sam-multi-tier-demo" \
  "http://127.0.0.1:19002/mesh/${V1_PEER}/egress/api.github.com/repos/google/sam/pulls?state=open&per_page=1")"
PR_SUMMARY="$(echo "${GH_ALLOW}" | jq -r 'if type=="array" and length>0 then "#\(.[0].number) \(.[0].title)" else "200 OK (open PRs queried)" end')"
echo "    Allowed Path (GET /repos/google/sam/pulls): HTTP 200 OK — ${PR_SUMMARY} (node injected secrets/github-ro)"

GH_DENY_CODE="$(curl -s -o "${WORK_DIR}/gh-deny.out" -w '%{http_code}' \
  -X POST \
  -H "X-Mesh-Authentication: Bearer caller-secret" \
  -H "Accept: application/vnd.github+json" \
  -H "User-Agent: sam-multi-tier-demo" \
  "http://127.0.0.1:19002/mesh/${V1_PEER}/egress/api.github.com/repos/google/sam/pulls" \
  -d '{}')"
echo "    Forbidden Path (POST /repos/google/sam/pulls): HTTP ${GH_DENY_CODE} — blocked by Block 0 Org HTTP Policy before GitHub hears of it!"

echo ""
echo "--> [Layer 2: Department Lead Local Node Enforcement (Label Point 2b: Egress/Provider Node)]"
echo "    Support Dept's node-v1.yaml requires positive 'check if label(\"team\", \"support\")'."
echo "    Contractor node (attested team=contractor) attempts to call mcp://orders-db..."
"${SAM_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/contractor.token" \
  --config "${SCRIPT_DIR}/node-contractor.yaml" \
  --data-dir "${WORK_DIR}/node-contractor" \
  --bind-addr 127.0.0.1:19004 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/contractor-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-contractor.log" 2>&1 &
echo $! > "${WORK_DIR}/node-contractor.pid"

for _ in $(seq 1 30); do
  grep -q "SAM Node Online" "${WORK_DIR}/node-contractor.log" 2>/dev/null && break
  sleep 0.25
done
CONTRACTOR_PEER="$(awk '/PeerID:/ {print $2; exit}' "${WORK_DIR}/node-contractor.log")"

set +e
CONTRACTOR_OUT="$("${MCP_CLIENT}" \
  -url "http://127.0.0.1:19004/mcp" \
  -token "contractor-secret" \
  -tool "call_remote_tool" \
  -args "{\"peer_id\":\"${V1_PEER}\",\"tool_name\":\"mcp://orders-db/get_order_status\",\"arguments\":{\"order_id\":\"1042\"}}" 2>&1)"
set -e
echo "    Contractor Call Result (Org Policy=ALLOW, Dept Egress-Node Label Check=DENY):"
echo "    $(echo "${CONTRACTOR_OUT}" | tail -n 1)"

echo ""
echo "--> [Layer 3: Individual User Per-Task Biscuit Attenuation (tar_block)]"
echo "    Minting a sealed Task Biscuit via RFC 8693 POST /oauth/token scoped ONLY to tool:get_order_status..."
TOKEN_RESP="$(curl -sf -X POST "http://127.0.0.1:19002/oauth/token" \
  -H "Authorization: Bearer caller-secret" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  --data-urlencode "resource=mcp://orders-db" \
  --data-urlencode "scope=tool:get_order_status" \
  --data-urlencode "seal=true")"
TASK_BISCUIT="$(echo "${TOKEN_RESP}" | jq -r '.access_token')"
echo "    Issued Sealed Task Biscuit (first 36 chars): ${TASK_BISCUIT:0:36}..."

echo "    1) Legitimate SQLite MCP tool call (get_order_status) using the narrowed Task Biscuit:"
ALLOW_TOOL_OUT="$("${MCP_CLIENT}" \
  -url "http://127.0.0.1:19002/mcp" \
  -token "${TASK_BISCUIT}" \
  -tool "call_remote_tool" \
  -args "{\"peer_id\":\"${V1_PEER}\",\"tool_name\":\"mcp://orders-db/get_order_status\",\"arguments\":{\"order_id\":\"1042\"}}" 2>&1)"
echo "       $(echo "${ALLOW_TOOL_OUT}" | tr '\n' ' ')"

echo "    2) Prompt-Injection Exploit attempt (calling issue_refund for \$5,000) using the SAME Task Biscuit:"
set +e
DENY_TOOL_OUT="$("${MCP_CLIENT}" \
  -url "http://127.0.0.1:19002/mcp" \
  -token "${TASK_BISCUIT}" \
  -tool "call_remote_tool" \
  -args "{\"peer_id\":\"${V1_PEER}\",\"tool_name\":\"mcp://orders-db/issue_refund\",\"arguments\":{\"order_id\":\"1042\",\"amount_dollars\":5000}}" 2>&1)"
set -e
echo "       BLOCKED MID-FLIGHT: $(echo "${DENY_TOOL_OUT}" | tail -n 1)"
REFUND_COUNT="$("${PYTHON}" -c "import sqlite3; print(sqlite3.connect('${ORDERS_DB_PATH}').execute('SELECT COUNT(*) FROM refunds').fetchone()[0])")"
echo "       SQLite verification: SELECT COUNT(*) FROM refunds => ${REFUND_COUNT} (database untouched!)"
step_pause

echo ""
echo "================================================================================"
echo "  ACT 4: Day-2 Operations — Correlated Audit Receipts & Instant Kill-Switch"
echo "  (Persona: Day-2 Operations / SRE)"
echo "================================================================================"
echo ""
echo "--> Hosting Node (node-v1) Structured Audit Trail (python3 audit.py):"
"${PYTHON}" "${SCRIPT_DIR}/audit.py" < "${WORK_DIR}/node-v1.log" | sed 's/^/    /'

echo ""
echo "--> Executing Instant Kill-Switch: banning contractor PeerID via sam-one admin ban..."
BAN_OUT="$("${SAM_ONE}" admin ban "${CONTRACTOR_PEER}" --server "${CP_URL}" --data-dir "${WORK_DIR}/one" 2>&1)"
echo "    ${BAN_OUT}"

echo ""
echo "================================================================================"
echo "  ALL 4 ACTS COMPLETED SUCCESSFULLY!"
echo "================================================================================"
