#!/usr/bin/env bash
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

set -e

echo "Installing protobuf Go plugins..."
# Keep in sync with the google.golang.org/protobuf version in go.mod.
if ! command -v protoc-gen-go >/dev/null 2>&1; then
  go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
fi

resolve_protoc() {
  if [[ -n "${PROTOC:-}" ]]; then
    echo "${PROTOC}"
    return
  fi
  local p base
  while IFS= read -r p; do
    base=$(dirname "$(dirname "${p}")")
    if [[ -f "${base}/include/google/protobuf/timestamp.proto" ]]; then
      echo "${p}"
      return
    fi
  done < <(type -ap protoc 2>/dev/null || true)
  command -v protoc
}

PROTOC_BIN=$(resolve_protoc)

echo "Generating Go protobuf code..."
mkdir -p api
"${PROTOC_BIN}" --go_out=paths=source_relative:. api/agentmesh.proto

echo "Protobuf generation complete."
