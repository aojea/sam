#!/usr/bin/env bats

load "lib/container_mesh.bash"

setup() {
  mesh_setup_env
  mkdir -p tests/e2e/logs
}

teardown() {
  mesh_cleanup_env
}

@test "container framework starts router and multiple nodes" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  run mesh_start_router
  [[ "$status" -eq 0 ]]

  local node_count=5
  local i
  for i in $(seq 1 "$node_count"); do
    mesh_start_node "$i"
    sleep 2
  done

  # Verify every container remains healthy enough to keep the process running.
  run mesh_assert_container_running "${MESH_PREFIX}-router"
  [[ "$status" -eq 0 ]]

  for i in $(seq 1 "$node_count"); do
    run mesh_assert_container_running "${MESH_PREFIX}-node-${i}"
    [[ "$status" -eq 0 ]]
  done

  # Wait for MCP readiness on each node.
  for i in $(seq 1 "$node_count"); do
    run mesh_wait_for_mcp_ready "${i}" 20
    [[ "$status" -eq 0 ]]
  done

  # An idle node holds its router and nothing else: members connect to one
  # another when they call, not ahead of time.
  for i in $(seq 1 "$node_count"); do
    run mesh_wait_for_attached_routers "${i}" 1 30
    [[ "$status" -eq 0 ]]
    run mesh_wait_for_node_count "${i}" 0 5
    [[ "$status" -eq 0 ]]
  done

  # Asked to, a node reaches another through the mesh.
  local node2_peer_id
  node2_peer_id="$(mesh_node_peer_id 2)"
  [[ -n "${node2_peer_id}" ]]
  run mesh_connect_peer 1 "$(mesh_node_addr 2)"
  [[ "$status" -eq 0 ]]
  run mesh_wait_for_peer_connection 1 "${node2_peer_id}" 30
  [[ "$status" -eq 0 ]]
}
