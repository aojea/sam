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
}

teardown() {
  mesh_cleanup_env
}

@test "Services: remote service is discoverable by type" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  mesh_start_router

  echo "[$(date +%T)] Starting Node 1"
  mesh_start_node 1 "--log-level debug"
  local node1_name="${MESH_PREFIX}-node-1"
  mesh_wait_for_mcp_ready 1 60

  echo "[$(date +%T)] Starting calc-mcp backend"
  start_calc_mcp

  echo "[$(date +%T)] Starting Node 2 (with calculator service config)"
  mesh_start_node 2 \
    "--log-level debug" \
    "tests/e2e/docker/calc-mcp/agentmesh-node-config.yaml"
  local node2_name="${MESH_PREFIX}-node-2"
  mesh_wait_for_mcp_ready 2 30

  local node2_peer_id
  node2_peer_id=$(mesh_node_peer_id 2)
  [[ -n "${node2_peer_id}" ]]

  echo "[$(date +%T)] Connecting Node 1 to Node 2"
  run mesh_connect_peer 1 "$(mesh_node_addr 2)"
  [[ "$status" -eq 0 ]]
  mesh_wait_for_peer_connection 1 "${node2_peer_id}" 20

  # Wait for the per-type rendezvous CID to propagate through the DHT.
  sleep 2

  echo "[$(date +%T)] Discovering MCP services from Node 1 (type-only)"
  run docker run --rm --network "${MESH_NETWORK}" \
    "${MESH_RUNTIME_IMAGE}" mcp-client \
    -timeout 30 \
    -url "http://${node1_name}:8080/mcp" \
    -tool "discover_remote_services" \
    -args '{"type":"mcp"}'
  echo "Discovery output: $output"
  [[ "$status" -eq 0 ]]

  # mcp-client prints each text-content line; the catalog JSON is the last non-empty line.
  local catalog
  catalog=$(echo "$output" | tail -n 1)

  local match_count
  match_count=$(echo "$catalog" | jq --arg pid "${node2_peer_id}" '
    [.[] | select(.srv_name == "calculator"
                 and .peer_id == $pid
                 and .srv_description == "Simple math operations")] | length
  ')
  echo "Matching entries for calculator: ${match_count}"
  [[ "${match_count}" -eq 1 ]]

  local underscore_match_count
  underscore_match_count=$(echo "$catalog" | jq --arg pid "${node2_peer_id}" '
    [.[] | select(.srv_name == "calc_service"
                 and .peer_id == $pid
                 and .srv_description == "Simple math operations with underscore")] | length
  ')
  echo "Matching entries for calc_service: ${underscore_match_count}"
  [[ "${underscore_match_count}" -eq 1 ]]
}
