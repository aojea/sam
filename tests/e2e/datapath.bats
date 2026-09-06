#!/usr/bin/env bats

load "lib/container_mesh.bash"

setup() {
  mesh_setup_env
}

teardown() {
  mesh_cleanup_env
  # Cleanup any additional containers started in the test
  docker rm -f http-service sse-client >/dev/null 2>&1 || true
}

@test "Datapath: HTTP and Stdio services are reachable across nodes" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  # Start router
  mesh_start_router
  local router_name="${MESH_PREFIX}-router"
  local router_peer_id
  router_peer_id=$(cat "/tmp/${MESH_PREFIX}-router-peer-id")

  # Backends exist before the nodes: services are declared in each node's
  # configuration, there is no runtime registration endpoint.
  echo "[$(date +%T)] Starting dummy HTTP service"
  docker run -d \
    --name http-service \
    --network "${MESH_NETWORK}" \
    python:3.12 python3 -c '
from http.server import HTTPServer, BaseHTTPRequestHandler
class S(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-type", "application/json")
        self.end_headers()
        self.wfile.write(b"{\"status\":\"success\"}")
HTTPServer(("0.0.0.0", 8000), S).serve_forever()
'
  MESH_CONTAINERS+=("http-service")

  # Wait for http-service to be listening
  local i
  for ((i=0; i<30; i++)); do
    if docker run --rm --network "${MESH_NETWORK}" python:3.12 python3 -c "import urllib.request; urllib.request.urlopen('http://http-service:8000')" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done

  local node1_cfg="${BATS_TEST_TMPDIR}/node1-services.yaml"
  cat > "${node1_cfg}" <<'EOF'
version: "v1alpha1"
services:
  - type: "mcp"
    name: "http-tool"
    description: "test http service"
    target_url: "http://http-service:8000"
EOF

  local node2_cfg="${BATS_TEST_TMPDIR}/node2-services.yaml"
  cat > "${node2_cfg}" <<'EOF'
version: "v1alpha1"
services:
  - type: "mcp"
    name: "stdio-tool"
    description: "test stdio service"
    command: ["sh", "-c", "sleep 1; cat"]
EOF
  # The node container runs as a non-root user; a restrictive umask would
  # otherwise make the mounted config unreadable inside it.
  chmod 644 "${node1_cfg}" "${node2_cfg}"

  # Start Node 1
  echo "[$(date +%T)] Starting Node 1"
  mesh_start_node 1 "--log-level debug" "${node1_cfg}"
  local node1_name="${MESH_PREFIX}-node-1"
  mesh_wait_for_log "${node1_name}" "SAM Node Online" 20
  mesh_wait_for_mcp_ready 1 20
  
  local node1_peer_id
  node1_peer_id=$(docker logs "${node1_name}" 2>&1 | grep "PeerID:" | head -n 1 | awk '{print $2}' | tr -d '\r')

  # Start Node 2
  echo "[$(date +%T)] Starting Node 2"
  mesh_start_node 2 "--log-level debug" "${node2_cfg}"
  local node2_name="${MESH_PREFIX}-node-2"
  mesh_wait_for_log "${node2_name}" "SAM Node Online" 20
  mesh_wait_for_mcp_ready 2 20

  local node2_peer_id
  node2_peer_id=$(docker logs "${node2_name}" 2>&1 | grep "PeerID:" | head -n 1 | awk '{print $2}' | tr -d '\r')

  # Explicitly connect Node 1 to Node 2 (DHT auto-discovery is slow/unreliable in this E2E setup)
  echo "[$(date +%T)] Explicitly connecting Node 1 to Node 2"
  local node2_addr="/dns4/${node2_name}/tcp/5002/p2p/${node2_peer_id}"
  run mesh_connect_peer 1 "${node2_addr}"
  [[ "$status" -eq 0 ]]

  # Verify connection
  mesh_wait_for_peer_connection 1 "${node2_peer_id}" 20
  [[ "$status" -eq 0 ]]

  # 3. Test HTTP Datapath: Node 2 calls Node 1's HTTP service
  echo "[$(date +%T)] Testing HTTP Datapath from Node 2 to Node 1"
  
  local i
  for ((i=0; i<15; i++)); do
    run docker run --rm --network "${MESH_NETWORK}" python:3.12 python3 -c "
import urllib.request
req = urllib.request.Request(
    \"http://${node2_name}:8080/sam/${node1_peer_id}/mcp/http-tool/\",
    headers={\"X-Sam-Authentication\": \"Bearer secret-token\"}
)
with urllib.request.urlopen(req) as response:
    print(response.read().decode(\"utf-8\"))
"
    if [[ "$status" -eq 0 ]] && [[ "$output" == *"{\"status\":\"success\"}"* ]]; then
      break
    fi
    sleep 1
  done

  echo "HTTP Call output: $output"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"{\"status\":\"success\"}"* ]]

  # 4. Test Stdio Datapath: Node 1 calls Node 2's Stdio service
  echo "[$(date +%T)] Testing Stdio Datapath from Node 1 to Node 2"
  
  # Start SSE client in background on Node 1 targeting Node 2's service
  docker run -d \
    --name sse-client \
    --network "${MESH_NETWORK}" \
    python:3.12 python3 -c "
import urllib.request
req = urllib.request.Request(
    \"http://${node1_name}:8080/sam/${node2_peer_id}/mcp/stdio-tool/\",
    headers={\"X-Sam-Authentication\": \"Bearer secret-token\"}
)
try:
    with urllib.request.urlopen(req) as response:
        print(\"SSE Client Connected\", flush=True)
        for line in response:
            print(line.decode(\"utf-8\").strip(), flush=True)
except Exception as e:
    print(f\"Error: {e}\", flush=True)
"
  MESH_CONTAINERS+=("sse-client")

  # Wait a bit for SSE stream container to start up
  local i
  for ((i=0; i<15; i++)); do
    if docker logs sse-client 2>&1 | grep -q "SSE Client Connected"; then
        break
    fi
    sleep 1
  done

  # Send message via POST from Node 1 to Node 2's service
  test_message="{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":1}"
  run docker run --rm --network "${MESH_NETWORK}" -e MSG="${test_message}" python:3.12 python3 -c "
import urllib.request
import os
req = urllib.request.Request(
    \"http://${node1_name}:8080/sam/${node2_peer_id}/mcp/stdio-tool/\",
    data=os.environ['MSG'].encode('utf-8'),
    headers={
        \"X-Sam-Authentication\": \"Bearer secret-token\",
        \"Content-Type\": \"application/json\"
    }
)
with urllib.request.urlopen(req) as response:
    print(response.status)
"
  echo "POST status: $output"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"200"* ]]

  # Check SSE client logs for the echoed message
  local success=0
  for ((i=0; i<15; i++)); do
    run docker logs sse-client
    if [[ "$output" == *"data: ${test_message}"* ]]; then
      success=1
      break
    fi
    sleep 1
  done
  echo "SSE client logs: $output"
  [[ "$success" -eq 1 ]]
}
