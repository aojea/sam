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

# Private DIY Enterprise Stack — End-to-End Runner
#
# Demonstrates how an enterprise connects three open layers into a zero-trust AI
# workforce running safely across on-premises infrastructure and cloud:
#   1. Reasoning Layer: Open-weights LLM (Gemma 3 / DeepSeek-R1) on Cloud Run,
#      GKE, or on-prem GPUs published to the mesh by a GPU sidecar (node-inference.yaml).
#   2. Orchestration Layer: Self-hosted Agent Harness (agent_harness.py) that
#      talks ONLY to its local sidecar (127.0.0.1:19101/mcp and /v1) with zero
#      hardcoded database credentials or network topology knowledge.
#   3. Zero-Trust Fabric: agentmesh-one (Control Plane + Router) and
#      node-tools.yaml enforcing cryptographic Biscuit identities, Default-Deny
#      service-name routing, data residency labels, and per-task tar_block limits.

set -euo pipefail

# Local simulation execution: ensure no corporate HTTP proxy intercepts 127.0.0.1.
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
export NO_PROXY="127.0.0.1,localhost"
export no_proxy="127.0.0.1,localhost"

INTERACTIVE=0
if [[ "${1:-}" == "--interactive" ]]; then
  INTERACTIVE=1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
BIN_DIR="${BIN_DIR:-${REPO_ROOT}/bin}"
AGENTMESH_ONE="${BIN_DIR}/agentmesh-one"
AGENTMESH_NODE="${BIN_DIR}/agentmesh-node"
MCP_CLIENT="${BIN_DIR}/mcp-client"
WORK_DIR="${WORK_DIR:-$(mktemp -d /tmp/sam-private-diy-XXXXXX)}"

export ERP_DB_PATH="${WORK_DIR}/erp.db"
export ERP_MCP_PORT="${ERP_MCP_PORT:-18890}"
export COMPLIANCE_API_PORT="${COMPLIANCE_API_PORT:-18891}"
export PRIVATE_LLM_PORT="${PRIVATE_LLM_PORT:-18892}"
export LLM_MODEL="${LLM_MODEL:-deepseek-r1:1.5b}"
export COMPLIANCE_API_SECRET="private-vault-secret-2026"

step_pause() {
  if [[ "${INTERACTIVE}" -eq 1 ]]; then
    echo ""
    read -r -p "    [Press ENTER to continue to the next act...] " _
    echo ""
  fi
}

