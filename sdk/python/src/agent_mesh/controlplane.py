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

"""Client for the control plane's mesh-protocol surface: protobuf over HTTP
(api/sam.proto). The operator plane (/admin/*, JSON) is out of scope."""

from __future__ import annotations

import base64
import email.utils
import ipaddress
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Callable, Mapping, Optional, Sequence, Union

from google.protobuf.timestamp_pb2 import Timestamp
from google.protobuf.unknown_fields import UnknownFieldSet

from . import challenges
from ._proto import sam_pb2 as pb
from .identity import PUBLIC_KEY_SIZE, Identity, verify_ed25519

PROTOBUF_CONTENT_TYPE = "application/x-protobuf"
HEADER_CHALLENGE_TIMESTAMP = "X-Sam-Challenge-Ts"
HEADER_CHALLENGE_SIGNATURE = "X-Sam-Challenge-Sig"
STALE_CHALLENGE_TIMESTAMP_MESSAGE = "stale or invalid challenge timestamp"

# The role a plain mesh member enrolls with (api.RoleNode).
ROLE_NODE = "sam:role:node"

# How far a signed /keys response's timestamp may drift from our clock.
KEYS_RESPONSE_FRESHNESS_MS = 5 * 60 * 1000

_MAX_RESPONSE_BYTES = 1024 * 1024

# (method, url, headers, body) -> (status, body[, response_headers]). Injection point for tests.
TransportResult = Union[tuple[int, bytes], tuple[int, bytes, Mapping[str, str]]]
Transport = Callable[[str, str, Mapping[str, str], Optional[bytes]], TransportResult]


class ControlPlaneError(Exception):
    """A non-2xx answer from the control plane."""

    def __init__(self, path: str, status: int, body: bytes):
        text = body.decode("utf-8", "replace").strip()
        super().__init__(f"control plane {path}: HTTP {status}" + (f": {text}" if text else ""))
        self.path = path
        self.status = status
        self.body = text


class EnrollmentRejectedError(Exception):
    """The control plane answered, and the answer is a refusal."""


class KeysNotTrustedError(ValueError):
    """The /keys answer is signed by no key this member trusts: every key it
    holds has left the control plane's set, so its credential cannot be
    verified or refreshed either."""


class InsecureControlPlaneURLError(Exception):
    """Plaintext http:// to a host that is not loopback; see api.ValidateControlPlaneTransport."""

    def __init__(self, url: str):
        super().__init__(
            f"plaintext http:// control plane URL to a non-loopback host: {url} "
            "(use https://, or set allow_insecure for a network you trust)"
        )


@dataclass(frozen=True)
class Enrollment:
    """What an approved enrollment hands the caller."""

    biscuit: bytes
    # Unix seconds at which the biscuit expires.
    expiration: int
    control_plane_public_key: bytes
    router_addresses: list[str] = field(default_factory=list)


@dataclass(frozen=True)
class RefreshResult:
    biscuit: bytes
    # Unix seconds.
    expiration: int


def _is_loopback_host(host: str) -> bool:
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host.strip("[]")).is_loopback
    except ValueError:
        return False


def validate_control_plane_url(raw_url: str, allow_insecure: bool = False) -> str:
    """Mirrors api.ValidateControlPlaneTransport: https, or http to loopback only."""
    parts = urllib.parse.urlsplit(raw_url)
    if parts.scheme == "https" and parts.hostname:
        return raw_url.rstrip("/")
    if parts.scheme == "http" and parts.hostname:
        if allow_insecure or _is_loopback_host(parts.hostname):
            return raw_url.rstrip("/")
        raise InsecureControlPlaneURLError(raw_url)
    if parts.scheme in ("http", "https"):
        raise ValueError(f"invalid control plane URL {raw_url!r}")
    raise ValueError(f"control plane URL {raw_url!r} must use http:// or https://")


