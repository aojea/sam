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

"""The DHT walks against a fake host: what they do with the closer peers a
router names. The wire format against a real router is covered by
tests/integration/sdk_mesh_test.go."""

import multiaddr
import pytest
import trio
import trio.testing
from libp2p.kad_dht.pb import kademlia_pb2 as kad
from libp2p.peer.id import ID
from libp2p.peer.peerstore import PeerStore
from libp2p.utils.varint import encode_varint_prefixed, read_varint_prefixed_bytes

from agent_mesh.discovery import DHT_PROTOCOL, _QUERY_TIMEOUT, find_peer, find_providers
from agent_mesh.host import DIAL_TIMEOUT

SELF = ID.from_base58("12D3KooWJoJPhXqLrGAcVsGSzHMzWrmoHCWoTzVSSiT3JfR7RBVo")
ROUTER = ID.from_base58("12D3KooWG1pA6goegCncqwbZLSr8pnjUZ6JMAAe6SmnHTgUNCk88")
ROUTER_B = ID.from_base58("12D3KooWBTdQ3QQZztZFaxQSTzJx5ZSbpgM8zfs43VYzBXAFkdZm")
DARK_ROUTER = ID.from_base58("12D3KooWGvdRCJLYATauVWfsieF2j3a2wXZoEQJUS2MsvRdDtgLM")
TARGET = ID.from_base58("12D3KooWQYhTNQdmr3ArTeUHRYzFg94BKyTkoWBDWez9kSCVe2Xo")
PROVIDER = ID.from_base58("12D3KooWPZCZBAZRx1TNpPt19sZ7PKcdce8rE7gNGwhzLJtWXU9u")
GONE_POD = ID.from_base58("12D3KooWEETCw8Eb2fGFfenvEEMWhvKCe9fThJeSG8oNGrpPGp1r")
GONE_POD_2 = ID.from_base58("12D3KooWKmPdCr3BcvPAsP5mAhraWvKwCBT3M58mWe5f7TrFHSRT")


class _Stream:
    """One kad request answered from a table, as a router's DHT would."""

    def __init__(self, answer):
        self._answer = answer
        self._req = b""
        self._resp = b""

    async def write(self, data):
        self._req += data

    async def read(self, n=None):
        if self._req:
            req = kad.Message.FromString(await read_varint_prefixed_bytes(_Bytes(self._req)))
            self._req = b""
            self._resp = encode_varint_prefixed(self._answer(req).SerializeToString())
        n = len(self._resp) if n is None else n
        out, self._resp = self._resp[:n], self._resp[n:]
        return out

    async def close(self):
        pass


class _Bytes:
    def __init__(self, data):
        self._data = data

    async def read(self, n=None):
        n = len(self._data) if n is None else n
        out, self._data = self._data[:n], self._data[n:]
        return out


class _Host:
    """Connected to its routers; any other peer a router names never
    answers a SYN."""

    def __init__(self, answers):
        self._answers = answers
        self.connected = set(answers)
        self.dials = []
        self.peerstore = PeerStore()

    def get_id(self):
        return SELF

    def get_peerstore(self):
        return self.peerstore

    def get_network(self):
        return None

    def get_connected_peers(self):
        return list(self.connected)

    async def connect(self, info):
        self.dials.append(info.peer_id)
        await trio.sleep_forever()

    async def new_stream(self, peer_id, protocols):
        assert protocols == [DHT_PROTOCOL]
        assert peer_id in self.connected
        return _Stream(self._answers[peer_id])

    async def disconnect(self, peer_id):
        self.connected.discard(peer_id)


def _peer(peer_id, *addrs):
    return kad.Message.Peer(id=peer_id.to_bytes(), addrs=[multiaddr.Multiaddr(a).to_bytes() for a in addrs])


def _names_the_dark_router(req):
    return kad.Message(type=req.type, closerPeers=[_peer(DARK_ROUTER, "/ip4/203.0.113.7/tcp/4501")])


