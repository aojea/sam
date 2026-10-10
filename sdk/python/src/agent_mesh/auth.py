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

"""The /mesh/auth/1.0.0 handshake, both sides. Frames are varint-length-prefixed
protobufs, go-msgio style."""

from __future__ import annotations

import logging
from typing import Awaitable, Callable, Optional, Sequence

import trio
from libp2p.abc import IHost, INetStream
from libp2p.custom_types import TProtocol
from libp2p.peer.id import ID
from libp2p.utils.varint import (
    MessageTooLarge,
    ParseError,
    encode_varint_prefixed,
    read_varint_prefixed_bytes_limited,
)

from ._proto import agentmesh_pb2 as pb
from .biscuit import BiscuitVerificationError, VerifiedBiscuit, verify_peer_biscuit
from .host import open_stream

logger = logging.getLogger("agent_mesh")

AUTH_PROTOCOL = TProtocol("/mesh/auth/1.0.0")
MCP_PROTOCOL = TProtocol("/mesh/mcp/1.0.0")
GOAWAY_PROTOCOL = TProtocol("/mesh/goaway/1.0.0")

# The first frame on a stream is capped, as msgio.NewVarintReaderSize(s, 64 KiB).
MAX_AUTH_FRAME_BYTES = 64 * 1024
# How long either side waits for the other's frame.
AUTH_HANDSHAKE_TIMEOUT = 10.0

# Bounds on an inbound /mesh/goaway/1.0.0 stream, matching agentmesh-node's HandleGoAway.
MAX_GOAWAY_MESSAGE_BYTES = 1024
GOAWAY_STREAM_TIMEOUT = 5.0
ROUTER_SHUN_DURATION = 5 * 60.0
GOAWAY_MAX_RETRY_AFTER = 60 * 60.0


class AuthRejectedError(Exception):
    def __init__(self, peer_id: str, reason: str):
        super().__init__(f"peer {peer_id} rejected the auth handshake: {reason}")
        self.peer_id = peer_id
        self.reason = reason


async def read_bounded_varint_prefixed_bytes(stream: INetStream, max_bytes: int) -> bytes:
    """Reads a varint-length-prefixed frame, rejecting any length prefix above
    max_bytes before reading the frame payload."""
    try:
        return await read_varint_prefixed_bytes_limited(stream, max_bytes)
    except (MessageTooLarge, ParseError) as err:
        raise ValueError(f"frame exceeds the {max_bytes} byte cap or has invalid varint: {err}") from err


async def _read_frame(stream: INetStream) -> bytes:
    return await read_bounded_varint_prefixed_bytes(stream, MAX_AUTH_FRAME_BYTES)


async def authenticate_with_peer(host: IHost, peer_id: ID, frame: bytes, trusted_keys: Sequence[bytes]) -> VerifiedBiscuit:
    """Client side: presents `frame` on a new /mesh/auth/1.0.0 stream to a
    connected peer and returns the peer's verified credential."""
    stream = await open_stream(host, peer_id, AUTH_PROTOCOL, AUTH_HANDSHAKE_TIMEOUT)
    try:
        with trio.fail_after(AUTH_HANDSHAKE_TIMEOUT):
            await stream.write(encode_varint_prefixed(frame))
            resp = pb.AuthResponse.FromString(await _read_frame(stream))
        if not resp.success:
            raise AuthRejectedError(str(peer_id), resp.error or "no reason given")
        if not resp.biscuit:
            raise AuthRejectedError(str(peer_id), "empty credential in the mutual response")
        # Discovery names whoever answered; only this says the peer is enrolled.
        return verify_peer_biscuit(resp.biscuit, str(peer_id), trusted_keys)
    except trio.TooSlowError as err:
        raise AuthRejectedError(str(peer_id), "handshake timed out") from err
    finally:
        await stream.close()


