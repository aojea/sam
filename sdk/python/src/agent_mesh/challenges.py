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

"""Proof-of-possession challenges on the control plane's mesh surface.
Each payload names the peer and the endpoint, so a captured signature
verifies nowhere else. Mirrors api/network.go; ts is unix milliseconds."""


def _challenge(domain: str, peer_id: str, ts: int) -> bytes:
    if not isinstance(ts, int) or ts <= 0:
        raise ValueError(f"challenge timestamp must be a positive integer, got {ts!r}")
    return f"mesh:{domain}:{peer_id}:{ts}".encode()


def enroll_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at POST /enroll (bootstrap token enrollment)."""
    return _challenge("enroll", peer_id, ts)


def enroll_status_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at GET /enroll/status while a bootstrap enrollment is pending."""
    return _challenge("enroll-status", peer_id, ts)


def register_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at POST /register (OIDC enrollment)."""
    return _challenge("register", peer_id, ts)


def refresh_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at POST /refresh."""
    return _challenge("refresh", peer_id, ts)


def policies_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at GET /policies."""
    return _challenge("policies", peer_id, ts)


def egress_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at GET /egress."""
    return _challenge("egress", peer_id, ts)


def revocations_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at GET /revocations."""
    return _challenge("revocations", peer_id, ts)


def nodes_catalog_challenge(peer_id: str, ts: int) -> bytes:
    """Signed at POST /nodes/catalog."""
    return _challenge("nodes-catalog", peer_id, ts)

