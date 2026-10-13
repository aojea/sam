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
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/test"

	"github.com/google/agentmesh/api"
)

// TestMemberDHTStaysClientWhenPublic: a member whose reachability is public
// must not serve the DHT protocol, or it lands in every other member's
// routing table. The mode is the decision; the protocol list is what other
// members see of it. The control DHT, in the mode the node used before,
// shows that this host setup does flip a ModeAuto DHT to server.
func TestMemberDHTStaysClientWhenPublic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	newHost := func() host.Host {
		h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.ForceReachabilityPublic())
		if err != nil {
			t.Fatalf("libp2p.New: %v", err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	serves := func(h host.Host) bool {
		return slices.Contains(h.Mux().Protocols(), protocol.ID(api.DHTProtocolID))
	}

	control := newHost()
	controlDHT, err := dht.New(control, dht.Mode(dht.ModeAuto), dht.ProtocolPrefix(api.DHTProtocolPrefix))
	if err != nil {
		t.Fatalf("control dht.New: %v", err)
	}
	t.Cleanup(func() { _ = controlDHT.Close() })

	member := newHost()
	memberDHT, err := dht.New(member, dhtOptions(api.DefaultDirectorySettings())...)
	if err != nil {
		t.Fatalf("member dht.New: %v", err)
	}
	t.Cleanup(func() { _ = memberDHT.Close() })
	if err := memberDHT.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for !serves(control) {
		if time.Now().After(deadline) {
			t.Fatalf("control DHT in ModeAuto on a public host never served %s; protocols: %v", api.DHTProtocolID, control.Mux().Protocols())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := memberDHT.Mode(); got != dht.ModeClient {
		t.Errorf("member DHT mode = %v, want %v", got, dht.ModeClient)
	}
	if serves(member) {
		t.Errorf("member on a public host serves %s; protocols: %v", api.DHTProtocolID, member.Mux().Protocols())
	}
}

// TestSetRouterCatalogPrunesDHTTable: with kad-dht's liveness pings off, the
// catalog is what takes a router out of the member's DHT table.
func TestSetRouterCatalogPrunesDHTTable(t *testing.T) {
	h, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	kdht, err := dht.New(h, dhtOptions(api.DefaultDirectorySettings())...)
	if err != nil {
		t.Fatalf("dht.New: %v", err)
	}
	t.Cleanup(func() { _ = kdht.Close() })

	listed, gone := test.RandPeerIDFatal(t), test.RandPeerIDFatal(t)
	for _, p := range []peer.ID{listed, gone} {
		if added, err := kdht.RoutingTable().TryAddPeer(p, false, false); err != nil || !added {
			t.Fatalf("TryAddPeer(%s) = %v, %v; want added", p, added, err)
		}
	}

	n := &AgentMeshNode{config: Options{Routers: 2}, Host: h, DHT: kdht}
	n.setRouterCatalog([]routerCandidate{{AddrInfo: peer.AddrInfo{ID: listed}}})

	got := kdht.RoutingTable().ListPeers()
	if len(got) != 1 || got[0] != listed {
		t.Fatalf("routing table after the catalog dropped %s = %v, want [%s]", gone, got, listed)
	}

	// An empty catalog says nothing about the routers and must not empty the table.
	n.setRouterCatalog(nil)
	if got := kdht.RoutingTable().ListPeers(); len(got) != 1 {
		t.Fatalf("routing table after an empty catalog = %v, want [%s]", got, listed)
	}
}

// TestMemberDHTBucketHoldsTheReplicationFactor: the mesh's replication
// factor is kademlia's k on the member, and a bucket of its table holds
// that many.
func TestMemberDHTBucketHoldsTheReplicationFactor(t *testing.T) {
	for _, r := range []int{2, api.DefaultDHTReplication, 5} {
		t.Run(fmt.Sprintf("replication=%d", r), func(t *testing.T) {
			h, err := libp2p.New(libp2p.NoListenAddrs)
			if err != nil {
				t.Fatalf("libp2p.New: %v", err)
			}
			t.Cleanup(func() { _ = h.Close() })
			settings := api.DefaultDirectorySettings()
			settings.Replication = r
			kdht, err := dht.New(h, dhtOptions(settings)...)
			if err != nil {
				t.Fatalf("dht.New: %v", err)
			}
			t.Cleanup(func() { _ = kdht.Close() })
			rt := kdht.RoutingTable()
			added := 0
			for i := 0; i < r+1; i++ {
				p, err := rt.GenRandPeerID(0)
				if err != nil {
					t.Fatalf("GenRandPeerID: %v", err)
				}
				// The last one is refused for capacity; any other refusal is a test bug.
				if ok, err := rt.TryAddPeer(p, true, false); ok {
					added++
				} else if added < r {
					t.Fatalf("TryAddPeer #%d refused before the bucket was full: %v", i, err)
				}
			}
			if added != r {
				t.Fatalf("a bucket took %d peers, want %d", added, r)
			}
		})
	}
}
