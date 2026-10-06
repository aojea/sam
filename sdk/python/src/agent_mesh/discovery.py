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

"""Service discovery as sam-node does it (internal/node/service.go): a provider
record in the mesh DHT under a key derived from the service. The lookup is a
bounded Kademlia GET_PROVIDERS walk, and a peer lookup a FIND_NODE one,
spoken directly on go-libp2p-kad-dht's protocol: py-libp2p's DHT client
(0.8 included) hardcodes the /ipfs prefix and the mesh uses /sam. An SDK
member only looks records up; it announces none."""

from __future__ import annotations

import hashlib
import logging
from dataclasses import dataclass, field
from typing import Callable, Iterable, Literal

import multiaddr
import trio
from libp2p.abc import IHost
from libp2p.custom_types import TProtocol
from libp2p.kad_dht.pb import kademlia_pb2 as kad
from libp2p.peer.id import ID
from libp2p.peer.peerinfo import PeerInfo
from libp2p.utils.varint import encode_varint_prefixed

from .auth import read_bounded_varint_prefixed_bytes
from .host import dial, open_stream

logger = logging.getLogger("agent_mesh")

# go-libp2p-kad-dht with dht.ProtocolPrefix("/sam").
DHT_PROTOCOL = TProtocol("/sam/kad/1.0.0")

ServiceType = Literal["mcp", "inference", "a2a", "egress"]

_QUERY_TIMEOUT = 5.0
_MAX_ROUNDS = 3
_MAX_PEERS_PER_ROUND = 8
MAX_DHT_MESSAGE_BYTES = 256 * 1024


@dataclass
class DiscoveredProvider:
    """A peer the DHT names as offering a service."""

    peer_id: str
    # Addresses the provider advertised; may be empty when the record carried none.
    addrs: list[str] = field(default_factory=list)


def service_key(service_type: ServiceType, name: str | None = None) -> bytes:
    """The DHT key of a service: the sha256 multihash of "sam:service:<type>[:<name>]".
    go-libp2p-kad-dht keys provider records by the multihash, not the CID."""
    parts = ["sam:service", service_type] + ([name] if name else [])
    digest = hashlib.sha256(":".join(parts).encode()).digest()
    return b"\x12\x20" + digest


def parse_service_target(target: str) -> tuple[str, str]:
    """Splits "mcp://calculator" into ("mcp", "calculator"); "" is the node's own catalog."""
    if target == "":
        return "", ""
    scheme, sep, name = target.partition("://")
    if not sep or scheme not in ("mcp", "inference", "a2a", "egress") or not name:
        raise ValueError(f"service target must look like mcp://<name>, got {target!r}")
    return scheme, name


def _peer_infos(peers: Iterable[kad.Message.Peer]) -> list[PeerInfo]:
    out = []
    for p in peers:
        try:
            addrs = [multiaddr.Multiaddr(a) for a in p.addrs]
            out.append(PeerInfo(ID(p.id), addrs))
        except Exception:  # noqa: BLE001 - a malformed entry from a peer is dropped, not fatal
            continue
    return out


async def _query(host: IHost, peer_id: ID, req: kad.Message) -> kad.Message | None:
    """One request to a peer's DHT, answered or None: a peer that does not
    serve the DHT, or does not answer in time, is skipped."""
    try:
        stream = await open_stream(host, peer_id, DHT_PROTOCOL, _QUERY_TIMEOUT)
    except Exception as err:  # noqa: BLE001 - a peer that does not serve the DHT is skipped
        logger.debug("dht: %s does not answer %s: %s", peer_id, DHT_PROTOCOL, err)
        return None
    try:
        with trio.fail_after(_QUERY_TIMEOUT):
            await stream.write(encode_varint_prefixed(req.SerializeToString()))
            resp = kad.Message.FromString(await read_bounded_varint_prefixed_bytes(stream, MAX_DHT_MESSAGE_BYTES))
    except Exception as err:  # noqa: BLE001
        logger.debug("dht: query to %s failed: %s", peer_id, err)
        return None
    finally:
        await stream.close()
    return resp if resp.type == req.type else None


