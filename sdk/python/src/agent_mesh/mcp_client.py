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

"""MCP over a mesh stream, the client side of sam-node's /sam/mcp/1.0.0
(internal/node/gate.go): an AuthFrame naming the service, the provider's
AuthResponse, then JSON-RPC messages each with a varint length prefix."""

from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import AsyncIterator, Mapping, Optional, Sequence

import anyio
import mcp_types
import trio
from libp2p.abc import IHost, INetStream
from libp2p.peer.id import ID
from libp2p.utils.varint import encode_varint_prefixed
from mcp import ClientSession
from mcp.shared.message import SessionMessage

from ._proto import sam_pb2 as pb
from .auth import (
    AUTH_HANDSHAKE_TIMEOUT,
    MAX_AUTH_FRAME_BYTES,
    MCP_PROTOCOL,
    AuthRejectedError,
    read_bounded_varint_prefixed_bytes,
)
from .biscuit import BiscuitVerificationError, VerifiedBiscuit, require_role, verify_peer_biscuit
from .controlplane import ROLE_NODE
from .host import open_stream

logger = logging.getLogger("agent_mesh")

# go-msgio's default message cap, which sam-node's StreamTransport uses.
MAX_MCP_MESSAGE_BYTES = 8 * 1024 * 1024

MCP_CLIENT_INFO = mcp_types.Implementation(name="agent-mesh-sdk", version="0.1.0")


class LabelsNotSatisfiedError(Exception):
    """The provider's credential lacks a label the caller requires or a label
    of the session's egress floor, as checkPeerLabels refuses."""

    def __init__(self, peer_id: str, required: Sequence[str], what: str):
        super().__init__(f"peer {peer_id} {what}: {', '.join(required)}")


def _require_every_pair(provider: VerifiedBiscuit, required: Optional[Mapping[str, str]], what: str) -> None:
    if not required or all(provider.labels.get(k) == v for k, v in required.items()):
        return
    raise LabelsNotSatisfiedError(provider.peer_id, [f"{k}={v}" for k, v in required.items()], what)


def require_labels(provider: VerifiedBiscuit, required: Optional[Mapping[str, str]]) -> None:
    """A requirement is satisfied only when the provider attests every pair, as
    sam-node's api.LabelCheck (`check if label(k1, v1), label(k2, v2)`), the
    same rule as the egress floor. A map holds one value per key, so listing
    several pairs narrows the acceptable providers. Empty is no requirement."""
    _require_every_pair(provider, required, "does not attest every required label")


def require_egress_labels(provider: VerifiedBiscuit, required: Optional[Mapping[str, str]]) -> None:
    """The session's egress floor, sam-node's egress.require_labels: the same
    rule as require_labels, refused with a message that names the floor. Empty
    is no floor."""
    _require_every_pair(provider, required, "does not attest the egress floor")


@dataclass
class ToolInfo:
    """One entry of a provider's tool list."""

    name: str
    description: str | None = None


@dataclass
class ToolCallResult:
    """A tool call's outcome, as MCP reports it."""

    is_error: bool
    # Text content blocks, in order; other block types are left out.
    text: list[str]
    raw: mcp_types.CallToolResult


async def _read_frame(stream: INetStream) -> bytes:
    return await read_bounded_varint_prefixed_bytes(stream, MAX_MCP_MESSAGE_BYTES)


@asynccontextmanager
async def open_mcp_session(
    host: IHost,
    peer_id: ID,
    frame: bytes,
    trusted_keys: Sequence[bytes],
    *,
    required_labels: Optional[Mapping[str, str]] = None,
    egress_require_labels: Optional[Mapping[str, str]] = None,
) -> AsyncIterator[tuple[ClientSession, VerifiedBiscuit]]:
    """Opens /sam/mcp/1.0.0 to a connected provider with `frame`, this member's
    AuthFrame naming the service, verifies the provider and yields an
    initialized MCP ClientSession with the provider's credential; egress_require_labels
    is the session's, not the caller's (require_egress_labels)."""
    stream = await open_stream(host, peer_id, MCP_PROTOCOL, AUTH_HANDSHAKE_TIMEOUT)
    try:
        with trio.fail_after(AUTH_HANDSHAKE_TIMEOUT):
            await stream.write(encode_varint_prefixed(frame))
            try:
                data = await read_bounded_varint_prefixed_bytes(stream, MAX_AUTH_FRAME_BYTES)
            except ValueError as err:
                raise AuthRejectedError(str(peer_id), "oversized auth response") from err
        resp = pb.AuthResponse.FromString(data)
        if not resp.success:
            raise AuthRejectedError(str(peer_id), resp.error or "no reason given")
        try:
            provider = verify_peer_biscuit(resp.biscuit, str(peer_id), trusted_keys)
            # Only nodes host services; a router's or an admin's credential is a member, not a provider.
            require_role(provider, ROLE_NODE)
        except BiscuitVerificationError as err:
            raise AuthRejectedError(str(peer_id), f"provider credential rejected: {err}") from err
        require_labels(provider, required_labels)
        require_egress_labels(provider, egress_require_labels)
    except trio.TooSlowError as err:
        await stream.close()
        raise AuthRejectedError(str(peer_id), "handshake timed out") from err
    except BaseException:
        await stream.close()
        raise

    # The mcp client speaks over a pair of memory streams; two tasks move
    # frames between them and the libp2p stream.
    read_writer, read_stream = anyio.create_memory_object_stream[SessionMessage | Exception](0)
    write_stream, write_reader = anyio.create_memory_object_stream[SessionMessage](0)

    async def pump_in() -> None:
        try:
            async with read_writer:
                while True:
                    data = await _read_frame(stream)
                    try:
                        message = mcp_types.jsonrpc_message_adapter.validate_json(data, by_name=False)
                    except ValueError as exc:
                        await read_writer.send(exc)
                        continue
                    await read_writer.send(SessionMessage(message))
        except (anyio.ClosedResourceError, anyio.BrokenResourceError):
            pass
        except Exception as err:  # noqa: BLE001 - the stream ended; the session sees the closed read side
            logger.debug("mcp stream from %s ended: %s", peer_id, err)

    async def pump_out() -> None:
        try:
            async with write_reader:
                async for session_message in write_reader:
                    data = session_message.message.model_dump_json(by_alias=True, exclude_unset=True).encode()
                    await stream.write(encode_varint_prefixed(data))
        except (anyio.ClosedResourceError, anyio.BrokenResourceError):
            pass

    async with trio.open_nursery() as nursery:
        nursery.start_soon(pump_in)
        nursery.start_soon(pump_out)
        try:
            async with ClientSession(read_stream, write_stream, client_info=MCP_CLIENT_INFO) as session:
                await session.initialize()
                yield session, provider
        finally:
            await stream.close()
            nursery.cancel_scope.cancel()


def tool_call_result(result: mcp_types.CallToolResult) -> ToolCallResult:
    text = [c.text for c in result.content if isinstance(c, mcp_types.TextContent)]
    # mcp_types 2.x names the field is_error; 1.x named it isError.
    is_error = getattr(result, "is_error", None)
    if is_error is None:
        is_error = getattr(result, "isError", False)
    return ToolCallResult(is_error=bool(is_error), text=text, raw=result)
