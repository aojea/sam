#!/usr/bin/env bats

load "lib/container_mesh.bash"

setup() {
  mesh_setup_env
}

teardown() {
  mesh_cleanup_env
}

# assert_enrolled fails unless the control plane holds an enrollment for the
# node named <idx>, made the given way; it prints the record.
assert_enrolled() {
  local idx="$1"
  local enrollment_type="$2"
  local peer_id record
  peer_id="$(mesh_node_peer_id "${idx}")"
  [[ -n "${peer_id}" ]]
  record="$(mesh_enrolled_node "${peer_id}")"
  if [[ -z "${record}" ]]; then
    echo "control plane has no enrollment for ${peer_id}" >&2
    return 1
  fi
  [[ "$(jq -r '.enrollment_type' <<<"${record}")" == "${enrollment_type}" ]]
  [[ "$(jq -r '.role' <<<"${record}")" == "sam:role:node" ]]
  echo "${record}"
}

@test "Authentication Flow 1: Client Credentials Flow" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  run mesh_start_router
  [[ "$status" -eq 0 ]]

  # mesh_start_node uses --token-url by default, which implements Client Credentials flow
  mesh_start_node 1

  mesh_wait_for_mcp_ready 1 30
  assert_enrolled 1 OIDC
}

@test "Authentication Flow 2: Device Authorization Flow (Interactive)" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  run mesh_start_router
  [[ "$status" -eq 0 ]]

  # Run sam-node join to enroll and store identity. The sam-node image has
  # no browser to open, so the loopback flow can't complete; join falls back
  # to the OAuth 2.0 Device Authorization Grant (RFC 8628), which the mock
  # OIDC provider advertises and auto-approves on the first poll. The
  # provider serves the device grant only for the code it handed out, so an
  # enrollment is proof the device flow ran.
  local node_name="${MESH_PREFIX}-node-login"
  local router_peer_id
  router_peer_id=$(cat "/tmp/${MESH_PREFIX}-router-peer-id")

  local data_vol="${MESH_PREFIX}-data"
  docker volume create "${data_vol}"
  CLEANUP_VOLUMES+=("${data_vol}")

  # Labels are declared in the node config, never on the command line, so join
  # is the path that has to carry them into enrollment.
  local labels_config
  labels_config=$(realpath tests/e2e/fixtures/sam-node-labels.yaml)

  docker run -d --name "${node_name}-join" \
    --network "${MESH_NETWORK}" \
    $(mesh_get_add_hosts) \
    -v "${data_vol}:/data" \
    -v "${labels_config}:/etc/sam/node-config.yaml:ro" \
    "sam-node:local" \
    join --config /etc/sam/node-config.yaml --data-dir /data --insecure-control-plane "http://sam-control-plane:8080"
  MESH_CONTAINERS+=("${node_name}-join")

  # join is a command: it ends, and its exit code is its verdict.
  run timeout 60 docker wait "${node_name}-join"
  echo "join exit: $output"
  [[ "$status" -eq 0 && "$output" == "0" ]] || mesh_container_log "${node_name}-join"
  [[ "$status" -eq 0 && "$output" == "0" ]]
  docker rm -f "${node_name}-join" >/dev/null 2>&1 || true

  # Now run the node with the stored identity, on the same control plane it
  # enrolled against: a mismatch is fatal, as is a tokenless TCP sidecar. The
  # API binds all interfaces so the checks below can reach it from the network.
  docker run -d \
    --name "${node_name}" \
    --network "${MESH_NETWORK}" \
    $(mesh_get_add_hosts) \
    -v "${data_vol}:/data" \
    -e SAM_API_TOKEN="secret-token" \
    "sam-node:local" \
    run \
    --data-dir /data \
    --bind-addr "0.0.0.0:8080" \
    --control-plane "http://sam-control-plane:8080" \
    --insecure-control-plane
  MESH_CONTAINERS+=("${node_name}")

  mesh_wait_for_mcp_ready login 30

  # Enrolled by the user the provider signed in, through the device grant.
  local record
  record="$(assert_enrolled login OIDC)"
  echo "enrollment: ${record}"
  [[ "$(jq -r '.claims_json | fromjson | .sub' <<<"${record}")" == "test-user" ]]

  # The labels join declared must come back attested. /sam/identity hands back
  # the raw biscuit rather than decoded claims, so read the signed
  # label("region", "eu") fact out of its symbol table. The Unix socket is the
  # only transport that endpoint accepts besides mTLS.
  local deadline=$((SECONDS + 30))
  local evidence="" biscuit=""
  while ((SECONDS < deadline)); do
    evidence=$(docker run --rm -v "${data_vol}:/data" python:3.12 \
      curl -s --unix-socket /data/sam.sock http://localhost/sam/identity 2>&1)
    biscuit=$(echo "${evidence}" | jq -r '.biscuit // empty' 2>/dev/null)
    if [[ -n "${biscuit}" ]] && python3 -c "
import base64, sys
raw = base64.b64decode(sys.argv[1])
sys.exit(0 if b'\x05label' in raw and b'\x06region' in raw and b'\x02eu' in raw else 1)
" "${biscuit}"; then
      return 0
    fi
    sleep 1
  done
  echo "join did not carry config labels into enrollment; last evidence: ${evidence}"
  docker logs --tail 30 "${node_name}" 2>&1 || true
  return 1
}

