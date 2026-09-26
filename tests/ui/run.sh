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
#
# Boots a control plane and console against a throwaway sqlite DB, then runs the
# Playwright console smoke test against them. No docker or kind required.
#
# To click around that same stack by hand instead, use dev.sh.

set -o errexit
set -o nounset
set -o pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/stack.sh"
cd "${REPO_ROOT}"

trap stack_cleanup EXIT INT TERM

export SAM_ADMIN_TOKEN="${STACK_ADMIN_TOKEN}"
export SAM_CONSOLE_URL="${STACK_CONSOLE_URL}"

stack_start

# The browser SDK test (browser-sdk.spec.js) runs sdk/js in a page against
# bin/sam-one; the page and the Node examples it talks to are built here.
cd "${REPO_ROOT}/sdk/js"
npm ci --no-audit --no-fund
npm run build
npm run examples
node scripts/bundle-browser.mjs examples/browser/app.js build/browser-example

cd "${REPO_ROOT}/tests/ui"
npm ci --no-audit --no-fund
# --with-deps needs root to install OS libraries; only CI runners allow that.
if [[ -n "${CI:-}" ]]; then
  npx playwright install --with-deps chromium
else
  npx playwright install chromium
fi
npx playwright test "$@"
