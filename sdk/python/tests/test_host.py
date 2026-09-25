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

"""What a dial must do that py-libp2p does not: resolve a /dnsaddr router
address through its TXT records and keep the transports this host has
(dial_addrs), and give up on an address nobody answers (dial). And what a
stream must do: come back after the 256th one on a connection (py-libp2p
0.7 leaks its yamux backlog slots), or fail at a deadline."""

import struct

import multiaddr
import pytest
import trio
import trio.testing
from libp2p.custom_types import TProtocol
from libp2p.peer.id import ID
from libp2p.peer.peerinfo import PeerInfo, info_from_p2p_addr
from libp2p.stream_muxer.yamux.yamux import FLAG_SYN, YAMUX_HEADER_FORMAT
from multiaddr.resolvers import DNSResolver

from agent_mesh.host import LIBP2P_LEAKS_CONNECTION_SCOPES, LIBP2P_LEAKS_STREAM_SLOTS, create_mesh_host, dial_addrs, open_stream, peer_info
from agent_mesh.identity import Identity
from agent_mesh.session import DIAL_TIMEOUT, dial

ROUTER = "12D3KooWG1pA6goegCncqwbZLSr8pnjUZ6JMAAe6SmnHTgUNCk88"
OTHER = "12D3KooWGvdRCJLYATauVWfsieF2j3a2wXZoEQJUS2MsvRdDtgLM"


class FakeTXT:
    """A dnspython answer: one TXT record per string."""

    def __init__(self, strings):
        self._strings = strings

    def __iter__(self):
        for s in self._strings:
            yield type("TXT", (), {"strings": [s.encode()]})()

    def __len__(self):
        return len(self._strings)


@pytest.fixture
def testnet_dns(monkeypatch):
    """The _dnsaddr records a testnet publishes: two routers, TCP and QUIC each."""
    records = {
        "_dnsaddr.bootstrap.example": [
            f"dnsaddr=/ip4/203.0.113.1/udp/4501/quic-v1/p2p/{ROUTER}",
            f"dnsaddr=/ip4/203.0.113.1/tcp/4501/p2p/{ROUTER}",
            f"dnsaddr=/ip4/203.0.113.2/tcp/4501/p2p/{OTHER}",
            f"dnsaddr=/ip4/203.0.113.2/udp/4501/quic-v1/p2p/{OTHER}",
        ],
        "_dnsaddr.quic-only.example": [f"dnsaddr=/ip4/203.0.113.3/udp/4501/quic-v1/p2p/{ROUTER}"],
    }

    class FakeDNS:
        async def resolve(self, name, rdtype):
            assert rdtype == "TXT"
            return FakeTXT(records.get(str(name).rstrip("."), []))

    def init(self):
        self._resolver = FakeDNS()

    monkeypatch.setattr(DNSResolver, "__init__", init)


def test_dnsaddr_router_address_resolves_to_its_tcp_address(testnet_dns):
    async def main():
        addrs = await dial_addrs(multiaddr.Multiaddr(f"/dnsaddr/bootstrap.example/p2p/{ROUTER}"))
        assert [str(a) for a in addrs] == [f"/ip4/203.0.113.1/tcp/4501/p2p/{ROUTER}"]
        info = await peer_info(multiaddr.Multiaddr(f"/dnsaddr/bootstrap.example/p2p/{ROUTER}"))
        assert str(info.peer_id) == ROUTER and len(info.addrs) == 1

    trio.run(main)


def test_addresses_without_a_transport_this_host_has_are_an_error(testnet_dns):
    async def main():
        with pytest.raises(RuntimeError, match="TCP"):
            await dial_addrs(multiaddr.Multiaddr(f"/dnsaddr/quic-only.example/p2p/{ROUTER}"))
        with pytest.raises(RuntimeError, match="TCP"):
            await dial_addrs(multiaddr.Multiaddr(f"/ip4/203.0.113.9/udp/4501/quic-v1/p2p/{ROUTER}"))
        with pytest.raises(RuntimeError, match="no address"):
            await dial_addrs(multiaddr.Multiaddr(f"/dnsaddr/nowhere.example/p2p/{ROUTER}"))

    trio.run(main)


def test_a_concrete_address_passes_through():
    async def main():
        ma = multiaddr.Multiaddr(f"/ip4/127.0.0.1/tcp/4001/p2p/{ROUTER}")
        assert await dial_addrs(ma) == [ma]

    trio.run(main)


def test_a_dial_nobody_answers_ends_at_the_timeout():
    """A provider record can name a pod a rollout replaced; its SYNs go
    unanswered. The dial ends at DIAL_TIMEOUT, not at the kernel's."""

    class Host:
        async def connect(self, info):
            await trio.sleep_forever()

    async def main():
        started = trio.current_time()
        with pytest.raises(ConnectionError, match=f"within {DIAL_TIMEOUT:g}s"):
            await dial(Host(), PeerInfo(ID.from_base58(ROUTER), []))
        assert trio.current_time() - started == pytest.approx(DIAL_TIMEOUT)

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


ECHO = TProtocol("/test/echo/1.0.0")


