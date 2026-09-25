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

"""The libp2p host a member joins the mesh with, configured the way sam-node's
is (internal/node/node.go): TLS is the only security protocol and yamux the
muxer. py-libp2p is trio-based, so everything here is trio async."""

from __future__ import annotations

import importlib.metadata
import os
import re
import struct
from datetime import datetime, timedelta, timezone
from typing import Sequence

import multiaddr
import trio
from cryptography import x509
from cryptography.x509.oid import NameOID
from libp2p import new_host
from libp2p.abc import IHost, INetStream
from libp2p.crypto.ed25519 import create_new_key_pair
from libp2p.custom_types import TProtocol
from libp2p.network.config import ConnectionConfig
from libp2p.peer.id import ID
from libp2p.peer.peerinfo import PeerInfo, info_from_p2p_addr
from libp2p.security.tls.transport import PROTOCOL_ID as TLS_PROTOCOL_ID
from libp2p.security.tls.transport import IdentityConfig, TLSTransport
from libp2p.stream_muxer.exceptions import MuxedStreamError
from libp2p.stream_muxer.yamux.yamux import FLAG_SYN, TYPE_WINDOW_UPDATE, YAMUX_HEADER_FORMAT
from libp2p.stream_muxer.yamux.yamux import PROTOCOL_ID as YAMUX_PROTOCOL_ID
from libp2p.stream_muxer.yamux.yamux import Yamux, YamuxStream
from multiaddr.resolvers import DNSResolver

from .identity import Identity


def _libp2p_version() -> tuple[int, int]:
    # PEP 440 lets a pre-release follow the minor directly (0.8a1), so the
    # string is not split on dots.
    m = re.match(r"(\d+)\.(\d+)", importlib.metadata.version("libp2p"))
    return (int(m.group(1)), int(m.group(2))) if m else (0, 0)


# py-libp2p 0.7 takes one of the 256 slots of a connection's
# stream_backlog_semaphore for every outbound stream and gives it back only
# when sending the SYN fails, so after 256 streams on one connection every
# open_stream blocks forever, silently. main releases the slot when the local
# side closes the stream (libp2p/py-libp2p#1426); until that is released,
# Yamux.open_stream is replaced here with the same steps plus that release,
# and the slot is returned on any failure, a trio.Cancelled after the acquire
# included, which 0.7 (except Exception) keeps. A subclass would not do:
# MuxerMultistream.new_conn constructs Yamux by name, whatever muxer_opt says.
# Delete this block and its tests with the libp2p>=0.8 bump.
LIBP2P_LEAKS_STREAM_SLOTS = _libp2p_version() < (0, 8)

# py-libp2p 0.7's Swarm.upgrade_*_raw_conn opens a ResourceManager connection
# scope and stores it on the muxed connection, then add_conn opens a second
# one and stores it on the SwarmConn; only the second is closed when the
# connection ends. A SecurityUpgradeFailure closes the raw connection and
# raises without closing the pre-upgrade scope either. Each leaked scope is
# one of the manager's 1000 connection slots for the life of the process.
# Retire with the same bump.
LIBP2P_LEAKS_CONNECTION_SCOPES = _libp2p_version() < (0, 8)


async def _open_stream_returning_slot(self: Yamux) -> YamuxStream:
    await self.stream_backlog_semaphore.acquire()
    released = False

    def release_once() -> None:
        nonlocal released
        if not released:
            released = True
            self.stream_backlog_semaphore.release()

    stream_id: int | None = None
    try:
        async with self.streams_lock:
            if self.event_shutting_down.is_set():
                raise MuxedStreamError("Connection is shutting down")
            stream_id = self.next_stream_id
            self.next_stream_id += 2
            stream = YamuxStream(stream_id, self, True)
            self.streams[stream_id] = stream
            self.stream_buffers[stream_id] = bytearray()
            self.stream_events[stream_id] = trio.Event()
        await self._write_frame(struct.pack(YAMUX_HEADER_FORMAT, 0, TYPE_WINDOW_UPDATE, FLAG_SYN, stream_id, 0))
    except BaseException as err:
        release_once()
        if stream_id is not None:
            with trio.CancelScope(shield=True):
                async with self.streams_lock:
                    self.streams.pop(stream_id, None)
                    self.stream_buffers.pop(stream_id, None)
                    self.stream_events.pop(stream_id, None)
        if isinstance(err, Exception) and not isinstance(err, MuxedStreamError):
            raise MuxedStreamError(f"Failed to send SYN: {err}") from err
        raise

    close, reset = stream.close, stream.reset

    async def close_and_release() -> None:
        try:
            await close()
        finally:
            release_once()

    async def reset_and_release() -> None:
        try:
            await reset()
        finally:
            release_once()

    stream.close = close_and_release  # type: ignore[method-assign]
    stream.reset = reset_and_release  # type: ignore[method-assign]
    return stream


if LIBP2P_LEAKS_STREAM_SLOTS:
    Yamux.open_stream = _open_stream_returning_slot  # type: ignore[method-assign]