def verify_keys_response(resp: pb.KeysResponse, trusted: Sequence[bytes], now_ms: Optional[int] = None) -> list[bytes]:
    """Returns the key set if it is fresh and at least one listed key is already
    trusted and its signature verifies. Mirrors api.VerifyKeysResponse."""
    if not trusted:
        raise ValueError("no trusted control plane key to verify /keys against")
    if len(resp.signatures) != len(resp.public_keys):
        raise ValueError(f"keys response carries {len(resp.signatures)} signatures for {len(resp.public_keys)} keys")
    now_ms = int(time.time() * 1000) if now_ms is None else now_ms
    if not resp.HasField("sign_time"):
        raise ValueError("keys response carries no sign_time")
    issued_ms = resp.sign_time.ToMilliseconds()
    if abs(now_ms - issued_ms) > KEYS_RESPONSE_FRESHNESS_MS:
        raise ValueError(f"keys response sign_time {resp.sign_time.ToJsonString()} is outside the freshness window")
    # Each signature covers the set and the signing time, deterministically
    # encoded with the signatures cleared.
    payload = pb.KeysResponse(public_keys=list(resp.public_keys), sign_time=resp.sign_time).SerializeToString(deterministic=True)
    keys: list[bytes] = []
    verified = False
    for i, (pub, sig) in enumerate(zip(resp.public_keys, resp.signatures)):
        if len(pub) != PUBLIC_KEY_SIZE:
            raise ValueError(f"keys response key {i} has invalid size {len(pub)} (expected {PUBLIC_KEY_SIZE})")
        keys.append(bytes(pub))
        if not verified and any(t == pub for t in trusted) and verify_ed25519(pub, payload, sig):
            verified = True
    if not verified:
        raise KeysNotTrustedError("keys response is not signed by any trusted control plane key")
    return keys


def _urllib_transport(timeout: float) -> Transport:
    def send(method: str, url: str, headers: Mapping[str, str], body: Optional[bytes]) -> tuple[int, bytes, Mapping[str, str]]:
        req = urllib.request.Request(url, data=body, method=method, headers=dict(headers))
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 - scheme validated by the client
                return resp.status, resp.read(_MAX_RESPONSE_BYTES + 1), dict(resp.headers)
        except urllib.error.HTTPError as err:
            resp_headers = dict(err.headers) if err.headers is not None else {}
            return err.code, err.read(_MAX_RESPONSE_BYTES + 1), resp_headers

    return send


def _parse_http_date_ms(headers: Mapping[str, str]) -> Optional[int]:
    date_value: Optional[str] = None
    for k, v in headers.items():
        if k.lower() == "date":
            date_value = v
            break
    if not date_value:
        return None
    try:
        dt = email.utils.parsedate_to_datetime(date_value)
    except (TypeError, ValueError):
        return None
    ms = int(dt.timestamp() * 1000)
    return ms if ms > 0 else None


