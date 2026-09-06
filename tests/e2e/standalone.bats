#!/usr/bin/env bats

# Black-box CUJ for the sam-one all-in-one binary: boot on a random port
# published in the banner, join real sam-nodes through it, drive the admin CLI
# against the live server, and push a request across the dataplane from node B
# to a smoke HTTP service declared in node A's configuration (egress proxy -> router -> A).
# In-process coverage lives in tests/integration/standalone_test.go; this file
# only exercises what needs the real binaries.

setup() {
  export SAM_ONE_BINARY="${SAM_ONE_BINARY:-./bin/sam-one}"
  export SAM_NODE_BINARY="${SAM_NODE_BINARY:-./bin/sam-node}"

  export TEST_TMPDIR
  TEST_TMPDIR="$(mktemp -d)"
  export HOME="$TEST_TMPDIR/home"
  export XDG_CONFIG_HOME="$HOME/.config"
  mkdir -p "$XDG_CONFIG_HOME"

  export SAM_ONE_DATA="$TEST_TMPDIR/sam-one"
}

teardown() {
  for pid in "${BACKEND_PID:-}" "${NODE_A_PID:-}" "${NODE_B_PID:-}" "${SAM_ONE_PID:-}"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  # Give the processes a moment to release the sqlite/bbolt locks.
  wait 2>/dev/null || true
  chmod -R +w "$TEST_TMPDIR" || true
  rm -rf "$TEST_TMPDIR"
}

wait_for_http() {
  local url="$1"
  for _ in $(seq 1 100); do
    curl -sf "$url" > /dev/null 2>&1 && return 0
    sleep 0.2
  done
  return 1
}

wait_for_log() {
  local file="$1" needle="$2"
  for _ in $(seq 1 150); do
    grep -q "$needle" "$file" 2>/dev/null && return 0
    sleep 0.2
  done
  echo "timed out waiting for '$needle' in $file:" >&2
  cat "$file" >&2 || true
  return 1
}

# start_node boots a background sam-node joined via the bootstrap token; sets
# NODE_<NAME>_PID for teardown. The sidecar serves only on the Unix socket so
# two nodes on one host never fight over the default TCP bind, and loopback
# addresses must be publishable or peers on one host cannot dial each other.
# Extra arguments are passed through to the node (e.g. --config).
start_node() {
  local name="$1"
  shift
  SAM_API_TOKEN=e2e-secret "$SAM_NODE_BINARY" run \
    --control-plane "$SAM_ONE_URL" \
    --bootstrap-token "$JOIN_TOKEN" \
    --data-dir "$TEST_TMPDIR/node-$name" \
    --bind-addr= \
    --allow-loopback \
    --listen "/ip4/127.0.0.1/tcp/0" "$@" > "$TEST_TMPDIR/node-$name.log" 2>&1 &
  eval "NODE_${name^^}_PID=$!"
}

@test "sam-one boots, nodes join over the single port, and the dataplane carries a service call" {
  "$SAM_ONE_BINARY" --bind-address 127.0.0.1 --port 0 \
    --data-dir "$SAM_ONE_DATA" > "$TEST_TMPDIR/sam-one.log" 2>&1 &
  SAM_ONE_PID=$!

  # The random port is published in the banner, like the generated tokens.
  wait_for_log "$TEST_TMPDIR/sam-one.log" "Join Token:"
  grep -q "Admin Token:  sam_adm_" "$TEST_TMPDIR/sam-one.log"
  SAM_ONE_PORT="$(grep -oE 'API URL:[[:space:]]+http://[^:]+:[0-9]+' "$TEST_TMPDIR/sam-one.log" | grep -oE '[0-9]+$')"
  [[ -n "$SAM_ONE_PORT" ]]
  export SAM_ONE_URL="http://127.0.0.1:${SAM_ONE_PORT}"
  wait_for_http "$SAM_ONE_URL/healthz"

  # The embedded console is served from the same port.
  run curl -sf "$SAM_ONE_URL/console/"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"<html"* ]]

  # Two real nodes join through the single port with the persisted join token.
  JOIN_TOKEN="$(cat "$SAM_ONE_DATA/join-token")"
  [[ "$JOIN_TOKEN" == sam_tok_* ]]

  # Smoke HTTP backend on node A's host, declared as a URL-backed service in
  # node A's configuration: services only exist by declaration at startup,
  # there is no runtime registration endpoint.
  # -u: unbuffered, or the "Serving HTTP" banner never flushes to the log.
  mkdir -p "$TEST_TMPDIR/www"
  echo "sam-one dataplane ok" > "$TEST_TMPDIR/www/hello.txt"
  python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$TEST_TMPDIR/www" \
    > "$TEST_TMPDIR/backend.log" 2>&1 &
  BACKEND_PID=$!
  wait_for_log "$TEST_TMPDIR/backend.log" "Serving HTTP"
  backend_port="$(grep -oE 'port [0-9]+' "$TEST_TMPDIR/backend.log" | grep -oE '[0-9]+')"

  cat > "$TEST_TMPDIR/node-a-services.yaml" <<EOF
version: "v1alpha1"
services:
  - type: "mcp"
    name: "smoke"
    description: "e2e smoke backend"
    target_url: "http://127.0.0.1:${backend_port}"
EOF

  start_node a --config "$TEST_TMPDIR/node-a-services.yaml"
  start_node b
  wait_for_log "$TEST_TMPDIR/node-a.log" "SAM Node Online"
  wait_for_log "$TEST_TMPDIR/node-b.log" "SAM Node Online"
  peer_a="$(grep -oE 'PeerID: [A-Za-z0-9]+' "$TEST_TMPDIR/node-a.log" | head -1 | cut -d' ' -f2)"
  [[ -n "$peer_a" ]]

  sock_a="$TEST_TMPDIR/node-a/sam.sock"
  sock_b="$TEST_TMPDIR/node-b/sam.sock"
  [[ -S "$sock_a" && -S "$sock_b" ]]

  # Node B reaches the service on node A through its egress proxy: the request
  # crosses B -> router -> A over the mesh, exercising the full dataplane.
  body=""
  for _ in $(seq 1 30); do
    body="$(curl -sf --unix-socket "$sock_b" "http://localhost/sam/${peer_a}/mcp/smoke/hello.txt" || true)"
    [[ "$body" == *"sam-one dataplane ok"* ]] && break
    sleep 1
  done
  [[ "$body" == *"sam-one dataplane ok"* ]]

  # The admin CLI works against the live server using the persisted admin token.
  run "$SAM_ONE_BINARY" token create --server "$SAM_ONE_URL" \
    --data-dir "$SAM_ONE_DATA" --description "e2e token"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Token:    sam-bt-"* ]]

  run "$SAM_ONE_BINARY" token list --server "$SAM_ONE_URL" --data-dir "$SAM_ONE_DATA"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"e2e token"* ]]
  # The two node enrollments above consumed join token usages.
  join_row="$(echo "$output" | grep "sam-one join token")"
  [[ "$join_row" == *" 2/"* ]]
}
