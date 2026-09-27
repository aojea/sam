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

import ssl
import struct

import multiaddr
import pytest
import trio
import trio.testing
from libp2p.custom_types import TProtocol
from libp2p.peer.id import ID
from libp2p.peer.peerinfo import PeerInfo, info_from_p2p_addr
from libp2p.stream_muxer.yamux.yamux import FLAG_SYN, YAMUX_HEADER_FORMAT
from libp2p.transport.websocket.transport import WebsocketTransport
from multiaddr.resolvers import DNSResolver

from agent_mesh.host import DIAL_TIMEOUT, HANGUP_GRACE, create_mesh_host, dial, dial_addrs, open_stream, peer_info
from agent_mesh.identity import Identity

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
        for text in (f"/ip4/127.0.0.1/tcp/4001/p2p/{ROUTER}", f"/ip4/127.0.0.1/tcp/8080/ws/p2p/{ROUTER}", f"/dns4/mesh.example/tcp/443/tls/ws/p2p/{ROUTER}"):
            ma = multiaddr.Multiaddr(text)
            assert await dial_addrs(ma) == [ma]

    trio.run(main)


def test_a_websocket_address_is_dialed_by_its_name():
    """A TLS-terminating edge in front of the router (sam-one behind a tunnel)
    selects the origin by the name in the TLS SNI and the Host header.
    py-libp2p resolves the name first and sends the IP (libp2p/py-libp2p#1549
    not yet released); the host sends the name. Pinned on the Host header of
    the upgrade request, the SNI is the same string."""
    seen: dict[str, str] = {}

    async def record(stream):
        seen["request"] = (await stream.receive_some(4096)).decode(errors="replace")
        await stream.aclose()

    async def main():
        listeners = await trio.open_tcp_listeners(0, host="127.0.0.1")
        port = listeners[0].socket.getsockname()[1]
        client, _ = create_mesh_host(Identity.generate())
        async with trio.open_nursery() as nursery:
            nursery.start_soon(trio.serve_listeners, record, listeners)
            async with client.run(listen_addrs=[]):
                addr = multiaddr.Multiaddr(f"/dns4/localhost/tcp/{port}/ws/p2p/{ROUTER}")
                assert await dial_addrs(addr) == [addr]
                with pytest.raises(Exception), trio.fail_after(10):
                    await client.connect(await peer_info(addr))
            nursery.cancel_scope.cancel()
        assert f"\r\nhost: localhost:{port}\r\n" in seen["request"].lower(), seen["request"]

    trio.run(main)