class ControlPlaneClient:
    def __init__(
        self,
        url: str,
        *,
        allow_insecure: bool = False,
        timeout: float = 30.0,
        transport: Optional[Transport] = None,
        now_ms: Optional[Callable[[], int]] = None,
    ):
        self.url = validate_control_plane_url(url, allow_insecure)
        self._transport = transport or _urllib_transport(timeout)
        self._now_ms = now_ms or _now_ms

    def info(self) -> pb.ControlPlaneInfoResponse:
        """GET /info: OIDC settings, router addresses and the ban list. Unauthenticated."""
        return pb.ControlPlaneInfoResponse.FromString(self._request("GET", "/info"))

    def keys(self, trusted: Sequence[bytes]) -> list[bytes]:
        """GET /keys: every signing key the control plane currently trusts,
        verified against a key the caller already trusts (the enrollment key)."""
        return verify_keys_response(pb.KeysResponse.FromString(self._request("GET", "/keys")), trusted, self._now_ms())

    def enroll_bootstrap(
        self,
        identity: Identity,
        bootstrap_token: str,
        *,
        role: str = ROLE_NODE,
        labels: Optional[Mapping[str, str]] = None,
        poll_interval: Optional[float] = None,
        cancel: Optional[threading.Event] = None,
    ) -> Enrollment:
        """POST /enroll with a bootstrap token, then GET /enroll/status until an
        operator approves the request if the mesh is not on auto-approve.
        `cancel` bounds that wait; set it to stop polling."""
        raw = self._request_with_challenge(
            "POST",
            "/enroll",
            lambda ts: (
                pb.BootstrapEnrollRequest(
                    bootstrap_token=bootstrap_token,
                    peer_id=identity.peer_id,
                    public_key=identity.libp2p_public_key,
                    requested_role=role,
                    labels=dict(labels or {}),
                    challenge_unix_ms=ts,
                    challenge_signature=identity.sign(challenges.enroll_challenge(identity.peer_id, ts)),
                ).SerializeToString(),
                None,
            ),
        )
        resp = pb.BootstrapEnrollResponse.FromString(raw)
        while resp.status == pb.ENROLLMENT_STATUS_PENDING:
            wait = poll_interval if poll_interval is not None else max(1, resp.poll_interval_seconds)
            if cancel is not None:
                if cancel.wait(wait):
                    raise TimeoutError("enrollment cancelled while pending approval")
            else:
                time.sleep(wait)
            resp = self._enroll_status(identity)
        return _enrollment_from_bootstrap_response(resp)

    def _enroll_status(self, identity: Identity) -> pb.BootstrapEnrollResponse:
        path = "/enroll/status?" + urllib.parse.urlencode({"peer_id": identity.peer_id})
        body = self._request_with_challenge(
            "GET",
            path,
            lambda ts: (
                None,
                {
                    HEADER_CHALLENGE_TIMESTAMP: str(ts),
                    HEADER_CHALLENGE_SIGNATURE: base64.urlsafe_b64encode(
                        identity.sign(challenges.enroll_status_challenge(identity.peer_id, ts))
                    )
                    .rstrip(b"=")
                    .decode(),
                },
            ),
        )
        return pb.BootstrapEnrollResponse.FromString(body)

    def register(
        self,
        identity: Identity,
        jwt: str,
        *,
        role: str = ROLE_NODE,
        labels: Optional[Mapping[str, str]] = None,
    ) -> Enrollment:
        """POST /register with an OIDC ID token."""
        raw = self._request_with_challenge(
            "POST",
            "/register",
            lambda ts: (
                pb.EnrollRequest(
                    jwt=jwt,
                    peer_id=identity.peer_id,
                    public_key=identity.libp2p_public_key,
                    requested_role=role,
                    labels=dict(labels or {}),
                    challenge_unix_ms=ts,
                    challenge_signature=identity.sign(challenges.register_challenge(identity.peer_id, ts)),
                ).SerializeToString(),
                None,
            ),
        )
        resp = pb.EnrollResponse.FromString(raw)
        if resp.error_message:
            raise EnrollmentRejectedError(f"enrollment failed: {resp.error_message}")
        return _checked_enrollment(
            biscuit=resp.biscuit_token,
            expire_time=resp.expire_time if resp.HasField("expire_time") else None,
            control_plane_public_key=resp.control_plane_public_key,
            router_addresses=list(resp.router_addresses),
        )

    def refresh(self, identity: Identity, biscuit: bytes, *, jwt: Optional[str] = None) -> RefreshResult:
        """POST /refresh: trades the biscuit for a fresh one. The old one is
        spent by this call; callers must persist the result before using it."""
        auth_header = "Bearer " + base64.b64encode(biscuit).decode()
        body = self._request_with_challenge(
            "POST",
            "/refresh",
            lambda ts: (
                pb.TokenRefreshRequest(
                    challenge_unix_ms=ts,
                    challenge_signature=identity.sign(challenges.refresh_challenge(identity.peer_id, ts)),
                    peer_id=identity.peer_id,
                    jwt=jwt or "",
                ).SerializeToString(),
                {"Authorization": auth_header},
            ),
        )
        resp = pb.TokenRefreshResponse.FromString(body)
        if resp.error_message:
            raise EnrollmentRejectedError(f"refresh failed: {resp.error_message}")
        if not resp.biscuit_token:
            raise ValueError("refresh returned an empty biscuit")
        if not resp.HasField("expire_time"):
            raise ValueError("refresh response carries no expire_time")
        return RefreshResult(biscuit=resp.biscuit_token, expiration=resp.expire_time.ToSeconds())

    def policy_rules(self, identity: Identity, biscuit: bytes) -> list[str]:
        """GET /policies: the mesh policy as the Datalog rules a provider adds
        to its authorizer, one per entry, rendered by the control plane. The
        text is the contract; nothing here derives rules from roles and bindings."""
        auth_header = "Bearer " + base64.b64encode(biscuit).decode()
        body = self._request_with_challenge(
            "GET",
            "/policies",
            lambda ts: (
                None,
                {
                    "Authorization": auth_header,
                    HEADER_CHALLENGE_TIMESTAMP: str(ts),
                    HEADER_CHALLENGE_SIGNATURE: base64.urlsafe_b64encode(
                        identity.sign(challenges.policies_challenge(identity.peer_id, ts))
                    )
                    .rstrip(b"=")
                    .decode(),
                },
            ),
        )
        resp = pb.PolicyConfigGetResponse.FromString(body)
        if len(UnknownFieldSet(resp)) > 0:
            raise ValueError("control plane predates datalog_rules in its policy response; upgrade the control plane")
        return list(resp.datalog_rules)

    def _request_with_challenge(
        self,
        method: str,
        path: str,
        build: Callable[[int], tuple[Optional[bytes], Optional[Mapping[str, str]]]],
    ) -> bytes:
        first_body, first_headers = build(self._now_ms())
        status, data, resp_headers = self._fetch_once(method, path, first_body, first_headers)
        if 200 <= status < 300:
            return data
        text = data.decode("utf-8", "replace")
        if status == 401 and STALE_CHALLENGE_TIMESTAMP_MESSAGE in text:
            server_ms = _parse_http_date_ms(resp_headers)
            if server_ms is not None:
                retry_body, retry_headers = build(server_ms)
                return self._request(method, path, retry_body, retry_headers)
        raise ControlPlaneError(path, status, data)

    def _fetch_once(
        self,
        method: str,
        path: str,
        body: Optional[bytes] = None,
        headers: Optional[Mapping[str, str]] = None,
    ) -> tuple[int, bytes, Mapping[str, str]]:
        all_headers = {"Accept": PROTOBUF_CONTENT_TYPE, **(headers or {})}
        if body is not None:
            all_headers["Content-Type"] = PROTOBUF_CONTENT_TYPE
        res = self._transport(method, self.url + path, all_headers, body)
        if len(res) == 3:
            status, data, resp_headers = res
        else:
            status, data = res
            resp_headers = {}
        if len(data) > _MAX_RESPONSE_BYTES:
            raise ValueError(f"control plane {path}: response of {len(data)} bytes exceeds the {_MAX_RESPONSE_BYTES} byte limit")
        return status, data, resp_headers

    def _request(self, method: str, path: str, body: Optional[bytes] = None, headers: Optional[Mapping[str, str]] = None) -> bytes:
        status, data, _ = self._fetch_once(method, path, body, headers)
        if not 200 <= status < 300:
            raise ControlPlaneError(path, status, data)
        return data