def test_a_connection_outlives_the_yamux_stream_backlog():
    """A member keeps one connection to its router for days and opens a
    short stream on it every few minutes (DHT provide, reservation renewal).
    py-libp2p 0.7 never returns a closed stream's slot to the connection's
    256-slot backlog, so the 257th open_stream parks forever; ours must not."""

    async def echo(stream):
        try:
            await stream.write(await stream.read(64))
        finally:
            await stream.close()

    async def main():
        server, server_listen = create_mesh_host(Identity.generate(), ["/ip4/127.0.0.1/tcp/0"])
        client, _ = create_mesh_host(Identity.generate())
        server.set_stream_handler(ECHO, echo)
        async with server.run(listen_addrs=server_listen), client.run(listen_addrs=[]):
            await client.connect(info_from_p2p_addr(multiaddr.Multiaddr(f"{server.get_addrs()[0]}")))
            for i in range(300):
                # Alternate the two ways a caller lets go of a stream.
                with trio.fail_after(5):
                    stream = await open_stream(client, server.get_id(), ECHO, 5)
                    try:
                        await stream.write(b"ping")
                        assert await stream.read(4) == b"ping"
                    finally:
                        await (stream.close() if i % 2 else stream.reset())

    trio.run(main)


def test_a_stream_open_cut_by_its_deadline_returns_the_slot():
    """py-libp2p 0.7 releases the backlog slot only for an Exception; a
    trio.Cancelled from the caller's deadline, raised while the SYN waits on
    a stalled connection, kept it. Every such timeout was one slot fewer."""

    async def main():
        server, server_listen = create_mesh_host(Identity.generate(), ["/ip4/127.0.0.1/tcp/0"])
        client, _ = create_mesh_host(Identity.generate())
        server.set_stream_handler(ECHO, lambda stream: stream.close())
        async with server.run(listen_addrs=server_listen), client.run(listen_addrs=[]):
            await client.connect(info_from_p2p_addr(multiaddr.Multiaddr(f"{server.get_addrs()[0]}")))
            mux = client.get_network().connections[server.get_id()][0].muxed_conn
            slots = mux.stream_backlog_semaphore.value
            write_frame = mux._write_frame

            async def syn_stalls(header):
                # Only the SYN of a new stream; the muxer's pings and window updates pass.
                if struct.unpack(YAMUX_HEADER_FORMAT, header[:12])[2] & FLAG_SYN:
                    await trio.sleep_forever()
                await write_frame(header)

            mux._write_frame = syn_stalls
            for _ in range(3):
                with pytest.raises(ConnectionError, match="within 0.2s"):
                    await open_stream(client, server.get_id(), ECHO, 0.2)
            mux._write_frame = write_frame
            assert mux.stream_backlog_semaphore.value == slots
            with trio.fail_after(5):
                stream = await open_stream(client, server.get_id(), ECHO, 5)
                await stream.close()
            assert mux.stream_backlog_semaphore.value == slots

    trio.run(main)


def test_open_stream_ends_at_its_timeout():
    class Host:
        async def new_stream(self, peer_id, protocols):
            await trio.sleep_forever()

    async def main():
        started = trio.current_time()
        with pytest.raises(ConnectionError, match="within 10s"):
            await open_stream(Host(), ID.from_base58(ROUTER), ECHO, 10)
        assert trio.current_time() - started == pytest.approx(10)

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def test_the_stream_slot_workaround_retires_with_libp2p_0_8():
    """When the pin moves past 0.7, delete the Yamux.open_stream patch and
    this flag in host.py, and the backlog test above."""
    import importlib.metadata

    major, minor = (int(p) for p in importlib.metadata.version("libp2p").split(".")[:2])
    assert LIBP2P_LEAKS_STREAM_SLOTS == ((major, minor) < (0, 8))


def test_the_host_runs_without_a_resource_manager_and_a_background_dialer():
    """py-libp2p 0.7 leaks a ResourceManager connection slot per connection
    and per failed security upgrade; at 1000 the manager degrades to one
    connection for good, and the host refuses every peer. And its
    AutoConnector dials every peer in the peerstore every 30s while under
    100 connections, which for a member is always. Neither runs here."""
    host, _ = create_mesh_host(Identity.generate())
    swarm = host.get_network()
    if LIBP2P_LEAKS_CONNECTION_SCOPES:
        assert swarm._resource_manager is None  # noqa: SLF001 - py-libp2p offers no getter
    assert swarm.connection_config.low_watermark == 0
    assert swarm.connection_config.min_connections == 0
    # The Swarm's own limits do not depend on the manager.
    assert swarm.connection_config.max_connections > 0
    assert swarm.connection_config.max_connections_per_peer > 0


def test_a_connection_ends_with_no_slot_held():
    """Connections come and go for a member's whole life; each must leave
    the host as it found it. While the manager is off there is no slot to
    hold; once a libp2p without the leak brings it back, the slots it
    counts must return to zero when the connections do."""

    async def main():
        server, server_listen = create_mesh_host(Identity.generate(), ["/ip4/127.0.0.1/tcp/0"])
        client, _ = create_mesh_host(Identity.generate())
        async with server.run(listen_addrs=server_listen), client.run(listen_addrs=[]):
            info = info_from_p2p_addr(multiaddr.Multiaddr(f"{server.get_addrs()[0]}"))
            for _ in range(5):
                with trio.fail_after(10):
                    await client.connect(info)
                    assert server.get_id() in client.get_connected_peers()
                    await client.disconnect(server.get_id())
                    await trio.sleep(0.05)
            assert client.get_network().get_total_connections() == 0
            manager = client.get_network()._resource_manager  # noqa: SLF001 - py-libp2p offers no getter
            if LIBP2P_LEAKS_CONNECTION_SCOPES:
                assert manager is None
            elif manager is not None:
                assert manager._current_connections == 0  # noqa: SLF001

    trio.run(main)


def test_the_connection_scope_workaround_retires_with_libp2p_0_8():
    """When the pin moves past 0.7, delete the set_resource_manager(None)
    call and this flag in host.py, and check the leak is gone upstream."""
    import importlib.metadata

    major, minor = (int(p) for p in importlib.metadata.version("libp2p").split(".")[:2])
    assert LIBP2P_LEAKS_CONNECTION_SCOPES == ((major, minor) < (0, 8))
