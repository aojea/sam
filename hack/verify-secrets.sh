#!/bin/bash
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

# Refuses tracked files that carry a Agent Mesh-shaped credential. Generic secret
# scanners do not know these shapes: a node API token is 32 random bytes hex
# encoded behind "Bearer", and a private key block anywhere outside a test
# fixture is a key somebody will trust (a demo recording once carried a real
# daemon token; a documented mock issuer once shipped its signing key).

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "${REPO_ROOT}"

status=0

# A bearer of 32+ hex chars is a token, not a placeholder.
if hits=$(git grep -nE 'Bearer [0-9a-f]{32,}' -- ':!hack/verify-secrets.sh'); then
  echo "Bearer tokens found in tracked files:"
  echo "${hits}"
  status=1
fi

# PEM private keys. Nothing tracked should hold one: fixtures generate theirs.
if hits=$(git grep -nE -- '-----BEGIN (RSA |EC |OPENSSH |)PRIVATE KEY-----' -- ':!hack/verify-secrets.sh'); then
  echo "Private key blocks found in tracked files:"
  echo "${hits}"
  status=1
fi

if [[ "${status}" -ne 0 ]]; then
  echo "Remove the credential, rotate it if it was ever real, and regenerate fixtures at start-up instead of committing them."
fi
exit "${status}"
