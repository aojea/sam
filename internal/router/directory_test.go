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

package router

import (
	"context"
	"slices"
	"testing"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	kb "github.com/libp2p/go-libp2p-kbucket"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
)

// newDirectoryRouter is a router with its DHT and the directory handler
// up, as Start wires them, on a loopback host.
func newDirectoryRouter(t *testing.T) *Router {
	t.Helper()
	r, gated := newGatedRouter(t)
	r.gated = gated
	kdht, err := dht.New(gated, r.dhtOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kdht.Close() })
	r.DHT = kdht
	gated.SetStreamHandler(api.DirectoryProtocolID, r.handleDirectory)
	return r
}

// federate connects two routers and has each admit the other as an
// authenticated peer router, as the handshake does.
func federate(t *testing.T, a, b *Router) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Host.Connect(ctx, peer.AddrInfo{ID: b.Host.ID(), Addrs: b.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]*Router{{a, b}, {b, a}} {
		pair[0].authenticatedPeers.Store(pair[1].Host.ID(), time.Now().Add(time.Hour))
		pair[0].admitPeerRouter(pair[1].Host.ID())
	}
}

// attach connects a member host to a router and marks it authenticated there.
func attach(t *testing.T, r *Router) host.Host {
	t.Helper()
	m := newClient(t, r)
	r.authenticatedPeers.Store(m.ID(), time.Now().Add(time.Hour))
	return m
}