def test_a_websocket_listener_is_reached_with_tls_and_yamux():
    """sam-one's router listens on /ws alone, on the port that also serves
    its HTTP API; a member reaches it as it reaches a TCP router."""

    async def echo(stream):
        try:
            await stream.write(await stream.read(64))
        finally:
            await stream.close()

    async def main():
        server, server_listen = create_mesh_host(Identity.generate(), ["/ip4/127.0.0.1/tcp/0/ws"])
        client, _ = create_mesh_host(Identity.generate())
        server.set_stream_handler(ECHO, echo)
        async with server.run(listen_addrs=server_listen), client.run(listen_addrs=[]):
            addr = server.get_addrs()[0]
            assert "/ws/p2p/" in str(addr), f"listening on {addr}"
            with trio.fail_after(10):
                await client.connect(await peer_info(addr))
                stream = await open_stream(client, server.get_id(), ECHO, 5)
                try:
                    await stream.write(b"ping")
                    assert await stream.read(4) == b"ping"
                finally:
                    await stream.close()

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
    py-libp2p 0.7 never returned a closed stream's slot to the connection's
    256-slot backlog, so the 257th open_stream parked forever; 0.8 returns
    it, and this holds it to that."""

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


def test_a_stream_open_cut_by_its_deadline_ends_the_connection():
    """py-libp2p's yamux gives a backlog slot back for an Exception while
    sending the SYN, not for the trio.Cancelled of the caller's deadline: a
    SYN that stalls on a dead connection would keep one of the 256 slots
    each time. A connection that cannot open a stream in time is no path to
    the peer, so open_stream hangs up on it, slots and all, and the next
    dial starts afresh."""

    async def main():
        server, server_listen = create_mesh_host(Identity.generate(), ["/ip4/127.0.0.1/tcp/0"])
        client, _ = create_mesh_host(Identity.generate())
        server.set_stream_handler(ECHO, lambda stream: stream.close())
        async with server.run(listen_addrs=server_listen), client.run(listen_addrs=[]):
            info = info_from_p2p_addr(multiaddr.Multiaddr(f"{server.get_addrs()[0]}"))
            await client.connect(info)
            mux = client.get_network().connections[server.get_id()][0].muxed_conn
            write_frame = mux._write_frame

            async def syn_stalls(header):
                # Only the SYN of a new stream; the muxer's pings and window updates pass.
                if struct.unpack(YAMUX_HEADER_FORMAT, header[:12])[2] & FLAG_SYN:
                    await trio.sleep_forever()
                await write_frame(header)

            mux._write_frame = syn_stalls
            with pytest.raises(ConnectionError, match="within 0.2s"):
                await open_stream(client, server.get_id(), ECHO, 0.2)
            assert server.get_id() not in client.get_connected_peers()

            # A fresh connection has every slot.
            await client.connect(info)
            mux = client.get_network().connections[server.get_id()][0].muxed_conn
            slots = mux.stream_backlog_semaphore.value
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


def test_hanging_up_on_a_stalled_connection_is_bounded_too():
    """Closing the connection writes a GO_AWAY on the very connection whose
    SYN stalled; on a blackholed peer that write parks as well. The caller
    gets its ConnectionError a moment after its timeout, not never."""

    class Host:
        async def new_stream(self, peer_id, protocols):
            await trio.sleep_forever()

        async def disconnect(self, peer_id):
            await trio.sleep_forever()

    async def main():
        started = trio.current_time()
        with pytest.raises(ConnectionError, match="within 10s"):
            await open_stream(Host(), ID.from_base58(ROUTER), ECHO, 10)
        assert trio.current_time() - started == pytest.approx(10 + HANGUP_GRACE)

    trio.run(main, clock=trio.testing.MockClock(autojump_threshold=0))


def test_wss_is_dialed_with_certificate_verification():
    """py-libp2p dials wss with verification off unless given a TLS client
    context (libp2p/py-libp2p#1550); the host gives it the system roots, so
    an edge's certificate is checked as go-libp2p and js-libp2p check it."""
    host, _ = create_mesh_host(Identity.generate())
    transports = host.get_network().transport_manager.get_transports()  # type: ignore[attr-defined]
    ws = next(t for t in transports if isinstance(t, WebsocketTransport))
    context = ws._config.tls_client_config  # noqa: SLF001 - py-libp2p offers no getter
    assert context is not None
    assert context.verify_mode == ssl.CERT_REQUIRED
    assert context.check_hostname


def test_the_host_runs_without_a_background_dialer():
    """py-libp2p's AutoConnector dials every peer in the peerstore every 30s
    while under 100 connections, which for a member is always. It does not
    run here; the Swarm's own limits do."""
    host, _ = create_mesh_host(Identity.generate())
    swarm = host.get_network()
    assert swarm.connection_config.low_watermark == 0
    assert swarm.connection_config.min_connections == 0
    assert swarm.connection_config.max_connections > 0
    assert swarm.connection_config.max_connections_per_peer > 0


def test_a_connection_ends_with_no_slot_held():
    """Connections come and go for a member's whole life; each must leave
    the host as it found it. py-libp2p 0.7 kept one ResourceManager
    connection slot per connection and degraded to one connection at 1000;
    the slots 0.8 counts must return to zero when the connections do."""

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
            assert manager is not None
            assert manager._current_connections == 0  # noqa: SLF001

    trio.run(main)
