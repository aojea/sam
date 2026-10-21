#!/usr/bin/env bash
# Records the 1080p 3D Live-Execution Walkthrough for the Private DIY Enterprise Stack
# (`site/static/demo-private-diy-stack.mp4`).
#
# Starts the real `agentmesh-one` control plane, four real `agentmesh-node` sidecars,
# the SQLite ERP database, and the Vendor Compliance REST API, runs all 4 acts against
# the live Go binaries, and renders a 1920x1080 30fps 3D perspective architectural
# animation synchronized with the real execution outputs.

set -euo pipefail

EXAMPLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$EXAMPLE_DIR/../../.." && pwd)"
export BIN_DIR="${BIN_DIR:-$REPO_ROOT/bin}"
export OUT_MP4="${OUT_MP4:-/tmp/demo-private-diy-stack.mp4}"

if [[ ! -x "$BIN_DIR/agentmesh-one" || ! -x "$BIN_DIR/agentmesh-node" || ! -x "$BIN_DIR/mcp-client" ]]; then
  echo "Building Agent Mesh binaries in $BIN_DIR..."
  mkdir -p "$BIN_DIR"
  (cd "$REPO_ROOT" && go build -o "$BIN_DIR/agentmesh-one" ./cmd/agentmesh-one && go build -o "$BIN_DIR/agentmesh-node" ./cmd/agentmesh-node && go build -o "$BIN_DIR/mcp-client" ./cmd/mcp-client)
fi

python3 "$EXAMPLE_DIR/render_3d_video.py"

if cp "$OUT_MP4" "$REPO_ROOT/site/static/demo-private-diy-stack.mp4" 2>/dev/null; then
  ls -lh "$REPO_ROOT/site/static/demo-private-diy-stack.mp4"
else
  echo "Rendered MP4 is ready at $OUT_MP4"
  ls -lh "$OUT_MP4"
fi
