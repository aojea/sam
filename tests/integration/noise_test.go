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

package integration_test

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/multiformats/go-multiaddr"
)

// TestNoiseOnlyPeer pins what a browser member is on the wire: a peer that
// speaks Noise and no libp2p TLS. It authenticates with the router over
// Noise, is relayed to a sam-node, and the relayed connection, upgraded end
// to end between the two of them, is Noise as well, so the node's auth
// handshake and a service call go through. A peer that speaks TLS still
// lands on TLS: it is offered first.
func TestNoiseOnlyPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mesh := startSDKMesh(t)

	h, err := libp2p.New(
		libp2p.NoListenAddrs,
		libp2p.Security(noise.ID, noise.New),
		libp2p.EnableRelay(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	biscuit := goHostBiscuit(t, mesh.cpPriv, h.ID())

	router, err := peer.AddrInfoFromString(mesh.routerAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Connect(ctx, *router); err != nil {
		t.Fatalf("noise-only peer could not connect to the router: %v", err)
	}
	if got := securityOf(h, router.ID); got != noise.ID {
		t.Fatalf("connection to the router is %q, want %q", got, noise.ID)
	}
	routerBiscuit := authHandshake(t, ctx, h, router.ID, biscuit)
	if err := identity.VerifyBiscuitRole(routerBiscuit, mesh.cpPriv.Public().(ed25519.PublicKey), api.RoleRouter, 5*time.Second); err != nil {
		t.Fatalf("router credential lacks the router role: %v", err)
	}

	nodeID := mesh.samNode.peerID
	relayed := multiaddr.StringCast(mesh.routerAddr + "/p2p-circuit/p2p/" + nodeID.String())
	// A relayed connection reports no security protocol in its ConnState;
	// that this peer, which has Noise alone, gets one at all is the check.
	if err := h.Connect(network.WithAllowLimitedConn(ctx, "test"), peer.AddrInfo{ID: nodeID, Addrs: []multiaddr.Multiaddr{relayed}}); err != nil {
		t.Fatalf("noise-only peer could not reach the node through the router: %v", err)
	}
	nodeBiscuit := authHandshake(t, ctx, h, nodeID, biscuit)
	if err := identity.VerifyBiscuitRole(nodeBiscuit, mesh.cpPriv.Public().(ed25519.PublicKey), api.RoleNode, 5*time.Second); err != nil {
		t.Fatalf("node credential lacks the node role: %v", err)
	}
	// The MCP service answers a bare GET with its own 400 (the streamable
	// HTTP transport wants an SSE Accept); an answer from the service is what
	// shows the request crossed the noise connection into the node.
	if status, body := libp2pHTTPGet(t, ctx, h, nodeID, biscuit, "/mcp/calc"); status != 200 && (status != 400 || !strings.Contains(body, "text/event-stream")) {
		t.Fatalf("service call over the noise connection: %d %s", status, body)
	}

	// The TLS peer of the other tests is unchanged: TLS is offered first.
	tlsPeer := newAdmittedGoPeer(t, ctx, mesh.cpPriv, mesh.routerAddr)
	if got := securityOf(tlsPeer, router.ID); !strings.HasPrefix(got, "/tls/") {
		t.Fatalf("TLS peer's connection to the router is %q, want /tls/...", got)
	}
}

// securityOf is the security protocol of h's connection to p.
func securityOf(h host.Host, p peer.ID) string {
	conns := h.Network().ConnsToPeer(p)
	if len(conns) == 0 {
		return ""
	}
	return string(conns[0].ConnState().Security)
}
