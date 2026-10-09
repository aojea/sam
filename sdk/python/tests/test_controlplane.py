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

"""A fake control plane behind an injected transport: enough of /enroll,
/enroll/status, /register, /refresh and /keys to check what the client sends
and how it treats every answer. The real one is exercised by
tests/integration/sdk_enroll_test.go."""

import base64
import email.utils
import threading
import time
import urllib.parse

import pytest

from agent_mesh import challenges
from agent_mesh._proto import agentmesh_pb2 as pb
from agent_mesh.controlplane import (
    HEADER_CHALLENGE_SIGNATURE,
    HEADER_CHALLENGE_TIMESTAMP,
    KEYS_RESPONSE_FRESHNESS_MS,
    ROLE_NODE,
    STALE_CHALLENGE_TIMESTAMP_MESSAGE,
    ControlPlaneClient,
    ControlPlaneError,
    EnrollmentRejectedError,
    InsecureControlPlaneURLError,
    validate_control_plane_url,
    verify_keys_response,
)
from agent_mesh.identity import Identity, verify_ed25519
from google.protobuf.timestamp_pb2 import Timestamp


def _ts_ms(ms: int) -> Timestamp:
    t = Timestamp()
    t.FromMilliseconds(int(ms))
    return t


def _ts_s(seconds: int) -> Timestamp:
    t = Timestamp()
    t.FromSeconds(int(seconds))
    return t

CP_KEY = Identity.generate()
BISCUIT = b"not-really-a-biscuit"


def fake_transport(routes):
    """routes: {"METHOD /path": handler(url_parts, headers, body) -> (status, bytes[, resp_headers])}"""

    def send(method, url, headers, body):
        parts = urllib.parse.urlsplit(url)
        handler = routes.get(f"{method} {parts.path}")
        if handler is None:
            return 404, f"no route for {method} {parts.path}".encode()
        return handler(parts, headers, body)

    return send


def signed_keys(signers, timestamp=None):
    """Signs a KeysResponse the way api.SignKeysResponse does."""
    ts = int(time.time() * 1000) if timestamp is None else timestamp
    unsigned = pb.KeysResponse(public_keys=[s.public_key_raw for s in signers], sign_time=_ts_ms(ts))
    payload = unsigned.SerializeToString(deterministic=True)
    return pb.KeysResponse(public_keys=list(unsigned.public_keys), sign_time=_ts_ms(ts), signatures=[s.sign(payload) for s in signers])


def test_validate_control_plane_url():
    assert validate_control_plane_url("https://hub.example/") == "https://hub.example"
    assert validate_control_plane_url("http://127.0.0.1:8080") == "http://127.0.0.1:8080"
    assert validate_control_plane_url("http://localhost") == "http://localhost"
    assert validate_control_plane_url("http://[::1]:9") == "http://[::1]:9"
    with pytest.raises(InsecureControlPlaneURLError):
        validate_control_plane_url("http://hub.example")
    assert validate_control_plane_url("http://hub.example", allow_insecure=True) == "http://hub.example"
    with pytest.raises(ValueError, match="must use http"):
        validate_control_plane_url("ftp://hub.example")
    with pytest.raises(ValueError, match="invalid control plane URL"):
        validate_control_plane_url("http://")


