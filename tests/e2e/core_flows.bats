#!/usr/bin/env bats

load "lib/container_mesh.bash"

setup() {
  if ! mesh_require_docker; then
    skip "docker not available or daemon not running"
  fi

  if [[ ! -x "./bin/sam-node" || ! -x "./bin/sam-hub" ]]; then
    skip "missing binaries; run: make build"
  fi

  mesh_setup_env
  mkdir -p tests/e2e/logs
}

teardown() {
  if [[ "${BATS_TEST_COMPLETED:-0}" -ne 1 ]]; then
    mkdir -p tests/e2e/logs
    local ids
    ids="$(docker ps -aq --filter "name=mesh-")"
    for id in ${ids}; do
      local name
      name="$(docker inspect -f '{{.Name}}' "${id}" | tr -d '/')"
      docker logs "${id}" > "tests/e2e/logs/${name}.log" 2>&1 || true
    done

    echo "Node 1 logs on failure (filtered):"
    docker logs "${MESH_PREFIX}-node-1" 2>&1 | grep -i -E 'mcp|request|error|fatal|panic' || true
  fi
  mesh_cleanup_env
}

@test "Core Flows: Auth, Relay, Python SDK" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  run mesh_start_hub
  [[ "$status" -eq 0 ]]

  # 1. Start Node 1 (Client Credentials + Relay + Python SDK flags)
  run mesh_start_node 1 "--enable-relay=true --log-level=debug --discovery-interval 100ms"
  [[ "$status" -eq 0 ]]

  run mesh_assert_container_running "${MESH_PREFIX}-node-1"
  [[ "$status" -eq 0 ]]

  local node1_name="${MESH_PREFIX}-node-1"
  run mesh_wait_for_log "${node1_name}" "Enabling Relay Service" 20
  [[ "$status" -eq 0 ]]

  mesh_wait_for_log "${node1_name}" "SAM Node Online" 20
  mesh_wait_for_mcp_ready 1 20

  # 2. Python SDK Check against Node 1
  run docker run --rm \
    --network "${MESH_NETWORK}" \
    -v "$(pwd)/sam-mcp-python:/sam-mcp-python" \
    -e PYTHONPATH=/sam-mcp-python/src \
    python:3.12 \
    bash -c 'pip install mcp httpx && python3 /sam-mcp-python/test_client.py'
  echo "Python SDK output: $output"
  if [[ "$status" -ne 0 ]]; then
    echo "Node 1 logs:"
    docker logs "${node1_name}"
  fi
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"TOOLS_COUNT:"* ]]
  [[ "$output" == *"CALL_RESULT:"* ]]

  # 3. Device Authorization Flow (Interactive)
  local token
  token=$(docker run --rm --network "${MESH_NETWORK}" python:3.12 python3 -c "import urllib.request; import json; req = urllib.request.Request('http://mock-oidc:18080/token', data=b''); resp = urllib.request.urlopen(req); print(json.loads(resp.read().decode())['access_token'])")

  [[ -n "${token}" ]]

  local node_login_name="${MESH_PREFIX}-node-login"
  local hub_peer_id
  hub_peer_id=$(cat "/tmp/${MESH_PREFIX}-hub-peer-id")

  local data_vol="${MESH_PREFIX}-data"
  docker volume create "${data_vol}"

  echo "${token}" | docker run --rm \
    --network "${MESH_NETWORK}" \
    -v "${data_vol}:/root/.config/sam-mesh" \
    -i \
    "sam-node:local" \
    login --hub "/dns4/sam-hub/tcp/4002/p2p/${hub_peer_id}" --token-url "http://mock-oidc:18080/token"

  docker run -d \
    --name "${node_login_name}" \
    --network "${MESH_NETWORK}" \
    -v "${data_vol}:/root/.config/sam-mesh" \
    "sam-node:local" \
    run \
    --hub "/dns4/sam-hub/tcp/4002/p2p/${hub_peer_id}"

  mesh_wait_for_log "${node_login_name}" "Using stored identity." 20
  docker volume rm "${data_vol}" >/dev/null 2>&1 || true

  # 4. Workload Identity Federation (JWT Path)
  local wi_token
  wi_token=$(docker run --rm --network "${MESH_NETWORK}" python:3.12 python3 -c "import urllib.request; import json; req = urllib.request.Request('http://mock-oidc:18080/token', data=b''); resp = urllib.request.urlopen(req); print(json.loads(resp.read().decode())['access_token'])")

  [[ -n "${wi_token}" ]]

  local token_vol="${MESH_PREFIX}-token"
  docker volume create "${token_vol}"

  docker run --rm \
    -v "${token_vol}:/tokens" \
    busybox \
    sh -c "echo \"${wi_token}\" > /tokens/sa-token"

  local node_wi_name="${MESH_PREFIX}-node-wi"

  docker run -d \
    --name "${node_wi_name}" \
    --network "${MESH_NETWORK}" \
    -v "${token_vol}:/var/run/secrets/tokens" \
    "sam-node:local" \
    run \
    --hub "/dns4/sam-hub/tcp/4002/p2p/${hub_peer_id}" \
    --jwt-path "/var/run/secrets/tokens/sa-token"

  mesh_wait_for_log "${node_wi_name}" "SAM Node Online" 20

  docker volume rm "${token_vol}" >/dev/null 2>&1 || true
}
