#!/usr/bin/env bats

setup() {
  export SAM_NODE_BINARY="${SAM_NODE_BINARY:-./bin/sam-node}"
  export SAM_CONTROL_PLANE_BINARY="${SAM_CONTROL_PLANE_BINARY:-./bin/sam-control-plane}"
  export SAM_ROUTER_BINARY="${SAM_ROUTER_BINARY:-./bin/sam-router}"
  export MCP_CLIENT_BINARY="${MCP_CLIENT_BINARY:-./bin/mcp-client}"

  # Kept so helpers built with "go run" reuse the real module cache instead of
  # filling the sandboxed HOME with read-only files.
  export REAL_HOME="$HOME"

  export TEST_TMPDIR
  TEST_TMPDIR="$(mktemp -d)"
  export HOME="$TEST_TMPDIR/home"
  export XDG_CONFIG_HOME="$HOME/.config"
  mkdir -p "$XDG_CONFIG_HOME"


}

teardown() {
  chmod -R +w "$TEST_TMPDIR" || true
  rm -rf "$TEST_TMPDIR"
}

# socket_get issues a plain HTTP request over a Unix socket, with no token, and
# echoes the raw response. Keeping it dependency-free avoids assuming curl.
socket_get() {
  python3 - "$1" "$2" <<'PY'
import socket, sys

path, target = sys.argv[1], sys.argv[2]
conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
conn.settimeout(5)
conn.connect(path)
conn.sendall(("GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n" % target).encode())
chunks = []
while True:
    chunk = conn.recv(4096)
    if not chunk:
        break
    chunks.append(chunk)
sys.stdout.write(b"".join(chunks).decode(errors="replace"))
PY
}

# wait_for_socket blocks until the node has created its socket.
wait_for_socket() {
  local socket="$1"
  for _ in $(seq 1 50); do
    [[ -S "$socket" ]] && return 0
    sleep 0.2
  done
  return 1
}

@test "sam-node --help returns success" {
  run "$SAM_NODE_BINARY" --help
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Agent Mesh Node"* ]]
}



@test "sam-node run without identity starts unauthenticated sidecar and serves mcp" {
  # Start sam-node in background with a specific port to avoid conflicts
  "$SAM_NODE_BINARY" run --bind-addr "127.0.0.1:8085" > "$TEST_TMPDIR/unauth-node.log" 2>&1 &
  local node_pid=$!

  # Give it a moment to start
  sleep 2

  # Call get_login_instructions using mcp-client
  run "$MCP_CLIENT_BINARY" -url "http://127.0.0.1:8085/mcp" -tool "get_login_instructions" -args "{}"
  
  # Clean up
  kill "$node_pid" || true
  wait "$node_pid" || true

  # Check the output of mcp-client
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"The node is unauthenticated. Please open a regular terminal and run:"* ]]
  [[ "$output" == *"sam-node join"* ]]
  
  # Check node logs for the initial message
  local log_output
  log_output=$(cat "$TEST_TMPDIR/unauth-node.log")
  [[ "$log_output" == *"No identity found. Starting unauthenticated sidecar for enrollment over MCP"* ]]
}

@test "sam-node run answers on a Unix socket in the data dir with no token" {
  local data_dir="$TEST_TMPDIR/socket-node"
  local socket="$data_dir/sam.sock"

  "$SAM_NODE_BINARY" run --data-dir "$data_dir" --bind-addr "127.0.0.1:8087" > "$TEST_TMPDIR/socket-node.log" 2>&1 &
  local node_pid=$!

  run wait_for_socket "$socket"
  [[ "$status" -eq 0 ]]

  # The socket's permissions are the credential, so only its owner gets in.
  [[ "$(stat -c %a "$socket")" == "600" ]]

  run socket_get "$socket" "/healthz"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"200 OK"* ]]

  kill "$node_pid" || true
  wait "$node_pid" || true

  # Unlinked on shutdown, so the next start is not blocked by a leftover file.
  [[ ! -e "$socket" ]]
}

@test "sam-node run --bind-addr \"\" serves only on the Unix socket" {
  local data_dir="$TEST_TMPDIR/socket-only"
  local socket="$TEST_TMPDIR/custom.sock"

  "$SAM_NODE_BINARY" run --data-dir "$data_dir" --bind-addr "" --socket-path "$socket" > "$TEST_TMPDIR/socket-only.log" 2>&1 &
  local node_pid=$!

  run wait_for_socket "$socket"
  [[ "$status" -eq 0 ]]

  run socket_get "$socket" "/healthz"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"200 OK"* ]]

  kill "$node_pid" || true
  wait "$node_pid" || true

  # No TCP listener at all: nothing on the machine can reach the node but the
  # socket, and the default data dir socket was not created either.
  local log_output
  log_output=$(cat "$TEST_TMPDIR/socket-only.log")
  [[ "$log_output" == *"Serving the local API on Unix socket $socket"* ]]
  [[ "$log_output" != *"on TCP address"* ]]
  [[ ! -e "$data_dir/sam.sock" ]]
}