def test_enroll_bootstrap_sends_bound_proof_and_polls_until_approved():
    identity = Identity.generate()
    seen = []
    router_addrs = ["/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWP8iKhDf3iCMo2H3butNVfdTUtYwYWYQ75jTGnynXPFMp"]

    def enroll(parts, headers, body):
        assert headers["Content-Type"] == "application/x-protobuf"
        r = pb.BootstrapEnrollRequest.FromString(body)
        assert r.bootstrap_token == "sbt_secret"
        assert r.peer_id == identity.peer_id
        assert r.public_key == identity.libp2p_public_key
        assert r.requested_role == ROLE_NODE
        assert dict(r.labels) == {"region": "eu"}
        assert abs(r.challenge_unix_ms - time.time() * 1000) < 5000
        assert verify_ed25519(identity.public_key_raw, challenges.enroll_challenge(r.peer_id, r.challenge_unix_ms), r.challenge_signature)
        seen.append("enroll")
        return 200, pb.BootstrapEnrollResponse(status=pb.ENROLLMENT_STATUS_PENDING, poll_interval_seconds=30).SerializeToString()

    def status(parts, headers, body):
        assert urllib.parse.parse_qs(parts.query)["peer_id"] == [identity.peer_id]
        ts = int(headers[HEADER_CHALLENGE_TIMESTAMP])
        sig = base64.urlsafe_b64decode(headers[HEADER_CHALLENGE_SIGNATURE] + "==")
        assert verify_ed25519(identity.public_key_raw, challenges.enroll_status_challenge(identity.peer_id, ts), sig)
        seen.append("status")
        if seen.count("status") < 2:
            return 200, pb.BootstrapEnrollResponse(status=pb.ENROLLMENT_STATUS_PENDING, poll_interval_seconds=30).SerializeToString()
        return 200, pb.BootstrapEnrollResponse(
            status=pb.ENROLLMENT_STATUS_APPROVED,
            biscuit_token=BISCUIT,
            control_plane_public_key=CP_KEY.public_key_raw,
            router_addresses=router_addrs,
            expire_time=_ts_s(1_800_000_000),
        ).SerializeToString()

    client = ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"POST /enroll": enroll, "GET /enroll/status": status}))
    enrollment = client.enroll_bootstrap(identity, "sbt_secret", labels={"region": "eu"}, poll_interval=0.001)
    assert seen == ["enroll", "status", "status"]
    assert enrollment.biscuit == BISCUIT
    assert enrollment.control_plane_public_key == CP_KEY.public_key_raw
    assert enrollment.router_addresses == router_addrs
    assert enrollment.expiration == 1_800_000_000


def test_enroll_bootstrap_surfaces_rejection_http_error_and_empty_biscuit():
    identity = Identity.generate()

    def rejected(parts, headers, body):
        return 200, pb.BootstrapEnrollResponse(
            status=pb.ENROLLMENT_STATUS_REJECTED, error_message="Bootstrap token expired, revoked or exhausted"
        ).SerializeToString()

    with pytest.raises(EnrollmentRejectedError, match="exhausted"):
        ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"POST /enroll": rejected})).enroll_bootstrap(identity, "x")

    with pytest.raises(ControlPlaneError, match="429.*Rate limit") as excinfo:
        ControlPlaneClient(
            "http://127.0.0.1:1", transport=fake_transport({"POST /enroll": lambda *a: (429, b"Rate limit exceeded")})
        ).enroll_bootstrap(identity, "x")
    assert excinfo.value.status == 429

    def empty(parts, headers, body):
        return 200, pb.BootstrapEnrollResponse(
            status=pb.ENROLLMENT_STATUS_APPROVED, control_plane_public_key=CP_KEY.public_key_raw
        ).SerializeToString()

    with pytest.raises(ValueError, match="empty biscuit"):
        ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"POST /enroll": empty})).enroll_bootstrap(identity, "x")


def test_enroll_bootstrap_stops_polling_when_cancelled():
    identity = Identity.generate()
    pending = lambda *a: (200, pb.BootstrapEnrollResponse(status=pb.ENROLLMENT_STATUS_PENDING, poll_interval_seconds=30).SerializeToString())  # noqa: E731
    client = ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"POST /enroll": pending}))
    cancel = threading.Event()
    cancel.set()
    with pytest.raises(TimeoutError, match="cancelled"):
        client.enroll_bootstrap(identity, "x", cancel=cancel)


