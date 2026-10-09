// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Proof-of-possession challenges on the control plane's mesh surface.
// Each payload names the peer and the endpoint, so a captured signature
// verifies nowhere else. Mirrors api/network.go; ts is unix milliseconds.

const encoder = new TextEncoder();

function challenge(domain: string, peerId: string, ts: number): Uint8Array {
  if (!Number.isSafeInteger(ts) || ts <= 0) {
    throw new Error(`challenge timestamp must be a positive integer, got ${ts}`);
  }
  return encoder.encode(`mesh:${domain}:${peerId}:${ts}`);
}

/** Signed at POST /enroll (bootstrap token enrollment). */
export function enrollChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("enroll", peerId, ts);
}

/** Signed at GET /enroll/status while a bootstrap enrollment is pending. */
export function enrollStatusChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("enroll-status", peerId, ts);
}

/** Signed at POST /register (OIDC enrollment). */
export function registerChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("register", peerId, ts);
}

/** Signed at POST /refresh. */
export function refreshChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("refresh", peerId, ts);
}

/** Signed at GET /policies. */
export function policiesChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("policies", peerId, ts);
}

/** Signed at GET /egress. */
export function egressChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("egress", peerId, ts);
}

/** Signed at GET /revocations. */
export function revocationsChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("revocations", peerId, ts);
}

/** Signed at POST /nodes/catalog. */
export function nodesCatalogChallenge(peerId: string, ts: number): Uint8Array {
  return challenge("nodes-catalog", peerId, ts);
}