@test "sam-node run --socket-path \"\" leaves no socket behind" {
  local data_dir="$TEST_TMPDIR/no-socket"

  "$SAM_NODE_BINARY" run --data-dir "$data_dir" --bind-addr "127.0.0.1:8088" --socket-path "" > "$TEST_TMPDIR/no-socket.log" 2>&1 &
  local node_pid=$!

  local log_output=""
  for _ in $(seq 1 50); do
    log_output=$(cat "$TEST_TMPDIR/no-socket.log")
    [[ "$log_output" == *"on TCP address"* ]] && break
    sleep 0.2
  done

  kill "$node_pid" || true
  wait "$node_pid" || true

  [[ "$log_output" == *"on TCP address"* ]]
  [[ "$log_output" != *"Unix socket"* ]]
  [[ ! -e "$data_dir/sam.sock" ]]
}



@test "sam-node skill install writes the agent skill and reports it as current" {
  local skills_dir="$TEST_TMPDIR/skills"

  run "$SAM_NODE_BINARY" skill install --dir "$skills_dir"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"installed"* ]]
  [[ -f "$skills_dir/sam-mesh/SKILL.md" ]]
  grep -q "^name: sam-mesh$" "$skills_dir/sam-mesh/SKILL.md"

  run "$SAM_NODE_BINARY" skill list --dir "$skills_dir"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"up to date"* ]]
}

@test "sam-node run --daemonize without an identity points at join instead of backgrounding" {
  run "$SAM_NODE_BINARY" run --daemonize --bind-addr "127.0.0.1:8086"
  [[ "$status" -eq 1 ]]
  [[ "$output" == *"not enrolled yet"* ]]
  [[ "$output" == *"sam-node join --headless"* ]]
  [[ ! -f "$XDG_CONFIG_HOME/sam-mesh/sam-node.pid" ]]
}

@test "sam-node run --daemonize starts a background node and reports why it stopped" {
  HOME="$REAL_HOME" go run tests/e2e/gen_db.go "$XDG_CONFIG_HOME/sam-mesh/agent.db"

  # No control plane is reachable here, so the backgrounded node exits during
  # startup: the parent must notice, surface the child's log, and fail.
  run "$SAM_NODE_BINARY" run --daemonize \
    --bind-addr "127.0.0.1:8086" \
    --router-connect-timeout 2s \
    --listen /ip4/127.0.0.1/udp/0/quic-v1 --listen /ip4/127.0.0.1/tcp/0
  [[ "$status" -eq 1 ]]
  [[ "$output" == *"the background node exited during startup"* ]]
  [[ "$output" == *"Last lines of"* ]]

  # The child ran detached with a generated token and its own log.
  [[ -f "$XDG_CONFIG_HOME/sam-mesh/api-token" ]]
  [[ -s "$XDG_CONFIG_HOME/sam-mesh/sam-node.log" ]]
  [[ "$(stat -c %a "$XDG_CONFIG_HOME/sam-mesh/api-token")" == "600" ]]
}

@test "sam-node reset --all needs confirmation and then clears the data directory" {
  HOME="$REAL_HOME" go run tests/e2e/gen_db.go "$XDG_CONFIG_HOME/sam-mesh/agent.db"

  # </dev/null keeps this deterministic: with no terminal to prompt on, the
  # command must refuse rather than block.
  run "$SAM_NODE_BINARY" reset --all </dev/null
  [[ "$status" -eq 1 ]]
  [[ "$output" == *"--yes"* ]]
  [[ -f "$XDG_CONFIG_HOME/sam-mesh/agent.db" ]]

  run "$SAM_NODE_BINARY" reset --all --yes </dev/null
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Deleted all local node state"* ]]
  [[ ! -f "$XDG_CONFIG_HOME/sam-mesh/agent.db" ]]

  run "$SAM_NODE_BINARY" reset --all --yes </dev/null
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"No local node state found"* ]]
}

@test "sam-control-plane --help returns success" {
  run "$SAM_CONTROL_PLANE_BINARY" --help
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Agent Mesh - Control Plane"* ]]
}

@test "sam-router --help returns success" {
  run "$SAM_ROUTER_BINARY" --help
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Agent Mesh - libp2p Router Node"* ]]
}