def test_register_carries_jwt_and_register_bound_challenge():
    identity = Identity.generate()

    def register(parts, headers, body):
        r = pb.EnrollRequest.FromString(body)
        assert r.jwt == "eyJ.fake.jwt"
        assert r.peer_id == identity.peer_id
        assert r.requested_role == "mesh:role:custom"
        assert verify_ed25519(identity.public_key_raw, challenges.register_challenge(r.peer_id, r.challenge_unix_ms), r.challenge_signature)
        return 200, pb.EnrollResponse(biscuit_token=BISCUIT, control_plane_public_key=CP_KEY.public_key_raw, expire_time=_ts_s(7)).SerializeToString()

    e = ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"POST /register": register})).register(
        identity, "eyJ.fake.jwt", role="mesh:role:custom"
    )
    assert e.biscuit == BISCUIT
    assert e.expiration == 7

    denied = lambda *a: (200, pb.EnrollResponse(error_message="role not bound").SerializeToString())  # noqa: E731
    with pytest.raises(EnrollmentRejectedError, match="role not bound"):
        ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"POST /register": denied})).register(identity, "j")


def test_refresh_presents_bearer_biscuit_and_signs_refresh_challenge():
    identity = Identity.generate()

    def refresh(parts, headers, body):
        assert headers["Authorization"] == "Bearer " + base64.b64encode(BISCUIT).decode()
        r = pb.TokenRefreshRequest.FromString(body)
        assert r.peer_id == identity.peer_id
        assert verify_ed25519(identity.public_key_raw, challenges.refresh_challenge(identity.peer_id, r.challenge_unix_ms), r.challenge_signature)
        return 200, pb.TokenRefreshResponse(biscuit_token=b"fresher-biscuit", expire_time=_ts_s(99)).SerializeToString()

    result = ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"POST /refresh": refresh})).refresh(identity, BISCUIT)
    assert result.biscuit == b"fresher-biscuit"
    assert result.expiration == 99

    with pytest.raises(ControlPlaneError) as excinfo:
        ControlPlaneClient(
            "http://127.0.0.1:1", transport=fake_transport({"POST /refresh": lambda *a: (401, b"Biscuit already rotated")})
        ).refresh(identity, BISCUIT)
    assert excinfo.value.status == 401


def test_policy_rules_presents_biscuit_and_signs_policies_challenge():
    identity = Identity.generate()
    rules = ['granted_service("mcp", "echo") <- role("mesh:role:node")']

    def policies(parts, headers, body):
        assert headers["Authorization"] == "Bearer " + base64.b64encode(BISCUIT).decode()
        ts = int(headers[HEADER_CHALLENGE_TIMESTAMP])
        sig = base64.urlsafe_b64decode(headers[HEADER_CHALLENGE_SIGNATURE] + "==")
        assert verify_ed25519(identity.public_key_raw, challenges.policies_challenge(identity.peer_id, ts), sig)
        return 200, pb.PolicyConfigGetResponse(datalog_rules=rules).SerializeToString()

    client = ControlPlaneClient("http://127.0.0.1:1", transport=fake_transport({"GET /policies": policies}))
    assert client.policy_rules(identity, BISCUIT) == rules

    assert challenges.policies_challenge("peer1", 42) == b"mesh:policies:peer1:42"
    assert challenges.egress_challenge("peer1", 42) == b"mesh:egress:peer1:42"
    assert challenges.revocations_challenge("peer1", 42) == b"mesh:revocations:peer1:42"
    assert challenges.nodes_catalog_challenge("peer1", 42) == b"mesh:nodes-catalog:peer1:42"


