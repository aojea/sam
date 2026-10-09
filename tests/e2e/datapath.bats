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
  mesh_wait_for_mcp_ready 1 30

  local node1_peer_id
  node1_peer_id=$(mesh_node_peer_id 1)
  [[ -n "${node1_peer_id}" ]]

  # Start Node 2
  echo "[$(date +%T)] Starting Node 2"
  mesh_start_node 2 "--log-level debug" "${node2_cfg}"
  local node2_name="${MESH_PREFIX}-node-2"
  mesh_wait_for_mcp_ready 2 30

  local node2_peer_id
  node2_peer_id=$(mesh_node_peer_id 2)
  [[ -n "${node2_peer_id}" ]]

  # Explicitly connect Node 1 to Node 2 (DHT auto-discovery is slow/unreliable in this E2E setup)
  echo "[$(date +%T)] Explicitly connecting Node 1 to Node 2"
  run mesh_connect_peer 1 "$(mesh_node_addr 2)"
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
    \"http://${node2_name}:8080/mesh/${node1_peer_id}/mcp/http-tool/\",
    headers={\"X-Mesh-Authentication\": \"Bearer secret-token\"}
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

  # One backend process serves every caller, so the bridge owns the JSON-RPC
  # id space: a reply comes back on the POST that asked for it, carrying the
  # caller's own id, and never on a shared stream. There is no GET/SSE side —
  # the old broadcast handed every caller's tool output to every other
  # reader — so a GET is refused, which MCP Streamable HTTP permits (405).
  run docker run --rm --network "${MESH_NETWORK}" python:3.12 python3 -c "
import urllib.request, urllib.error
req = urllib.request.Request(
    \"http://${node1_name}:8080/mesh/${node2_peer_id}/mcp/stdio-tool/\",
    headers={\"X-Mesh-Authentication\": \"Bearer secret-token\"}
)
try:
    with urllib.request.urlopen(req) as response:
        print(f\"unexpected {response.status}\")
except urllib.error.HTTPError as e:
    print(f\"GET status {e.code}\")
"
  echo "GET output: $output"
  [[ "$output" == *"GET status 405"* ]]

  # The backend is `cat`: it echoes the request line, so the reply is the
  # request with the caller's id restored. A second caller using the same id
  # in flight at the same time must get its own line back, not the first's.
  local reply_a reply_b
  run docker run --rm --network "${MESH_NETWORK}" python:3.12 python3 -c "
import json, urllib.request, concurrent.futures
url = \"http://${node1_name}:8080/mesh/${node2_peer_id}/mcp/stdio-tool/\"
def call(method):
    body = json.dumps({\"jsonrpc\": \"2.0\", \"method\": method, \"id\": 1}).encode()
    req = urllib.request.Request(url, data=body, headers={
        \"X-Mesh-Authentication\": \"Bearer secret-token\",
        \"Content-Type\": \"application/json\"})
    with urllib.request.urlopen(req, timeout=20) as r:
        return r.status, json.loads(r.read())
with concurrent.futures.ThreadPoolExecutor(2) as ex:
    a, b = ex.submit(call, \"ping-a\"), ex.submit(call, \"ping-b\")
    (sa, ra), (sb, rb) = a.result(), b.result()
print(f\"A {sa} {json.dumps(ra, sort_keys=True)}\")
print(f\"B {sb} {json.dumps(rb, sort_keys=True)}\")
"
  echo "POST output: $output"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *'A 200 {"id": 1, "jsonrpc": "2.0", "method": "ping-a"}'* ]]
  [[ "$output" == *'B 200 {"id": 1, "jsonrpc": "2.0", "method": "ping-b"}'* ]]
}