cleanup() {
  for pidfile in "${WORK_DIR}"/*.pid; do
    if [[ -f "${pidfile}" ]]; then
      local pid
      pid="$(cat "${pidfile}")"
      kill -9 "${pid}" 2>/dev/null || true
      wait "${pid}" 2>/dev/null || true
    fi
  done
  if [[ "${KEEP_WORK_DIR:-0}" != "1" ]]; then
    rm -rf "${WORK_DIR}"
  fi
}
trap cleanup EXIT

if [[ ! -x "${AGENTMESH_ONE}" || ! -x "${AGENTMESH_NODE}" || ! -x "${MCP_CLIENT}" ]]; then
  echo "Building agentmesh-one, agentmesh-node, and mcp-client..."
  make -C "${REPO_ROOT}" build OUT_DIR="${BIN_DIR}"
fi

mkdir -p "${WORK_DIR}/secrets"
printf '%s' "${COMPLIANCE_API_SECRET}" > "${WORK_DIR}/secrets/compliance-api-key"
chmod 600 "${WORK_DIR}/secrets/compliance-api-key"

echo "1) Starting On-Prem & Cloud Backends (SQLite ERP MCP, Compliance REST API, and ${LLM_MODEL} Reasoning Engine)..."
python3 "${SCRIPT_DIR}/backends.py" >"${WORK_DIR}/backends.log" 2>&1 &
echo $! > "${WORK_DIR}/backends.pid"

for _ in $(seq 1 60); do
  if curl -sf "http://127.0.0.1:${ERP_MCP_PORT}/healthz" >/dev/null && \
     curl -sf "http://127.0.0.1:${COMPLIANCE_API_PORT}/healthz" >/dev/null && \
     curl -sf "http://127.0.0.1:${PRIVATE_LLM_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 0.2
done

CP_PORT="${CP_PORT:-18095}"
CP_URL="http://127.0.0.1:${CP_PORT}"
echo "2) Starting Private Control Plane & Mesh Router (agentmesh-one on ${CP_URL})..."
"${AGENTMESH_ONE}" \
  --data-dir "${WORK_DIR}/one" \
  --port "${CP_PORT}" \
  --policy-file "${SCRIPT_DIR}/policy.json" \
  --no-join-token \
  --enroll-qr=false \
  --log-level info >"${WORK_DIR}/agentmesh-one.log" 2>&1 &
echo $! > "${WORK_DIR}/agentmesh-one.pid"

for _ in $(seq 1 80); do
  if [[ -s "${WORK_DIR}/one/admin-token" ]] && grep -q "Agent Mesh standalone mesh is ready" "${WORK_DIR}/agentmesh-one.log" 2>/dev/null; then
    break
  fi
  sleep 0.25
done

echo "agent-sidecar-secret" > "${WORK_DIR}/agent-api.token"
echo "gpu-sidecar-secret" > "${WORK_DIR}/gpu-api.token"
echo "vault-sidecar-secret" > "${WORK_DIR}/vault-api.token"
echo "external-sidecar-secret" > "${WORK_DIR}/external-api.token"
chmod 600 "${WORK_DIR}"/*.token

create_bootstrap_token() {
  local desc="$1"
  local out="$2"
  "${AGENTMESH_ONE}" token create \
    --server "${CP_URL}" \
    --data-dir "${WORK_DIR}/one" \
    --max-usages 1 \
    --description "${desc}" 2>/dev/null | awk '/^Token:/ {print $2}' > "${out}"
  chmod 600 "${out}"
}

create_bootstrap_token "agent-workstation-sidecar" "${WORK_DIR}/agent.bootstrap"
create_bootstrap_token "gpu-pool-inference-sidecar" "${WORK_DIR}/gpu.bootstrap"
create_bootstrap_token "vault-tools-sidecar" "${WORK_DIR}/vault.bootstrap"
create_bootstrap_token "external-dept-sidecar" "${WORK_DIR}/external.bootstrap"

echo ""
echo "================================================================================"
echo "  ACT 1: The Sidecar Node Architecture — Connecting Agent, GPU & Private Tools"
echo "================================================================================"
echo ""
echo "--> Enrolling 3 independent agentmesh-node sidecars into the private mesh:"
echo "    1. Vault Tool Sidecar      (node-tools.yaml)     -> publishes mcp://erp-db, mcp://payroll-vault & egress://compliance.internal"
echo "    2. GPU Reasoning Sidecar   (node-inference.yaml) -> publishes inference://private-llm (${LLM_MODEL})"
echo "    3. Agent Harness Sidecar   (node-agent.yaml)     -> exposes 127.0.0.1:19101/mcp and 127.0.0.1:19101/v1 to the local agent"

"${AGENTMESH_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/vault.bootstrap" \
  --config "${SCRIPT_DIR}/node-tools.yaml" \
  --secrets-dir "${WORK_DIR}/secrets" \
  --data-dir "${WORK_DIR}/node-tools" \
  --bind-addr 127.0.0.1:19102 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/vault-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-tools.log" 2>&1 &
echo $! > "${WORK_DIR}/node-tools.pid"

"${AGENTMESH_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/gpu.bootstrap" \
  --config "${SCRIPT_DIR}/node-inference.yaml" \
  --data-dir "${WORK_DIR}/node-inference" \
  --bind-addr 127.0.0.1:19103 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/gpu-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-inference.log" 2>&1 &
echo $! > "${WORK_DIR}/node-inference.pid"

"${AGENTMESH_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/agent.bootstrap" \
  --config "${SCRIPT_DIR}/node-agent.yaml" \
  --data-dir "${WORK_DIR}/node-agent" \
  --bind-addr 127.0.0.1:19101 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/agent-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-agent.log" 2>&1 &
echo $! > "${WORK_DIR}/node-agent.pid"

VAULT_PEER=""
GPU_PEER=""
for _ in $(seq 1 50); do
  MCP_DISC="$(curl -sf -H "X-Mesh-Authentication: Bearer agent-sidecar-secret" \
    "http://127.0.0.1:19101/mesh/service/discover?type=mcp&name=erp-db" 2>/dev/null || true)"
  INF_DISC="$(curl -sf -H "X-Mesh-Authentication: Bearer agent-sidecar-secret" \
    "http://127.0.0.1:19101/mesh/service/discover?type=inference&name=private-llm" 2>/dev/null || true)"
  VAULT_PEER="$(echo "${MCP_DISC}" | jq -r '.[0].peer_id // empty' 2>/dev/null || true)"
  GPU_PEER="$(echo "${INF_DISC}" | jq -r '.[0].peer_id // empty' 2>/dev/null || true)"
  if [[ -n "${VAULT_PEER}" && -n "${GPU_PEER}" ]]; then
    break
  fi
  sleep 0.5
done

echo ""
echo "    Discovered via Kademlia DHT / GossipSub from Agent's local sidecar:"
echo "      • Vault Tool Node PeerID      : ${VAULT_PEER} (mcp://erp-db, egress://compliance.internal)"
echo "      • Bare-Metal GPU Node PeerID  : ${GPU_PEER} (inference://private-llm -> ${LLM_MODEL})"
step_pause

echo ""
echo "================================================================================"
echo "  ACT 2: Private DIY Execution — DeepSeek Reasoning + Harness + Agent Mesh"
echo "================================================================================"
echo ""
echo "--> Running self-hosted agent_harness.py (talks ONLY to 127.0.0.1:19101/mcp and /v1):"
AGENTMESH_SIDECAR_URL="http://127.0.0.1:19101" \
AGENTMESH_API_TOKEN="agent-sidecar-secret" \
LLM_MODEL="${LLM_MODEL}" \
  python3 "${SCRIPT_DIR}/agent_harness.py"
step_pause

echo ""
echo "================================================================================"
echo "  ACT 3: Zero-Trust Identity & Default-Deny Policy (No Free-Roaming LLMs)"
echo "================================================================================"
echo ""
echo "--> [3a. Default-Deny on Service Names (mcp://payroll-vault)]"
echo "    Even though mcp://payroll-vault is registered on the same Vault node,"
echo "    policy.json only grants mcp://erp-db to this role. Attempting to call payroll-vault:"
set +e
PAYROLL_OUT="$("${MCP_CLIENT}" \
  -url "http://127.0.0.1:19101/mcp" \
  -token-path "${WORK_DIR}/agent-api.token" \
  -tool "call_remote_tool" \
  -args "{\"peer_id\":\"${VAULT_PEER}\",\"tool_name\":\"mcp://payroll-vault/dump_executive_salaries\",\"arguments\":{}}" 2>&1)"
set -e
echo "      Result -> $(echo "${PAYROLL_OUT}" | tail -n 1)"

echo ""
echo "--> [3b. HTTP Method/Path Guardrail on Internal API (egress://compliance.internal)]"
echo "    policy.json permits GET /v1/vendors/*, but denies POST mutations:"
HTTP_CODE="$(curl -s -o "${WORK_DIR}/post-deny.out" -w '%{http_code}' \
  -X POST \
  -H "X-Mesh-Authentication: Bearer agent-sidecar-secret" \
  "http://127.0.0.1:19101/mesh/${VAULT_PEER}/egress/compliance.internal/v1/vendors/ACME-01")"
echo "      POST /mesh/${VAULT_PEER:0:12}…/egress/compliance.internal/v1/vendors/ACME-01 -> HTTP ${HTTP_CODE} ($(tr -d '\n' < "${WORK_DIR}/post-deny.out"))"

echo ""
echo "--> [3c. Department Isolation Boundary (dept=external rejected by Vault Node Datalog Check)]"
"${AGENTMESH_NODE}" run \
  --control-plane "${CP_URL}" \
  --insecure-control-plane \
  --bootstrap-token-path "${WORK_DIR}/external.bootstrap" \
  --config "${SCRIPT_DIR}/node-external.yaml" \
  --data-dir "${WORK_DIR}/node-external" \
  --bind-addr 127.0.0.1:19104 \
  --socket-path="" \
  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
  --api-token-path "${WORK_DIR}/external-api.token" \
  --allow-loopback \
  --control-plane-sync-interval 5s >"${WORK_DIR}/node-external.log" 2>&1 &
echo $! > "${WORK_DIR}/node-external.pid"

for _ in $(seq 1 40); do
  grep -q "Agent Mesh Node Online" "${WORK_DIR}/node-external.log" 2>/dev/null && break
  sleep 0.25
done
EXTERNAL_PEER="$(awk '/PeerID:/ {print $2; exit}' "${WORK_DIR}/node-external.log")"

set +e
EXT_OUT="$("${MCP_CLIENT}" \
  -url "http://127.0.0.1:19104/mcp" \
  -token-path "${WORK_DIR}/external-api.token" \
  -tool "call_remote_tool" \
  -args "{\"peer_id\":\"${VAULT_PEER}\",\"tool_name\":\"mcp://erp-db/lookup_invoice\",\"arguments\":{\"invoice_id\":\"INV-2026-089\"}}" 2>&1)"
set -e
echo "      Caller with dept=external calling mcp://erp-db -> $(echo "${EXT_OUT}" | tail -n 1)"

echo ""
echo "--> [3d. Per-Task Sealed Biscuit (tar_block) — Blocking Prompt-Injected Wire Transfers]"
echo "    Minting a sealed Task Biscuit via RFC 8693 POST /oauth/token narrowed to read-only tools:"
TASK_BISCUIT="$(curl -sf -X POST "http://127.0.0.1:19101/oauth/token" \
  -H "Authorization: Bearer agent-sidecar-secret" \
  -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  -d "resource=mcp://erp-db" \
  -d "scope=tool:lookup_invoice tool:check_inventory" \
  -d "seal=true" | jq -r '.access_token')"
printf '%s' "${TASK_BISCUIT}" > "${WORK_DIR}/task-biscuit.token"
chmod 600 "${WORK_DIR}/task-biscuit.token"
echo "      Sealed Task Biscuit minted (prefix: ${TASK_BISCUIT:0:28}…)"

READ_OK="$("${MCP_CLIENT}" \
  -url "http://127.0.0.1:19101/mcp" \
  -token-path "${WORK_DIR}/task-biscuit.token" \
  -tool "call_remote_tool" \
  -args "{\"peer_id\":\"${VAULT_PEER}\",\"tool_name\":\"mcp://erp-db/lookup_invoice\",\"arguments\":{\"invoice_id\":\"INV-2026-089\"}}" | tail -n 1)"
echo "      1. Allowed read tool (lookup_invoice)          -> ${READ_OK}"

set +e
WIRE_DENY="$("${MCP_CLIENT}" \
  -url "http://127.0.0.1:19101/mcp" \
  -token-path "${WORK_DIR}/task-biscuit.token" \
  -tool "call_remote_tool" \
  -args "{\"peer_id\":\"${VAULT_PEER}\",\"tool_name\":\"mcp://erp-db/execute_wire_transfer\",\"arguments\":{\"invoice_id\":\"INV-2026-089\",\"amount_usd\":250000,\"iban\":\"DE89-ATTACKER\"}}" 2>&1)"
set -e
WIRE_COUNT="$(python3 -c "import sqlite3; c=sqlite3.connect('${ERP_DB_PATH}'); print(c.execute('SELECT COUNT(*) FROM wire_transfers').fetchone()[0])")"
echo "      2. Prompt-injected tool (execute_wire_transfer) -> $(echo "${WIRE_DENY}" | tail -n 1)"
echo "      3. SQLite wire_transfers table row count        -> ${WIRE_COUNT} (database untouched!)"
step_pause

echo ""
echo "================================================================================"
echo "  ACT 4: Private Day-2 Operations — Correlated Audit Trail & Kill-Switch"
echo "================================================================================"
echo ""
echo "--> Correlated ALLOW / DENY decisions across GPU and Vault Sidecars (audit.py):"
cat "${WORK_DIR}/node-inference.log" "${WORK_DIR}/node-tools.log" "${WORK_DIR}/node-agent.log" | python3 "${SCRIPT_DIR}/audit.py" | sed 's/^/    /'

echo ""
echo "--> Revoking unauthorized external node (${EXTERNAL_PEER}) mesh-wide via agentmesh-one admin ban:"
"${AGENTMESH_ONE}" admin ban "${EXTERNAL_PEER}" \
  --server "${CP_URL}" \
  --data-dir "${WORK_DIR}/one" | sed 's/^/    /'

echo ""
echo "Private DIY Enterprise Stack completed all 4 acts successfully."