// ask sends one request to a router and returns its answer.
func ask(t *testing.T, from host.Host, router peer.ID, req *api.DirectoryRequest) *api.DirectoryResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := from.NewStream(ctx, router, api.DirectoryProtocolID)
	if err != nil {
		t.Fatalf("open directory stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	data, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := msgio.NewVarintWriter(s).WriteMsg(data); err != nil {
		t.Fatal(err)
	}
	frame, err := msgio.NewVarintReaderSize(s, directoryMaxMessage).ReadMsg()
	if err != nil {
		t.Fatalf("read directory answer: %v", err)
	}
	var resp api.DirectoryResponse
	if err := proto.Unmarshal(frame, &resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

func provide(key []byte, addrs ...string) *api.DirectoryRequest {
	return &api.DirectoryRequest{Request: &api.DirectoryRequest_Provide{Provide: &api.DirectoryProvide{Key: key, Addrs: addrs}}}
}

func findProviders(key []byte) *api.DirectoryRequest {
	return &api.DirectoryRequest{Request: &api.DirectoryRequest_FindProviders{FindProviders: &api.DirectoryFindProviders{Key: key}}}
}

func findPeer(p peer.ID) *api.DirectoryRequest {
	return &api.DirectoryRequest{Request: &api.DirectoryRequest_FindPeer{FindPeer: &api.DirectoryFindPeer{PeerId: p.String()}}}
}

func directoryCount(t *testing.T, op, result string) float64 {
	t.Helper()
	var m dto.Metric
	if err := directoryRequestsTotal.WithLabelValues(op, result).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// A member announces a record to the router it is attached to; the router
// keeps it and places it on the routers closest to the key, as many as the
// mesh's replication. A member attached to any router then finds it: from
// that router's own store when it holds a copy, through the routers' DHT
// otherwise. A peer is found the same way, under its peer key, or from the
// router that holds it.
func TestDirectoryRecordsAreFoundFromAnyRouter(t *testing.T) {
	a, b, c := newDirectoryRouter(t), newDirectoryRouter(t), newDirectoryRouter(t)
	federate(t, a, b)
	federate(t, a, c)
	federate(t, b, c)
	// One copy besides the member's own router, on the closest of the others.
	a.directory.Replication = 1

	member := attach(t, a)
	key, err := api.ServiceKey(api.ServiceType_SERVICE_TYPE_MCP, "calculator")
	if err != nil {
		t.Fatal(err)
	}
	memberAddr := member.Addrs()[0].String() + "/p2p/" + member.ID().String()
	if resp := ask(t, member, a.Host.ID(), provide(key, memberAddr)); resp.GetError() != "" {
		t.Fatalf("provide refused: %s", resp.GetError())
	}
	if resp := ask(t, member, a.Host.ID(), provide(api.PeerKey(member.ID()), memberAddr)); resp.GetError() != "" {
		t.Fatalf("provide of the peer key refused: %s", resp.GetError())
	}

	closest := a.DHT.RoutingTable().NearestPeers(kb.ConvertKey(string(key)), 1)
	if len(closest) != 1 {
		t.Fatalf("closest routers to the key = %v, want one", closest)
	}
	holder, other := b, c
	if closest[0] == c.Host.ID() {
		holder, other = c, b
	}
	if provs, _ := holder.DHT.ProviderStore().GetProviders(context.Background(), key); len(provs) != 1 || provs[0].ID != member.ID() {
		t.Fatalf("the closest router holds %v, want the member's record", provs)
	}
	if provs, _ := other.DHT.ProviderStore().GetProviders(context.Background(), key); len(provs) != 0 {
		t.Fatalf("the other router holds %v, want nothing: replication is one", provs)
	}

	for name, router := range map[string]*Router{"own router": a, "holder": holder, "other router, through the DHT": other} {
		t.Run("find providers from the "+name, func(t *testing.T) {
			asker := attach(t, router)
			resp := ask(t, asker, router.Host.ID(), findProviders(key))
			if resp.GetError() != "" {
				t.Fatalf("find_providers failed: %s", resp.GetError())
			}
			if len(resp.GetProviders()) != 1 || resp.GetProviders()[0].GetPeerId() != member.ID().String() {
				t.Fatalf("providers = %v, want the member", resp.GetProviders())
			}
			if got := resp.GetProviders()[0].GetAddrs(); !slices.Contains(got, memberAddr) {
				t.Fatalf("provider addrs = %v, want %s among them", got, memberAddr)
			}
		})
	}

	t.Run("find peer from the other router", func(t *testing.T) {
		asker := attach(t, other)
		resp := ask(t, asker, other.Host.ID(), findPeer(member.ID()))
		if resp.GetError() != "" {
			t.Fatalf("find_peer failed: %s", resp.GetError())
		}
		if resp.GetPeer().GetPeerId() != member.ID().String() || !slices.Contains(resp.GetPeer().GetAddrs(), memberAddr) {
			t.Fatalf("peer = %v, want the member at %s", resp.GetPeer(), memberAddr)
		}
	})

	t.Run("find peer from its own router answers from the connection", func(t *testing.T) {
		asker := attach(t, a)
		resp := ask(t, asker, a.Host.ID(), findPeer(member.ID()))
		if resp.GetPeer().GetPeerId() != member.ID().String() || len(resp.GetPeer().GetAddrs()) == 0 {
			t.Fatalf("peer = %v, want the member with addresses", resp.GetPeer())
		}
	})

	t.Run("nobody is found under a key nobody announced", func(t *testing.T) {
		before := directoryCount(t, directoryOpFindProviders, directoryResultEmpty)
		unknown, _ := api.ServiceKey(api.ServiceType_SERVICE_TYPE_MCP, "nobody")
		asker := attach(t, b)
		resp := ask(t, asker, b.Host.ID(), findProviders(unknown))
		if resp.GetError() != "" || len(resp.GetProviders()) != 0 {
			t.Fatalf("got %v, want an empty answer and no error", resp)
		}
		if got := directoryCount(t, directoryOpFindProviders, directoryResultEmpty); got != before+1 {
			t.Fatalf("empty answers counted = %v, want %v", got, before+1)
		}
		if resp := ask(t, asker, b.Host.ID(), findPeer(asker.ID())); resp.GetPeer() != nil && len(resp.GetPeer().GetAddrs()) == 0 {
			t.Fatalf("find_peer of a peer with no record = %v, want unset or its own connection", resp.GetPeer())
		}
	})
}

// A member can only announce itself: a record naming another provider, an
// address of another peer, or a key that is not a sha256 multihash is
// refused and counted as invalid, and nothing is stored.
func TestDirectoryRefusesWhatAMemberMayNotAnnounce(t *testing.T) {
	a := newDirectoryRouter(t)
	member := attach(t, a)
	stranger := newClient(t, a)
	key, _ := api.ServiceKey(api.ServiceType_SERVICE_TYPE_MCP, "calculator")

	for name, req := range map[string]*api.DirectoryRequest{
		"another provider":              {Request: &api.DirectoryRequest_Provide{Provide: &api.DirectoryProvide{Key: key, Provider: stranger.ID().String()}}},
		"another peer's address":        provide(key, stranger.Addrs()[0].String()+"/p2p/"+stranger.ID().String()),
		"a key that is not a multihash": provide([]byte("calculator")),
		"a lookup of such a key":        findProviders([]byte("calculator")),
		"a peer id that is not one":     findPeer(peer.ID("nobody")),
	} {
		t.Run(name, func(t *testing.T) {
			op := directoryOpProvide
			switch req.GetRequest().(type) {
			case *api.DirectoryRequest_FindProviders:
				op = directoryOpFindProviders
			case *api.DirectoryRequest_FindPeer:
				op = directoryOpFindPeer
			}
			if req.GetProvide().GetProvider() != "" {
				op = directoryOpPlace
			}
			before := directoryCount(t, op, directoryResultInvalid)
			resp := ask(t, member, a.Host.ID(), req)
			if resp.GetError() == "" {
				t.Fatalf("accepted; want a refusal")
			}
			if got := directoryCount(t, op, directoryResultInvalid); got != before+1 {
				t.Fatalf("invalid %s counted = %v, want %v", op, got, before+1)
			}
		})
	}
	if provs, _ := a.DHT.ProviderStore().GetProviders(context.Background(), key); len(provs) != 0 {
		t.Fatalf("a refused record was stored: %v", provs)
	}
}

// A record placed by a router names its provider; the receiving router
// stores it under that provider, so a lookup there returns the member and
// not the router that placed it.
func TestDirectoryPlacementNamesTheProvider(t *testing.T) {
	a, b := newDirectoryRouter(t), newDirectoryRouter(t)
	federate(t, a, b)
	member := attach(t, a)
	key, _ := api.ServiceKey(api.ServiceType_SERVICE_TYPE_INFERENCE, "")
	memberAddr := member.Addrs()[0].String() + "/p2p/" + member.ID().String()
	if resp := ask(t, member, a.Host.ID(), provide(key, memberAddr)); resp.GetError() != "" {
		t.Fatalf("provide refused: %s", resp.GetError())
	}
	provs, err := b.DHT.ProviderStore().GetProviders(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 1 || provs[0].ID != member.ID() {
		t.Fatalf("router b holds %v, want the member's record placed by router a", provs)
	}
	if len(provs[0].Addrs) == 0 || provs[0].Addrs[0].String() != member.Addrs()[0].String() {
		t.Fatalf("router b holds addrs %v, want the member's %v without the peer suffix", provs[0].Addrs, member.Addrs()[0])
	}
}
