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

package node

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/test"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/multiformats/go-multiaddr"
)

// A peer reached only through a router this node has not handshaken with:
// the lookup learns the circuit address from a router that holds it, dials
// it to query the peer, and is refused, since that router has not admitted
// this node; kad-dht then answers "not found" and leaves the address in
// dial backoff. The node must take the address the lookup learned anyway,
// handshake with the router, and clear the backoff, or the call fails
// twice over for a peer that is one handshake away.
func TestPreparePeerAddrsUsesWhatAFailedLookupLearned(t *testing.T) {
	held := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	// The held router is a DHT server, as a router is, so it enters the
	// node's routing table and answers its lookups.
	heldDHT, err := dht.New(held.h, dht.Mode(dht.ModeServer), dht.ProtocolPrefix(api.DHTProtocolPrefix))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = heldDHT.Close() }()
	other := newFakeRouterWithKey(t, held.cpPub, held.cpPriv, "/ip4/127.0.0.1/tcp/0")

	// The peer reserved with the other router, which recorded the circuit;
	// the held router learned it, as a router does through identify.
	target := test.RandPeerIDFatal(t)
	circuit := other.routerAddrs[0].Encapsulate(multiaddr.StringCast("/p2p-circuit"))
	held.h.Peerstore().AddAddrs(target, []multiaddr.Multiaddr{circuit}, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := held.startNode(t, ctx, held.routerAddrs)
	for node.DHT.RoutingTable().Size() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("the held router never entered the node's routing table")
		case <-time.After(20 * time.Millisecond):
		}
	}
	before := counterValue(t, peerLookupsTotal.WithLabelValues(lookupFound))

	node.preparePeerAddrs(ctx, target)

	if got := other.handshakes.Load(); got != 1 {
		t.Fatalf("the other router saw %d handshakes, want 1: the node must handshake with the router the lookup named", got)
	}
	var got []string
	for _, a := range node.Host.Peerstore().Addrs(target) {
		got = append(got, a.String())
	}
	slices.Sort(got)
	want := []string{circuit.String(), "/p2p/" + other.h.ID().String() + "/p2p-circuit"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("addresses kept for the peer: %v, want the circuit the lookup learned and its bare form %v", got, want)
	}
	if after := counterValue(t, peerLookupsTotal.WithLabelValues(lookupFound)); after != before+1 {
		t.Errorf("lookups counted as found: %v, want %v", after, before+1)
	}
	sw, ok := node.Host.Network().(*swarm.Swarm)
	if !ok {
		t.Fatal("the node's network is not a swarm")
	}
	if sw.Backoff().Backoff(target, circuit) {
		t.Error("the circuit the lookup's refused dial put in backoff is still there after the handshake")
	}
}