async def find_peer(host: IHost, target: ID, seeds: Iterable[ID]) -> list[multiaddr.Multiaddr]:
    """The addresses the DHT knows for a peer, as go-libp2p-kad-dht's FindPeer
    learns them: a router answers FIND_NODE for a peer it is connected to with
    that peer and its addresses, its relayed one included, and names the
    routers closer to it otherwise. Empty when nobody knows the peer."""
    found: list[multiaddr.Multiaddr] = []

    def take(resp: kad.Message) -> bool:
        for info in _peer_infos(resp.closerPeers):
            if info.peer_id == target and info.addrs:
                found.extend(info.addrs)
                return True
        return False

    await _walk(host, kad.Message(type=kad.Message.FIND_NODE, key=target.to_bytes()), seeds, take, skip={target})
    return found


async def find_providers(host: IHost, key: bytes, seeds: Iterable[ID], limit: int = 20) -> list[DiscoveredProvider]:
    """Asks the seed peers (the routers we are connected to) for providers of
    key and follows the closer peers they name for a few rounds."""
    found: dict[str, DiscoveredProvider] = {}
    self_id = host.get_id()

    def take(resp: kad.Message) -> bool:
        for info in _peer_infos(resp.providerPeers):
            if info.peer_id == self_id:
                continue
            entry = found.setdefault(str(info.peer_id), DiscoveredProvider(peer_id=str(info.peer_id)))
            for ma in info.addrs:
                if str(ma) not in entry.addrs:
                    entry.addrs.append(str(ma))
        return bool(found)

    await _walk(host, kad.Message(type=kad.Message.GET_PROVIDERS, key=key), seeds, take)
    return list(found.values())[:limit]


async def _walk(host: IHost, req: kad.Message, seeds: Iterable[ID], take: Callable[[kad.Message], bool], skip: frozenset[ID] | set[ID] = frozenset()) -> None:
    """The Kademlia walk both lookups share. A round asks its peers at once
    and ends when an answer satisfies `take`; only otherwise are the closer
    peers the answers named dialed, at once as well. A round thus costs one
    query and one dial timeout, not one per peer: right after a rollout the
    routers' tables still name the pods it replaced, and the control plane
    may list a router this member cannot reach. A peer that did not answer
    its dial is not dialed again."""
    self_id = host.get_id()
    asked: set[ID] = set()
    frontier: list[ID] = list(seeds)
    for _ in range(_MAX_ROUNDS):
        batch = [p for p in frontier if p not in asked and p != self_id][:_MAX_PEERS_PER_ROUND]
        if not batch:
            return
        asked.update(batch)
        answers: list[kad.Message] = []

        async def ask(peer_id: ID) -> None:
            resp = await _query(host, peer_id, req)
            if resp is not None:
                answers.append(resp)

        async with trio.open_nursery() as nursery:
            for peer_id in batch:
                nursery.start_soon(ask, peer_id)
        # Every answer is taken: the providers each router knows add up.
        satisfied = False
        for resp in answers:
            satisfied = take(resp) or satisfied
        if satisfied:
            return

        closer: dict[ID, PeerInfo] = {}
        for resp in answers:
            for info in _peer_infos(resp.closerPeers):
                if info.peer_id not in asked and info.peer_id != self_id and info.peer_id not in skip:
                    closer.setdefault(info.peer_id, info)
        connected = set(host.get_connected_peers())
        frontier = [p for p in closer if p in connected]

        async def reach(info: PeerInfo) -> None:
            try:
                await dial(host, info)
            except Exception:  # noqa: BLE001 - unreachable closer peers are skipped
                asked.add(info.peer_id)
                return
            frontier.append(info.peer_id)

        async with trio.open_nursery() as nursery:
            for peer_id, info in closer.items():
                if peer_id not in connected:
                    nursery.start_soon(reach, info)