def test_clock_skew_retries_once_using_response_date_header_and_does_not_retry_other_401s():
    identity = Identity.generate()
    server_now_sec = int(time.time())
    server_now_ms = server_now_sec * 1000
    skewed_client_now_ms = server_now_ms + 10 * 60 * 1000
    date_header = email.utils.formatdate(server_now_sec, usegmt=True)
    rules = ['granted_service("mcp", "echo") <- true']

    refresh_calls = []
    policy_calls = []

    def refresh(parts, headers, body):
        refresh_calls.append(1)
        r = pb.TokenRefreshRequest.FromString(body)
        assert verify_ed25519(identity.public_key_raw, challenges.refresh_challenge(identity.peer_id, r.challenge_unix_ms), r.challenge_signature)
        if abs(server_now_ms - r.challenge_unix_ms) > 5 * 60 * 1000:
            return 401, STALE_CHALLENGE_TIMESTAMP_MESSAGE.encode(), {"Date": date_header}
        return 200, pb.TokenRefreshResponse(biscuit_token=b"fresher-biscuit", expire_time=_ts_s(99)).SerializeToString()

    def policies(parts, headers, body):
        policy_calls.append(1)
        ts = int(headers[HEADER_CHALLENGE_TIMESTAMP])
        sig = base64.urlsafe_b64decode(headers[HEADER_CHALLENGE_SIGNATURE] + "==")
        assert verify_ed25519(identity.public_key_raw, challenges.policies_challenge(identity.peer_id, ts), sig)
        if abs(server_now_ms - ts) > 5 * 60 * 1000:
            return 401, STALE_CHALLENGE_TIMESTAMP_MESSAGE.encode(), {"Date": date_header}
        return 200, pb.PolicyConfigGetResponse(datalog_rules=rules).SerializeToString()

    skewed_client = ControlPlaneClient(
        "http://127.0.0.1:1",
        transport=fake_transport({"POST /refresh": refresh, "GET /policies": policies}),
        now_ms=lambda: skewed_client_now_ms,
    )
    refreshed = skewed_client.refresh(identity, BISCUIT)
    assert refreshed.biscuit == b"fresher-biscuit"
    assert len(refresh_calls) == 2

    assert skewed_client.policy_rules(identity, BISCUIT) == rules
    assert len(policy_calls) == 2

    non_stale_calls = []

    def bad_sig(parts, headers, body):
        non_stale_calls.append(1)
        return 401, b"invalid challenge signature", {"Date": date_header}

    non_stale_client = ControlPlaneClient(
        "http://127.0.0.1:1",
        transport=fake_transport({"GET /policies": bad_sig}),
        now_ms=lambda: skewed_client_now_ms,
    )
    with pytest.raises(ControlPlaneError) as excinfo:
        non_stale_client.policy_rules(identity, BISCUIT)
    assert excinfo.value.status == 401
    assert len(non_stale_calls) == 1


def test_verify_keys_response_accepts_only_a_set_vouched_for_by_a_trusted_key():
    retiring = Identity.generate()
    resp = signed_keys([CP_KEY, retiring])

    assert verify_keys_response(resp, [CP_KEY.public_key_raw]) == [CP_KEY.public_key_raw, retiring.public_key_raw]
    assert verify_keys_response(resp, [retiring.public_key_raw]) == [CP_KEY.public_key_raw, retiring.public_key_raw]

    with pytest.raises(ValueError, match="no trusted control plane key"):
        verify_keys_response(resp, [])
    with pytest.raises(ValueError, match="not signed by any trusted"):
        verify_keys_response(resp, [Identity.generate().public_key_raw])
    with pytest.raises(ValueError, match="freshness window"):
        verify_keys_response(resp, [CP_KEY.public_key_raw], now_ms=resp.sign_time.ToMilliseconds() + KEYS_RESPONSE_FRESHNESS_MS + 1000)

    forged = pb.KeysResponse(public_keys=[Identity.generate().public_key_raw, retiring.public_key_raw], sign_time=resp.sign_time, signatures=list(resp.signatures))
    with pytest.raises(ValueError, match="not signed by any trusted"):
        verify_keys_response(forged, [retiring.public_key_raw])

    unsigned = pb.KeysResponse(public_keys=list(resp.public_keys), sign_time=resp.sign_time)
    with pytest.raises(ValueError, match="carries 0 signatures for 2 keys"):
        verify_keys_response(unsigned, [CP_KEY.public_key_raw])


def test_keys_fetches_and_verifies_against_the_enrollment_key():
    client = ControlPlaneClient(
        "http://127.0.0.1:1", transport=fake_transport({"GET /keys": lambda *a: (200, signed_keys([CP_KEY]).SerializeToString())})
    )
    assert client.keys([CP_KEY.public_key_raw]) == [CP_KEY.public_key_raw]
    with pytest.raises(ValueError, match="not signed by any trusted"):
        client.keys([Identity.generate().public_key_raw])