def _certificate_template() -> x509.CertificateBuilder:
    """The libp2p TLS certificate with nothing but the libp2p extension.
    py-libp2p's default template also adds BasicConstraints and KeyUsage,
    which the spec allows, but js-libp2p (@libp2p/tls) reads the libp2p
    extension from `extensions[0]` and refuses the certificate otherwise."""
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "libp2p")])
    not_before = datetime.now(timezone.utc) - timedelta(hours=1)
    return (
        x509.CertificateBuilder()
        .serial_number(int.from_bytes(os.urandom(8), "big"))
        .not_valid_before(not_before)
        .not_valid_after(not_before + timedelta(days=365 * 100))
        .subject_name(name)
        .issuer_name(name)
    )


def create_mesh_host(identity: Identity, listen_addrs: Sequence[str] = ()) -> tuple[IHost, list[multiaddr.Multiaddr]]:
    """Builds the host for `identity`. Run it with `async with host.run(addrs)`.
    listen_addrs are for direct connections, e.g. "/ip4/0.0.0.0/tcp/0"; an
    agent is normally reached through a router's relay instead."""
    key_pair = create_new_key_pair(identity.seed)
    # No ALPN muxer list: py-libp2p 0.7 advertises early muxer negotiation
    # but does not complete it, and go-libp2p then refuses the mux upgrade.
    # Without it the muxer is negotiated with multistream-select as before.
    tls = TLSTransport(key_pair, identity_config=IdentityConfig(cert_template=_certificate_template()))
    host = new_host(
        key_pair=key_pair,
        sec_opt={TLS_PROTOCOL_ID: tls},
        muxer_opt={TProtocol(YAMUX_PROTOCOL_ID): Yamux},
        # A member connects to its routers and to the peers it calls or that
        # call it. py-libp2p's AutoConnector would otherwise dial every peer
        # in the peerstore every 30s while under 100 connections, including
        # addresses this host has no transport for and relay addresses it
        # dials as if direct, and each failed TLS handshake leaks below.
        connection_config=ConnectionConfig(min_connections=0, low_watermark=0),
    )
    if LIBP2P_LEAKS_CONNECTION_SCOPES:
        # py-libp2p 0.7's Swarm opens two ResourceManager connection scopes
        # per connection and closes one, and none after a failed security
        # upgrade. After 1000 leaked slots the manager degrades its limit to
        # one connection and never recovers, and the host refuses every peer.
        # The Swarm's own limits (max_connections, max_connections_per_peer)
        # do not depend on the manager and stay in force.
        host.get_network().set_resource_manager(None)  # type: ignore[attr-defined]
    if str(host.get_id()) != identity.peer_id:
        raise RuntimeError(f"libp2p derived peer {host.get_id()} for identity {identity.peer_id}")
    return host, [multiaddr.Multiaddr(a) for a in listen_addrs]


async def open_stream(host: IHost, peer_id: ID, protocol: TProtocol, timeout: float) -> INetStream:
    """host.new_stream bounded by a timeout. py-libp2p bounds the protocol
    negotiation but not the muxer, and a muxer that cannot open a stream
    would otherwise park the caller forever without a word."""
    try:
        with trio.fail_after(timeout):
            return await host.new_stream(peer_id, [protocol])
    except trio.TooSlowError:
        raise ConnectionError(f"no {protocol} stream to {peer_id} within {timeout:g}s") from None


_DNS_PROTOCOLS = frozenset({"dnsaddr", "dns", "dns4", "dns6"})
# py-libp2p dials TCP only; a resolved address on another transport is noise.
_UNDIALABLE_PROTOCOLS = frozenset({"quic", "quic-v1", "ws", "wss", "webtransport", "webrtc", "webrtc-direct"})
_DNS_TIMEOUT = 10.0


async def dial_addrs(addr: multiaddr.Multiaddr) -> list[multiaddr.Multiaddr]:
    """The concrete addresses this host can dial for addr. A control plane
    hands out router addresses as `/dnsaddr/<host>/p2p/<id>`, resolved here
    through the host's `_dnsaddr` TXT records (and `/dns4`, `/dns6`, `/dns`
    through A and AAAA records) the way go-libp2p and js-libp2p do before
    dialing; py-libp2p does not, and would report no transport for them.
    Addresses on transports this host lacks are left out."""
    protocols = [p.name for p in addr.protocols()]
    if protocols and protocols[0] in _DNS_PROTOCOLS:
        resolved: list[multiaddr.Multiaddr] = []
        with trio.move_on_after(_DNS_TIMEOUT):
            resolved = list(await DNSResolver().resolve(addr))
        if not resolved:
            raise RuntimeError(f"{addr} resolved to no address")
    else:
        resolved = [addr]
    dialable: list[multiaddr.Multiaddr] = []
    for m in resolved:
        names = {p.name for p in m.protocols()}
        if "tcp" in names and not names & _UNDIALABLE_PROTOCOLS:
            dialable.append(m)
    if not dialable:
        raise RuntimeError(f"{addr} offers no TCP address; this host dials TCP only")
    return dialable


async def peer_info(addr: multiaddr.Multiaddr) -> PeerInfo:
    """The peer an address names and the concrete addresses to reach it on."""
    addrs = await dial_addrs(addr)
    return PeerInfo(info_from_p2p_addr(addrs[0]).peer_id, addrs)