def auth_stream_handler(
    own_biscuit: Callable[[], bytes],
    trusted_keys: Callable[[], Sequence[bytes]],
    on_authenticated: Optional[Callable[[str, VerifiedBiscuit], None]] = None,
    is_banned: Optional[Callable[[str], bool]] = None,
) -> Callable[[INetStream], "trio.lowlevel.Awaitable[None]"]:
    """Server side of /mesh/auth/1.0.0, mirroring agentmesh-node's HandleAuthHandshake:
    verify the caller's biscuit against the control plane keys and its
    connection peer ID, then answer with our own. A failed verification, or a
    peer the control plane banned, gets no answer, only a closed stream, as on
    the Go side."""

    async def handle(stream: INetStream) -> None:
        peer_id = str(stream.muxed_conn.peer_id)
        try:
            with trio.fail_after(AUTH_HANDSHAKE_TIMEOUT):
                frame = pb.AuthFrame.FromString(await _read_frame(stream))
                if is_banned is not None and is_banned(peer_id):
                    logger.warning("auth handshake from %s refused: peer is revoked", peer_id)
                    return
                try:
                    verified = verify_peer_biscuit(frame.biscuit, peer_id, trusted_keys())
                except BiscuitVerificationError as err:
                    logger.warning("auth handshake from %s refused: %s", peer_id, err)
                    return
                if on_authenticated is not None:
                    on_authenticated(peer_id, verified)
                resp = pb.AuthResponse(success=True, biscuit=own_biscuit())
                await stream.write(encode_varint_prefixed(resp.SerializeToString()))
        except (trio.TooSlowError, ValueError) as err:
            logger.warning("auth handshake from %s failed: %s", peer_id, err)
        finally:
            await stream.close()

    return handle


def go_away_reason_name(reason: int) -> str:
    """Returns the canonical proto enum name for a RouterGoAway.Reason value."""
    if reason == pb.RouterGoAway.DRAINING:
        return "DRAINING"
    if reason == pb.RouterGoAway.OVERLOADED:
        return "OVERLOADED"
    return "REASON_UNSPECIFIED"


def clamp_go_away_retry_after(msg: pb.RouterGoAway) -> float:
    """Computes the shun duration in seconds from a RouterGoAway message:
    defaults to ROUTER_SHUN_DURATION (5m) when retry_after <= 0, capped at
    GOAWAY_MAX_RETRY_AFTER (1h)."""
    seconds = 0.0
    if msg.HasField("retry_after"):
        seconds = msg.retry_after.seconds + msg.retry_after.nanos / 1_000_000_000.0
    if seconds <= 0:
        return ROUTER_SHUN_DURATION
    if seconds > GOAWAY_MAX_RETRY_AFTER:
        return GOAWAY_MAX_RETRY_AFTER
    return seconds


def go_away_stream_handler(
    is_held_router: Callable[[str], bool],
    on_go_away: Callable[[str, pb.RouterGoAway, float], Optional[Awaitable[None]]],
) -> Callable[[INetStream], "trio.lowlevel.Awaitable[None]"]:
    """Server side of /mesh/goaway/1.0.0, mirroring agentmesh-node's HandleGoAway:
    reads one varint-prefixed RouterGoAway message (1024 B cap, 5s timeout) only
    from a router the member currently holds, closes the stream, and notifies
    the session to shun the router and top up from the catalog."""

    async def handle(stream: INetStream) -> None:
        peer_id = str(stream.muxed_conn.peer_id)
        if not is_held_router(peer_id):
            logger.debug("ignoring go-away from unattached peer %s", peer_id)
            await stream.close()
            return
        try:
            with trio.fail_after(GOAWAY_STREAM_TIMEOUT):
                raw = await read_bounded_varint_prefixed_bytes(stream, MAX_GOAWAY_MESSAGE_BYTES)
                msg = pb.RouterGoAway.FromString(raw)
        except Exception as err:  # noqa: BLE001
            logger.warning("go-away read from %s failed: %s", peer_id, err)
            await stream.close()
            return
        await stream.close()
        retry_after = clamp_go_away_retry_after(msg)
        res = on_go_away(peer_id, msg, retry_after)
        if res is not None:
            await res

    return handle

