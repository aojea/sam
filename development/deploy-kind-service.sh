#!/usr/bin/env bash
# Build, load and install a service into the kind mesh from a directory holding
# a Dockerfile and a charts/agentmesh-node values.yaml (e.g. development/examples/calc-mcp).
# --release-name deploys the same directory as several nodes; anything else
# after the directory passes to helm (e.g. --set replicaCount=3).
set -euo pipefail

[[ $# -ge 1 ]] || { echo "usage: $(basename "$0") <service-dir> [--release-name <name>] [helm args...]" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[[ -d "$1" ]] || { echo "not a directory: $1" >&2; exit 1; }
DIR="$(cd "$1" && pwd)"; shift
[[ -f "${DIR}/values.yaml" ]] || { echo "no values.yaml in ${DIR}" >&2; exit 1; }
NAME="$(basename "${DIR}")"

RELEASE="${NAME}"
HELM_ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --release-name) [[ $# -ge 2 ]] || { echo "--release-name needs a value" >&2; exit 1; }; RELEASE="$2"; shift 2 ;;
    --release-name=*) RELEASE="${1#*=}"; shift ;;
    *) HELM_ARGS+=("$1"); shift ;;
  esac
done

HELM="helm"
if ! command -v helm >/dev/null 2>&1; then
  [[ -x "${PROJECT_ROOT}/bin/helm" ]] || { echo "missing prerequisite: helm (install helm or place it in ./bin/helm)" >&2; exit 1; }
  HELM="${PROJECT_ROOT}/bin/helm"
fi

set -x
docker build -t "${NAME}:local" "${DIR}"
kind load docker-image --name agentmesh-kind "${NAME}:local"
"${HELM}" --kube-context kind-agentmesh-kind -n agentmesh-kind upgrade --install "${RELEASE}" "${PROJECT_ROOT}/charts/agentmesh-node" \
  -f "${PROJECT_ROOT}/development/kind/agentmesh-node.values.yaml" \
  -f "${DIR}/values.yaml" ${HELM_ARGS[@]+"${HELM_ARGS[@]}"}
kubectl --context kind-agentmesh-kind -n agentmesh-kind rollout status deployment \
  -l "app.kubernetes.io/name=agentmesh-node,app.kubernetes.io/instance=${RELEASE}" --timeout=180s