def test_find_providers_dials_an_unanswering_closer_peer_once():
    """A router the control plane lists may be dark from where a member runs
    (a public address a network policy drops). Every router names it as
    closer to every key; the walk dials it once, bounded, then moves on."""
    host = _Host({ROUTER: _names_the_dark_router})

    async def main():
        started = trio.current_time()
        assert await find_providers(host, b"key", [ROUTER]) == []
        assert host.dials == [DARK_ROUTER]
        assert trio.current_time() - started == pytest.approx(DIAL_TIMEOUT)

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def test_find_peer_dials_an_unanswering_closer_peer_once():
    host = _Host({ROUTER: _names_the_dark_router})

    async def main():
        started = trio.current_time()
        assert await find_peer(host, TARGET, [ROUTER]) == []
        assert host.dials == [DARK_ROUTER]
        assert trio.current_time() - started == pytest.approx(DIAL_TIMEOUT)

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def test_find_peer_returns_the_target_a_router_knows():
    def knows_target(req):
        return kad.Message(
            type=req.type,
            closerPeers=[_peer(TARGET, "/ip4/10.0.0.9/tcp/4001"), _peer(DARK_ROUTER, "/ip4/203.0.113.7/tcp/4501")],
        )

    host = _Host({ROUTER: knows_target})

    async def main():
        addrs = await find_peer(host, TARGET, [ROUTER])
        assert [str(a) for a in addrs] == ["/ip4/10.0.0.9/tcp/4001"]
        assert host.dials == []

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def _after_a_rollout(req):
    """A router's answer minutes after a deploy: the provider, and a table
    that still names the pods the rollout replaced."""
    return kad.Message(
        type=req.type,
        providerPeers=[_peer(PROVIDER, "/ip4/10.84.3.81/tcp/5002")],
        closerPeers=[
            _peer(GONE_POD, "/ip4/10.84.1.7/tcp/5002"),
            _peer(GONE_POD_2, "/ip4/10.84.2.9/tcp/5002"),
            _peer(DARK_ROUTER, "/ip4/203.0.113.7/tcp/4501"),
        ],
    )


def test_providers_in_hand_end_the_walk_before_any_closer_peer_is_dialed():
    """The probe runs right after every canary was rolled: the routers'
    tables are full of addresses nobody answers on. The first round already
    has the provider; nothing else is dialed."""
    host = _Host({ROUTER: _after_a_rollout, ROUTER_B: _after_a_rollout})

    async def main():
        started = trio.current_time()
        found = await find_providers(host, b"key", [ROUTER, ROUTER_B])
        assert [p.peer_id for p in found] == [str(PROVIDER)]
        assert host.dials == []
        assert trio.current_time() - started == 0

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def test_stale_closer_peers_are_dialed_at_once_not_one_after_another():
    """When nobody has the record, the walk follows the closer peers. Three
    that never answer cost one dial timeout together, not three."""

    def knows_nothing(req):
        return kad.Message(type=req.type, closerPeers=_after_a_rollout(req).closerPeers)

    host = _Host({ROUTER: knows_nothing, ROUTER_B: knows_nothing})

    async def main():
        started = trio.current_time()
        assert await find_providers(host, b"key", [ROUTER, ROUTER_B]) == []
        assert sorted(map(str, host.dials)) == sorted(map(str, [GONE_POD, GONE_POD_2, DARK_ROUTER]))
        assert trio.current_time() - started == pytest.approx(DIAL_TIMEOUT)

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def test_the_routers_are_asked_at_once():
    """A router that takes its time to answer does not delay the others'
    answers: the round lasts as long as the slowest query, bounded."""

    class SlowHost(_Host):
        async def new_stream(self, peer_id, protocols):
            if peer_id == ROUTER_B:
                await trio.sleep_forever()
            return await super().new_stream(peer_id, protocols)

    host = SlowHost({ROUTER: _after_a_rollout, ROUTER_B: _after_a_rollout})

    async def main():
        started = trio.current_time()
        found = await find_providers(host, b"key", [ROUTER, ROUTER_B])
        assert [p.peer_id for p in found] == [str(PROVIDER)]
        # ROUTER_B's stream open ran to its timeout while ROUTER answered.
        assert trio.current_time() - started == pytest.approx(_QUERY_TIMEOUT)

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def test_read_bounded_varint_prefixed_bytes_rejects_oversized_prefix_before_reading_payload():
    from libp2p.utils.varint import encode_uvarint

    from agent_mesh.auth import read_bounded_varint_prefixed_bytes

    class _BombStream:
        def __init__(self, header: bytes):
            self._header = header
            self.payload_reads = 0

        async def read(self, n=None):
            if self._header:
                out, self._header = self._header[:1], self._header[1:]
                return out
            self.payload_reads += 1
            return b"x" * (n or 1)

    async def main():
        bomb = _BombStream(encode_uvarint(2 * 1024 * 1024 * 1024))
        with pytest.raises(ValueError, match="exceeds"):
            await read_bounded_varint_prefixed_bytes(bomb, 64 * 1024)
        assert bomb.payload_reads == 0

    trio.run(main)

