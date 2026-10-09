#!/usr/bin/env bats

load "lib/container_mesh.bash"

CALC_MCP_IMAGE="agentmesh-calc-mcp:local"

build_calc_mcp_image() {
  if ! docker image inspect "${CALC_MCP_IMAGE}" >/dev/null 2>&1; then
    docker build -t "${CALC_MCP_IMAGE}" \
      -f tests/e2e/docker/calc-mcp/Dockerfile \
      tests/e2e/docker/calc-mcp >/dev/null
  fi
}

start_calc_mcp() {
  local name="${MESH_PREFIX}-calc-mcp"
  docker run -d \
    --name "${name}" \
    --network "${MESH_NETWORK}" \
    --network-alias calc-mcp \
    "${CALC_MCP_IMAGE}" >/dev/null
  MESH_CONTAINERS+=("${name}")
  mesh_wait_for_http "http://calc-mcp:7777/mcp" 20
}

setup() {
  mesh_setup_env
  build_calc_mcp_image
  mkdir -p tests/e2e/logs

  local node_policy="version: \"v1alpha1\"
services:
  - type: \"mcp\"
    name: \"calculator\"
    description: \"Simple math operations\"
    target_url: \"http://calc-mcp:7777/mcp\"
  - type: \"mcp\"
    name: \"db-agent\"
    description: \"Database operations\"
    target_url: \"http://calc-mcp:7777/mcp\"
attenuation:
  policies:
    - 'deny if service(\"mcp\", \"db-agent\");'"

  local config_file="/tmp/${MESH_PREFIX}-local_policy.yaml"
  echo "${node_policy}" > "${config_file}"

  # Start services
  start_calc_mcp

  # Initialize router PeerID from suite-level file
  mesh_start_router

  # Start Node 1 (Target) with local policy file
  mesh_start_node 1 "" "${config_file}"
  mesh_wait_for_mcp_ready 1 30

  export TARGET_PEER_ID
  TARGET_PEER_ID="$(mesh_node_peer_id 1)"
  [[ -n "${TARGET_PEER_ID}" ]]

  # Start Node 2 (Caller)
  mesh_start_node 2
  mesh_wait_for_mcp_ready 2 30

  # Connect Node 2 to Node 1 explicitly so the calls below do not depend on
  # discovery timing, and wait until Node 2 reports the connection.
  mesh_connect_peer 2 "$(mesh_node_addr 1)" >/dev/null
  mesh_wait_for_peer_connection 2 "${TARGET_PEER_ID}" 30
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
  fi
  mesh_cleanup_env
  rm -f "/tmp/${MESH_PREFIX}-local_policy.yaml" || true
}

@test "Policy E2E: Positive Path (Allowed by control plane, Not blocked by Node)" {
  local call_args="{\"peer_id\":\"${TARGET_PEER_ID}\",\"tool_name\":\"mcp://calculator/add\",\"arguments\":{\"a\":2,\"b\":3}}"
  run docker run --rm --network "${MESH_NETWORK}" "${MESH_RUNTIME_IMAGE}" mcp-client -url "http://${MESH_PREFIX}-node-2:8080/mcp" -tool "call_remote_tool" -args "${call_args}"
  echo "Output: $output"
  [ "$status" -eq 0 ]
  [[ "$output" == *"5"* ]]
}

@test "Policy E2E: Negative Path (Denied by control plane)" {
  local call_args="{\"peer_id\":\"${TARGET_PEER_ID}\",\"tool_name\":\"mcp://unauthorized-service/reboot_server\",\"arguments\":{}}"
  run docker run --rm --network "${MESH_NETWORK}" "${MESH_RUNTIME_IMAGE}" mcp-client -url "http://${MESH_PREFIX}-node-2:8080/mcp" -tool "call_remote_tool" -args "${call_args}"
  echo "Output: $output"
  [[ "$output" == *"denied"* ]]
}

@test "Policy E2E: Attenuation Path (Allowed by control plane, Blocked by Node)" {
  local call_args="{\"peer_id\":\"${TARGET_PEER_ID}\",\"tool_name\":\"mcp://db-agent/delete_tables\",\"arguments\":{}}"
  run docker run --rm --network "${MESH_NETWORK}" "${MESH_RUNTIME_IMAGE}" mcp-client -url "http://${MESH_PREFIX}-node-2:8080/mcp" -tool "call_remote_tool" -args "${call_args}"
  echo "Output: $output"
  [[ "$output" == *"denied"* ]]
}