def _now_ms() -> int:
    return int(time.time() * 1000)


def _enrollment_from_bootstrap_response(resp: pb.BootstrapEnrollResponse) -> Enrollment:
    if resp.status == pb.ENROLLMENT_STATUS_APPROVED:
        return _checked_enrollment(
            biscuit=resp.biscuit_token,
            expire_time=resp.expire_time if resp.HasField("expire_time") else None,
            control_plane_public_key=resp.control_plane_public_key,
            router_addresses=list(resp.router_addresses),
        )
    if resp.status == pb.ENROLLMENT_STATUS_REJECTED:
        raise EnrollmentRejectedError(f"enrollment rejected: {resp.error_message or 'no reason given'}")
    raise ValueError(f"unexpected enrollment status {resp.status}: {resp.error_message}")


def _checked_enrollment(
    *, biscuit: bytes, expire_time: Optional[Timestamp], control_plane_public_key: bytes, router_addresses: list[str]
) -> Enrollment:
    if not biscuit:
        raise ValueError("received empty biscuit token")
    if len(control_plane_public_key) != PUBLIC_KEY_SIZE:
        raise ValueError(
            f"received invalid control plane public key size: {len(control_plane_public_key)} bytes (expected {PUBLIC_KEY_SIZE})"
        )
    # A biscuit of unknown lifetime cannot be kept fresh; the control plane must say when it expires.
    if expire_time is None:
        raise ValueError("enrollment response carries no expire_time")
    return Enrollment(
        biscuit=biscuit,
        expiration=expire_time.ToSeconds(),
        control_plane_public_key=control_plane_public_key,
        router_addresses=router_addresses,
    )
