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
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-msgio"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
)

// fakeDirectoryServer answers /mesh/directory on a test router's host the
// way a router does: records by key, a peer from the router's own peerstore
// or its peer-key record. It fails every request while failing is set.
type fakeDirectoryServer struct {
	h       host.Host
	mu      sync.Mutex
	records map[string]map[peer.ID][]multiaddr.Multiaddr
	served  int
	failing bool
}

func serveDirectory(h host.Host) *fakeDirectoryServer {
	d := &fakeDirectoryServer{h: h, records: map[string]map[peer.ID][]multiaddr.Multiaddr{}}
	h.SetStreamHandler(api.DirectoryProtocolID, d.handle)
	return d
}

func (d *fakeDirectoryServer) fail(failing bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing = failing
}

func (d *fakeDirectoryServer) servedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.served
}

func (d *fakeDirectoryServer) providers(key []byte) []peer.ID {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []peer.ID
	for p := range d.records[string(key)] {
		out = append(out, p)
	}
	return out
}

// add puts a record in as if another router had placed it.
func (d *fakeDirectoryServer) add(key []byte, p peer.ID, addrs ...multiaddr.Multiaddr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.records[string(key)] == nil {
		d.records[string(key)] = map[peer.ID][]multiaddr.Multiaddr{}
	}
	d.records[string(key)][p] = addrs
}

func (d *fakeDirectoryServer) handle(s network.Stream) {
	defer func() { _ = s.Close() }()
	frame, err := msgio.NewVarintReaderSize(s, directoryMaxMessage).ReadMsg()
	if err != nil {
		return
	}
	var req api.DirectoryRequest
	if err := proto.Unmarshal(frame, &req); err != nil {
		return
	}
	d.mu.Lock()
	d.served++
	resp := &api.DirectoryResponse{}
	switch {
	case d.failing:
		resp.Error = "the fake router is failing"
	case req.GetProvide() != nil:
		q := req.GetProvide()
		var addrs []multiaddr.Multiaddr
		for _, a := range q.GetAddrs() {
			if ma, err := multiaddr.NewMultiaddr(a); err == nil {
				if transport, _ := peer.SplitAddr(ma); transport != nil {
					addrs = append(addrs, transport)
				}
			}
		}
		if d.records[string(q.GetKey())] == nil {
			d.records[string(q.GetKey())] = map[peer.ID][]multiaddr.Multiaddr{}
		}
		d.records[string(q.GetKey())][s.Conn().RemotePeer()] = addrs
	case req.GetFindProviders() != nil:
		for p, addrs := range d.records[string(req.GetFindProviders().GetKey())] {
			resp.Providers = append(resp.Providers, recordOf(p, addrs))
		}
	case req.GetFindPeer() != nil:
		p, err := peer.Decode(req.GetFindPeer().GetPeerId())
		if err != nil {
			resp.Error = err.Error()
			break
		}
		if addrs := d.h.Peerstore().Addrs(p); len(addrs) > 0 {
			resp.Peer = recordOf(p, addrs)
		} else if addrs, ok := d.records[string(api.PeerKey(p))][p]; ok {
			resp.Peer = recordOf(p, addrs)
		}
	}
	d.mu.Unlock()
	data, _ := proto.Marshal(resp)
	_ = msgio.NewVarintWriter(s).WriteMsg(data)
}

func recordOf(p peer.ID, addrs []multiaddr.Multiaddr) *api.PeerRecord {
	rec := &api.PeerRecord{PeerId: p.String()}
	for _, a := range addrs {
		rec.Addrs = append(rec.Addrs, a.Encapsulate(multiaddr.StringCast("/p2p/"+p.String())).String())
	}
	return rec
}

// newDirectoryClient is a member host with a directory over the given
// router hosts, no handshake in between: the fake routers do not gate.
func newDirectoryClient(t *testing.T, routers ...host.Host) (host.Host, *routerDirectory) {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ids := make([]peer.ID, 0, len(routers))
	for _, r := range routers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := h.Connect(ctx, peer.AddrInfo{ID: r.ID(), Addrs: r.Addrs()}); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		ids = append(ids, r.ID())
	}
	d := &routerDirectory{host: h, routers: func() []peer.ID { return ids }, addrs: h.Addrs}
	return h, d
}

// A member announces what it serves and where it is reached to a router it
// is attached to, and looks peers and services up there. A router that
// fails is skipped for the next one the member holds; a lookup nobody has
// a record for is an empty answer, and a peer nobody knows is not found.
func TestRouterDirectoryAsksTheAttachedRouters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r1 := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	r2 := newFakeRouterWithKey(t, r1.cpPub, r1.cpPriv, "/ip4/127.0.0.1/tcp/0")
	dir1, dir2 := serveDirectory(r1.h), serveDirectory(r2.h)
	member, directory := newDirectoryClient(t, r1.h, r2.h)
	key, err := api.ServiceKey(api.ServiceType_SERVICE_TYPE_MCP, "calculator")
	if err != nil {
		t.Fatal(err)
	}

	if err := directory.Provide(ctx, key); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if got := dir1.providers(key); len(got) != 1 || got[0] != member.ID() {
		t.Fatalf("the first router holds %v, want the member's record", got)
	}
	if got := dir2.providers(key); len(got) != 0 {
		t.Fatalf("the second router holds %v; a record goes to one router, which places it", got)
	}

	found, err := directory.FindProviders(ctx, key, 0)
	if err != nil {
		t.Fatalf("FindProviders: %v", err)
	}
	if len(found) != 1 || found[0].ID != member.ID() || len(found[0].Addrs) == 0 {
		t.Fatalf("FindProviders = %v, want the member with its addresses", found)
	}
	if _, named := peer.SplitAddr(found[0].Addrs[0].Encapsulate(multiaddr.StringCast("/p2p/" + member.ID().String()))); named != member.ID() {
		t.Fatalf("addresses come back without the peer suffix: %v", found[0].Addrs)
	}

	unknown, _ := api.ServiceKey(api.ServiceType_SERVICE_TYPE_MCP, "nobody")
	if found, err := directory.FindProviders(ctx, unknown, 0); err != nil || len(found) != 0 {
		t.Fatalf("FindProviders of a key nobody announced = %v, %v; want none and no error", found, err)
	}

	stranger := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0").h.ID()
	if _, err := directory.FindPeer(ctx, stranger); err != routing.ErrNotFound {
		t.Fatalf("FindPeer of a peer nobody knows: err = %v, want %v", err, routing.ErrNotFound)
	}
	info, err := directory.FindPeer(ctx, member.ID())
	if err != nil || info.ID != member.ID() || len(info.Addrs) == 0 {
		t.Fatalf("FindPeer of the member = %v, %v; want it from the router's own connection", info, err)
	}

	// The first router fails: the second answers, and the member is none the wiser.
	dir1.fail(true)
	dir2.add(key, member.ID(), member.Addrs()...)
	before := dir2.servedCount()
	if found, err := directory.FindProviders(ctx, key, 0); err != nil || len(found) != 1 {
		t.Fatalf("FindProviders with the first router failing = %v, %v; want the second router's answer", found, err)
	}
	if got := dir2.servedCount(); got != before+1 {
		t.Fatalf("the second router served %d requests, want %d: it must be asked when the first fails", got, before+1)
	}
	dir2.fail(true)
	if _, err := directory.FindProviders(ctx, key, 0); err == nil {
		t.Fatal("FindProviders succeeded with every router failing")
	}
}
