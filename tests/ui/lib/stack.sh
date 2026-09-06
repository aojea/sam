# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Boots a control plane and console against a throwaway sqlite DB. Sourced by
# run.sh, which drives Playwright against it, and by dev.sh, which leaves it up
# for a human. No docker or kind required.
#
# Callers set STACK_* before sourcing; everything has a default.

STACK_ADMIN_TOKEN="${STACK_ADMIN_TOKEN:-ui-test-admin-token}"
STACK_CP_ADDR="${STACK_CP_ADDR:-127.0.0.1:8090}"
STACK_CONSOLE_ADDR="${STACK_CONSOLE_ADDR:-127.0.0.1:8091}"
STACK_OIDC_PORT="${STACK_OIDC_PORT:-18090}"

STACK_CP_URL="http://${STACK_CP_ADDR}"
STACK_CONSOLE_URL="http://${STACK_CONSOLE_ADDR}"
STACK_OIDC_ISSUER="http://127.0.0.1:${STACK_OIDC_PORT}"

# Both the control plane and the console read this from the environment.
export SAM_ADMIN_TOKEN="${STACK_ADMIN_TOKEN}"

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
WORK_DIR=$(mktemp -d)
PIDS=()

# $! can name a wrapper rather than the server itself (python3 is a shim on some
# systems), so walk the descendants instead of trusting the recorded pid alone.
kill_tree() {
  local pid="$1" child
  for child in $(pgrep -P "${pid}" 2>/dev/null || true); do
    kill_tree "${child}"
  done
  kill "${pid}" 2>/dev/null || true
}

stack_cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill_tree "${pid}"
    wait "${pid}" 2>/dev/null || true
  done
  rm -rf "${WORK_DIR}"
}

wait_for() {
  local url="$1" name="$2"
  for _ in $(seq 1 50); do
    if curl -fsS -o /dev/null "${url}" 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "timed out waiting for ${name} at ${url}" >&2
  cat "${WORK_DIR}/${name}.log" >&2 || true
  return 1
}

stack_start_oidc() {
  echo "Starting mock OIDC discovery endpoint..."
  # Nothing here signs tokens: callers authenticate with the admin token or a
  # bootstrap token, so the issuer only has to satisfy the control plane's
  # startup discovery. A static document is enough.
  mkdir -p "${WORK_DIR}/oidc/.well-known"
  cat >"${WORK_DIR}/oidc/.well-known/openid-configuration" <<EOF
{
  "issuer": "${STACK_OIDC_ISSUER}",
  "authorization_endpoint": "${STACK_OIDC_ISSUER}/auth",
  "token_endpoint": "${STACK_OIDC_ISSUER}/token",
  "jwks_uri": "${STACK_OIDC_ISSUER}/keys"
}
EOF
  echo '{"keys":[]}' >"${WORK_DIR}/oidc/keys"
  python3 -m http.server "${STACK_OIDC_PORT}" --bind 127.0.0.1 --directory "${WORK_DIR}/oidc" \
    >"${WORK_DIR}/oidc.log" 2>&1 &
  PIDS+=($!)
  wait_for "${STACK_OIDC_ISSUER}/.well-known/openid-configuration" "oidc"
}

stack_start_control_plane() {
  echo "Starting control plane..."
  "${REPO_ROOT}/bin/sam-control-plane" \
    --bind-address "${STACK_CP_ADDR}" \
    --db-dsn "${WORK_DIR}/console-ui.db" \
    --issuer "${STACK_OIDC_ISSUER}" \
    --auto-approve-enrollment \
    >"${WORK_DIR}/control-plane.log" 2>&1 &
  PIDS+=($!)
  wait_for "${STACK_CP_URL}/info" "control-plane"
}

stack_start_console() {
  echo "Starting console..."
  # Served straight from the working tree, so a browser refresh picks up edits to
  # the HTML, CSS and JS without a rebuild.
  "${REPO_ROOT}/bin/sam-console" \
    --control-plane "${STACK_CP_URL}" \
    --bind-addr "${STACK_CONSOLE_ADDR}" \
    --static-dir "${REPO_ROOT}/internal/console/public" \
    >"${WORK_DIR}/console.log" 2>&1 &
  PIDS+=($!)
  wait_for "${STACK_CONSOLE_URL}/" "console"
}

stack_start() {
  stack_start_oidc
  stack_start_control_plane
  stack_start_console
}
