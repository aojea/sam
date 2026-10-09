#!/usr/bin/env bats

load "lib/container_mesh.bash"

setup() {
  mesh_setup_env
}

teardown() {
  if [[ "${BATS_TEST_COMPLETED:-0}" -ne 1 ]]; then
    echo "Node 1 logs on failure (filtered):"
    docker logs "${MESH_PREFIX}-node-1" 2>&1 | grep -i -E 'mcp|request|error|fatal|panic' || true
  fi
  mesh_cleanup_env
}

@test "Docs Snippets: agent_demo.py runs successfully" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  run mesh_start_router
  [[ "$status" -eq 0 ]]

  mesh_start_node 1 "--log-level debug"

  local node1_name="${MESH_PREFIX}-node-1"
  mesh_wait_for_mcp_ready 1 30

  # Run the agent_demo.py snippet inside a container
  run docker run --rm \
    --network "${MESH_NETWORK}" \
    -v "$(pwd)/site/content/docs/snippets:/snippets" \
    -e AGENTMESH_MCP_URL="http://${node1_name}:8080/mcp" \
    -e AGENTMESH_API_TOKEN="secret-token" \
    python:3.12 \
    bash -c 'pip install "mcp>=2,<3" httpx && python3 /snippets/agent_demo.py'

  echo "agent_demo.py output: $output"

  if [[ "$status" -ne 0 ]]; then
    echo "Node 1 logs:"
    docker logs "${node1_name}"
  fi

  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Connecting to Agent Mesh Node at"* ]]
  [[ "$output" == *"Discovered"* ]]
  [[ "$output" == *"Calling get_mesh_info tool..."* ]]
  [[ "$output" == *"Result:"* ]]
}
