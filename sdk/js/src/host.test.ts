// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import assert from "node:assert/strict";
import net from "node:net";
import { test } from "node:test";
import { multiaddr } from "@multiformats/multiaddr";
import { createMeshHost } from "./host.ts";
import { Identity } from "./identity.ts";

const ECHO = "/test/echo/1.0.0";

// A TLS-terminating edge in front of the router (sam-one behind a tunnel)
// selects the origin by the name in the TLS SNI and the Host header, so a
// /dns4 WebSocket address is dialed by its name, not by what it resolves to.
// Pinned on the Host header of the upgrade request; the SNI is the same string.
test("a /dns4 WebSocket address is dialed by its name", async () => {
  const server = net.createServer();
  const request = new Promise<string>((resolve) => {
    server.once("connection", (socket) => {
      socket.once("data", (data) => {
        resolve(data.toString());
        socket.destroy();
      });
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as net.AddressInfo).port;
  try {
    const client = await createMeshHost(Identity.generate());
    try {
      const addr = multiaddr(`/dns4/localhost/tcp/${port}/ws/p2p/${Identity.generate().peerId}`);
      await assert.rejects(client.dial(addr, { signal: AbortSignal.timeout(5_000) }));
      const head = (await request).split("\r\n").map((l) => l.toLowerCase());
      assert.ok(head.includes(`host: localhost:${port}`), `upgrade request:\n${head.join("\n")}`);
    } finally {
      await client.stop();
    }
  } finally {
    server.close();
  }
});

// A mesh host reaches a peer over WebSocket as it does over TCP: sam-one's
// router listens on /ws alone, on the port that also serves its HTTP API.
for (const listen of ["/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/tcp/0/ws"]) {
  test(`a mesh host dials ${listen} and opens a TLS+yamux stream`, async () => {
    const server = await createMeshHost(Identity.generate(), { listenAddrs: [listen] });
    const client = await createMeshHost(Identity.generate());
    try {
      await server.handle(ECHO, (stream) => {
        stream.addEventListener("message", (ev) => {
          stream.send(ev.data.subarray());
        });
        stream.addEventListener("remoteCloseWrite", () => {
          void stream.close();
        });
      });
      const addr = server.getMultiaddrs()[0];
      assert.ok(addr !== undefined, "the server listens on nothing");
      assert.equal(addr.toString().endsWith("/ws/p2p/" + server.peerId.toString()), listen.endsWith("/ws"), `listening on ${addr.toString()}`);

      const stream = await client.dialProtocol(addr, ECHO);
      const answer = new Promise<string>((resolve) => {
        stream.addEventListener("message", (ev) => resolve(new TextDecoder().decode(ev.data.subarray())), { once: true });
      });
      stream.send(new TextEncoder().encode("hello"));
      assert.equal(await answer, "hello");
      await stream.close();

      const conn = client.getConnections(server.peerId)[0];
      assert.ok(conn !== undefined, "no connection to the server");
      assert.equal(conn.encryption, "/tls/1.0.0");
      assert.equal(conn.multiplexer, "/yamux/1.0.0");
      assert.equal(conn.remoteAddr.toString().includes("/ws"), listen.endsWith("/ws"));
    } finally {
      await client.stop();
      await server.stop();
    }
  });
}