@test "Authentication Flow 3: Workload Identity Federation (JWT Path)" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  run mesh_start_router
  [[ "$status" -eq 0 ]]

  # 1. Get a token from the mock provider the way a workload platform would
  # mint one: a real grant, not an empty POST.
  local token
  token=$(docker run --rm --network "${MESH_NETWORK}" $(mesh_get_add_hosts) "${MESH_RUNTIME_IMAGE}" \
    curl -sf --max-time 10 -X POST -d 'grant_type=client_credentials&client_id=sam-mesh-audience' \
    http://mock-oidc:18080/token | jq -r '.access_token // empty')

  [[ -n "${token}" ]]

  # 2. Save it to a file in a volume
  local token_vol="${MESH_PREFIX}-token"
  docker volume create "${token_vol}"
  CLEANUP_VOLUMES+=("${token_vol}")
  
  docker run --rm \
    -v "${token_vol}:/tokens" \
    busybox \
    sh -c "echo \"${token}\" > /tokens/sa-token"

  # 3. Run sam-node with --jwt-path
  local node_name="${MESH_PREFIX}-node-wi"
  local router_peer_id
  router_peer_id=$(cat "/tmp/${MESH_PREFIX}-router-peer-id")

  docker run -d \
    --name "${node_name}" \
    --network "${MESH_NETWORK}" \
    $(mesh_get_add_hosts) \
    -v "${token_vol}:/var/run/secrets/tokens" \
    -e SAM_API_TOKEN="secret-token" \
    "sam-node:local" \
    run \
    --bind-addr "0.0.0.0:8080" \
    --control-plane "http://sam-control-plane:8080" \
    --insecure-control-plane \
    --jwt-path "/var/run/secrets/tokens/sa-token"
  MESH_CONTAINERS+=("${node_name}")

  mesh_wait_for_mcp_ready wi 30
  assert_enrolled wi OIDC
}

@test "Authentication Flow 4: Bootstrap Token Flow (Headless)" {
  run mesh_start_mock_oidc
  [[ "$status" -eq 0 ]]

  run mesh_start_router
  [[ "$status" -eq 0 ]]

  # 1. Generate bootstrap token via control plane API running in Kubernetes
  # Create curl pod in background to request token
  kubectl --context="${KUBECONTEXT}" run curl-token-gen-node \
    --image=curlimages/curl:8.6.0 \
    --restart=Never \
    --overrides='{"spec": {"activeDeadlineSeconds": 30}}' \
    -- \
    curl -s -X POST \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer super-secret-admin-token" \
      -d '{"role": "sam:role:node", "max_usages": 1}' \
      http://sam-control-plane:8080/admin/bootstrap-tokens

  if ! kubectl --context="${KUBECONTEXT}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/curl-token-gen-node --timeout=15s; then
    echo "ERROR: Token generation pod failed! Diagnostics:"
    kubectl --context="${KUBECONTEXT}" describe pod curl-token-gen-node || true
    kubectl --context="${KUBECONTEXT}" logs pod/curl-token-gen-node || true
    kubectl --context="${KUBECONTEXT}" delete pod curl-token-gen-node --ignore-not-found || true
    exit 1
  fi

  local token_json
  token_json=$(kubectl --context="${KUBECONTEXT}" logs pod/curl-token-gen-node)
  kubectl --context="${KUBECONTEXT}" delete pod curl-token-gen-node --ignore-not-found

  local node_token
  node_token=$(echo "${token_json}" | jq -r .token)
  [[ -n "${node_token}" && "${node_token}" != "null" ]]

  # 2. Run sam-node join with the bootstrap token; it is a command whose
  # exit code is its verdict.
  local node_name="${MESH_PREFIX}-node-bootstrap"
  local data_vol="${MESH_PREFIX}-data-bootstrap"
  docker volume create "${data_vol}"
  CLEANUP_VOLUMES+=("${data_vol}")

  run docker run --name "${node_name}-join" \
    --network "${MESH_NETWORK}" \
    $(mesh_get_add_hosts) \
    -v "${data_vol}:/data" \
    "sam-node:local" \
    join --data-dir /data --bootstrap-token "${node_token}" --insecure-control-plane "http://sam-control-plane:8080"
  MESH_CONTAINERS+=("${node_name}-join")
  echo "join output: $output"
  [[ "$status" -eq 0 ]]

  # 3. Start the node container with stored identity. Without an API token
  # the TCP sidecar refuses to start, and the node with it.
  docker run -d \
    --name "${node_name}" \
    --network "${MESH_NETWORK}" \
    $(mesh_get_add_hosts) \
    -v "${data_vol}:/data" \
    -e SAM_API_TOKEN="secret-token" \
    "sam-node:local" \
    run \
    --data-dir /data \
    --bind-addr "0.0.0.0:8080" \
    --control-plane "http://sam-control-plane:8080" \
    --insecure-control-plane
  MESH_CONTAINERS+=("${node_name}")

  mesh_wait_for_mcp_ready bootstrap 30
  assert_enrolled bootstrap BOOTSTRAP
}
