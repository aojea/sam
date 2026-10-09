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
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeRouter is a libp2p host answering the Agent Mesh auth protocol. It counts
// handshakes so a test can tell a fresh session from a reused one, and keeps
// the last biscuit it was shown.
type fakeRouter struct {
	h           host.Host
	routerAddrs []multiaddr.Multiaddr
	handshakes  *atomic.Int32
	lastBiscuit *atomic.Pointer[[]byte]
	cpPub       ed25519.PublicKey
	cpPriv      ed25519.PrivateKey
	mint        func(peerID, role string) []byte
}

func newFakeRouter(t *testing.T, listenAddrs ...string) *fakeRouter {
	t.Helper()
	cpPub, cpPriv, _ := ed25519.GenerateKey(nil)

	h, err := libp2p.New(libp2p.ListenAddrStrings(listenAddrs...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	mint := func(peerID, role string) []byte {
		b := biscuit.NewBuilder(cpPriv)
		for _, f := range []biscuit.Fact{
			{Predicate: biscuit.Predicate{Name: api.FactNode, IDs: []biscuit.Term{biscuit.String(peerID)}}},
			{Predicate: biscuit.Predicate{Name: api.FactRole, IDs: []biscuit.Term{biscuit.String(role)}}},
			{Predicate: biscuit.Predicate{Name: api.FactExpiration, IDs: []biscuit.Term{biscuit.Date(time.Now().Add(24 * time.Hour))}}},
			api.MarkerFact(api.FactTargetUnrestricted),
		} {
			if err := b.AddAuthorityFact(f); err != nil {
				t.Fatal(err)
			}
		}
		tok, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		out, err := tok.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	routerBiscuit := mint(h.ID().String(), api.RoleRouter)

	var handshakes atomic.Int32
	var lastBiscuit atomic.Pointer[[]byte]
	h.SetStreamHandler(api.AuthProtocolID, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		handshakes.Add(1)
		reader := msgio.NewVarintReaderSize(s, 1024*64)
		msg, err := reader.ReadMsg()
		if err != nil {
			return
		}
		var frame api.AuthFrame
		if err := proto.Unmarshal(msg, &frame); err == nil {
			shown := append([]byte(nil), frame.Biscuit...)
			lastBiscuit.Store(&shown)
		}
		reader.ReleaseMsg(msg)
		data, _ := proto.Marshal(&api.AuthResponse{Success: true, Biscuit: routerBiscuit})
		_ = msgio.NewVarintWriter(s).WriteMsg(data)
	})

	var routerAddrs []multiaddr.Multiaddr
	for _, a := range h.Addrs() {
		ma, err := multiaddr.NewMultiaddr(a.String() + "/p2p/" + h.ID().String())
		if err != nil {
			t.Fatal(err)
		}
		routerAddrs = append(routerAddrs, ma)
	}

	return &fakeRouter{h: h, routerAddrs: routerAddrs, handshakes: &handshakes, lastBiscuit: &lastBiscuit, cpPub: cpPub, cpPriv: cpPriv, mint: mint}
}

// startNode brings up a node enrolled against this router and authenticated.
// Each tweak may adjust the options before the node is built.
func (r *fakeRouter) startNode(t *testing.T, ctx context.Context, routerAddrs []multiaddr.Multiaddr, tweaks ...func(*Options)) *AgentMeshNode {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	privKey := GetOrGenerateKey(store)
	pid, err := peer.IDFromPrivateKey(privKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIdentity(r.mint(pid.String(), api.RoleNode)); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		PrivKey:            privKey,
		Store:              store,
		ControlPlanePubKey: r.cpPub,
		RouterAddrs:        routerAddrs,
		ListenAddrs:        []string{"/ip4/127.0.0.1/tcp/0"},
		AllowLoopback:      true,
	}
	for _, tweak := range tweaks {
		tweak(&opts)
	}
	node, err := NewAgentMeshNode(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = node.Teardown() })
	return node
}

// waitForConnectedness polls until the node's view of the router matches want,
// and reports the last state it saw if the deadline passes first.
func waitForConnectedness(t *testing.T, node *AgentMeshNode, router peer.ID, want network.Connectedness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got network.Connectedness
	for time.Now().Before(deadline) {
		got = node.Host.Network().Connectedness(router)
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("connectedness to router: got %s, want %s", got, want)
}

// A router without an external URL advertises one multiaddr per interface,
// all for the same peer. Authenticating once per address re-runs the
// handshake on the already-open connection, which trips the router's
// per-peer handshake limiter and logs a failure for every extra address.
// One live authenticated session per router must be enough.
func TestStartAuthenticatesEachRouterOnce(t *testing.T) {
	// Two listen addresses on the same host stand in for the interfaces a
	// real router advertises; the handshake counter is per router, not per
	// address.
	r := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/tcp/0")
	h, handshakes := r.h, r.handshakes

	// The same address listed twice, as a control plane that stores the
	// router's lease verbatim can hand out.
	routerAddrs := append(r.routerAddrs, r.routerAddrs[0])
	if len(routerAddrs) < 3 {
		t.Fatalf("expected at least 3 router multiaddrs, got %d", len(routerAddrs))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := r.startNode(t, ctx, routerAddrs)

	if got := handshakes.Load(); got != 1 {
		t.Fatalf("router saw %d handshakes for %d advertised addresses, want 1", got, len(routerAddrs))
	}
	if !node.IsConnected() {
		t.Fatal("node does not report an authenticated router connection")
	}

	// A re-run with the session still open (what the connection monitor and
	// a re-enrollment do) must not handshake again either.
	for _, a := range routerAddrs {
		if err := node.ConnectAndAuthWithRouter(ctx, a); err != nil {
			t.Fatalf("ConnectAndAuthWithRouter(%s): %v", a, err)
		}
	}
	if got := handshakes.Load(); got != 1 {
		t.Fatalf("re-auth over a live session handshook again: %d total, want 1", got)
	}

	// Once the session is gone the router has forgotten the peer, so the
	// next attempt must handshake afresh rather than trust the stale entry.
	if err := node.Host.Network().ClosePeer(h.ID()); err != nil {
		t.Fatal(err)
	}
	waitForConnectedness(t, node, h.ID(), network.NotConnected)
	if err := node.ConnectAndAuthWithRouter(ctx, routerAddrs[0]); err != nil {
		t.Fatalf("re-auth after disconnect: %v", err)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("re-auth after disconnect: %d handshakes, want 2", got)
	}
}

// The router forgets a peer the moment its last connection closes. If the
// socket then comes back on its own - libp2p redialling, a DHT lookup, a
// relay reservation attempt - nothing re-runs the handshake, so the node must
// not keep reporting the session as authenticated. Left stale, IsConnected
// answers true, the connection monitor sees a healthy router and returns
// early, and the node sits authenticated-in-its-own-mind for as long as the
// process lives: reservations refused, Host.Addrs empty, undiscoverable.
func TestRouterDisconnectClearsAuthenticatedSession(t *testing.T) {
	r := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	h, handshakes := r.h, r.handshakes

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The redial would restore the session on its own; this is about the
	// state in between, so it is off here and TestRouterDropIsRedialed
	// covers the redial.
	node := r.startNode(t, ctx, r.routerAddrs, func(o *Options) { o.RouterRedialDelay = -1 })

	if !node.IsConnected() {
		t.Fatal("node does not report an authenticated router connection after Start")
	}

	if err := node.Host.Network().ClosePeer(h.ID()); err != nil {
		t.Fatal(err)
	}
	waitForConnectedness(t, node, h.ID(), network.NotConnected)

	// Reconnect at the libp2p level only, exactly as an unattended redial
	// does: a live socket carrying no handshake.
	if err := node.Host.Connect(ctx, peer.AddrInfo{ID: h.ID(), Addrs: h.Addrs()}); err != nil {
		t.Fatalf("reconnect without handshake: %v", err)
	}
	waitForConnectedness(t, node, h.ID(), network.Connected)

	if got := handshakes.Load(); got != 1 {
		t.Fatalf("plain reconnect handshook: %d total, want 1", got)
	}
	if node.IsConnected() {
		t.Fatal("node reports an authenticated session over a connection the router never authenticated")
	}

	// And the monitor's recovery path must restore it.
	if err := node.ConnectAndAuthWithRouter(ctx, r.routerAddrs[0]); err != nil {
		t.Fatalf("re-auth after unattended reconnect: %v", err)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("re-auth after unattended reconnect: %d handshakes, want 2", got)
	}
	if !node.IsConnected() {
		t.Fatal("node does not report an authenticated router connection after re-auth")
	}
}

// A router admits a peer until the biscuit it was shown expires, whatever
// the connection does; once that passes it refuses relay circuits to the
// peer. A credential refreshed with the control plane therefore has to be
// presented to every router on the live session, or the node turns
// unreachable an hour after it started while every connection looks fine.
func TestRefreshEnrollmentReadmitsConnectedRouters(t *testing.T) {
	r := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")

	var refreshed []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/refresh", func(w http.ResponseWriter, req *http.Request) {
		var body api.TokenRefreshRequest
		data, _ := io.ReadAll(req.Body)
		if err := proto.Unmarshal(data, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		refreshed = r.mint(body.PeerId, api.RoleNode)
		out, _ := proto.Marshal(&api.TokenRefreshResponse{BiscuitToken: refreshed, ExpireTime: timestamppb.New(time.Now().Add(24 * time.Hour))})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := r.startNode(t, ctx, r.routerAddrs)
	if err := node.Store.SaveControlPlaneURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	if got := r.handshakes.Load(); got != 1 {
		t.Fatalf("handshakes after Start: %d, want 1", got)
	}

	if err := node.RefreshEnrollment(ctx); err != nil {
		t.Fatalf("RefreshEnrollment: %v", err)
	}

	if got := r.handshakes.Load(); got != 2 {
		t.Fatalf("handshakes after refresh: %d, want 2 (the refreshed credential was not presented)", got)
	}
	shown := r.lastBiscuit.Load()
	if shown == nil || !bytes.Equal(*shown, refreshed) {
		t.Fatal("router was not shown the refreshed biscuit")
	}
	if !node.IsConnected() {
		t.Fatal("node does not report an authenticated router connection after refresh")
	}
	waitForConnectedness(t, node, r.h.ID(), network.Connected)
}

// The control plane keeps advertising a router for as long as its lease
// lasts, so after a rollout every joiner is handed a router that is gone.
// Start must not pay that router's dial timeout before the node is ready:
// the live router admits the node and the dead one is still being dialed
// when Start returns.
func TestStartIsReadyOnceOneRouterAdmitsTheNode(t *testing.T) {
	live := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")

	// A listener that accepts and never speaks: the dial completes at the
	// TCP level, the security handshake then waits out the timeout.
	deadListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deadListener.Close() }()
	go func() {
		for {
			c, err := deadListener.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
		}
	}()
	_, deadPub, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	deadID, err := peer.IDFromPublicKey(deadPub)
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := multiaddr.StringCast(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", deadListener.Addr().(*net.TCPAddr).Port, deadID))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The dead router first, where a sequential dial would have waited on it.
	routerAddrs := append([]multiaddr.Multiaddr{deadAddr}, live.routerAddrs...)

	const connectTimeout = 3 * time.Second
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	privKey := GetOrGenerateKey(store)
	pid, _ := peer.IDFromPrivateKey(privKey)
	if err := store.SaveIdentity(live.mint(pid.String(), api.RoleNode)); err != nil {
		t.Fatal(err)
	}
	node, err := NewAgentMeshNode(Options{
		PrivKey:              privKey,
		Store:                store,
		ControlPlanePubKey:   live.cpPub,
		RouterAddrs:          routerAddrs,
		ListenAddrs:          []string{"/ip4/127.0.0.1/tcp/0"},
		AllowLoopback:        true,
		RouterConnectTimeout: connectTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Teardown() })

	started := time.Now()
	if err := node.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	took := time.Since(started)
	if !node.IsConnected() {
		t.Fatal("Start returned without an authenticated router")
	}
	if took >= connectTimeout {
		t.Fatalf("Start took %v, at least the dead router's %v dial timeout; the live router should have made the node ready", took, connectTimeout)
	}
	if got := live.handshakes.Load(); got != 1 {
		t.Fatalf("live router saw %d handshakes, want 1", got)
	}
}

// Three entries for one router, as a control plane hands out when a dnsaddr
// resolves to every address behind the name: one peer, one dial, one
// handshake, and one static relay.
func TestRouterPeersGroupsAddressesByPeer(t *testing.T) {
	r := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/tcp/0")
	addrs := append(append([]multiaddr.Multiaddr{}, r.routerAddrs...), r.routerAddrs[0])

	peers := routerPeers(context.Background(), addrs)
	if len(peers) != 1 {
		t.Fatalf("routerPeers = %d peers for %d addresses of one router, want 1", len(peers), len(addrs))
	}
	if peers[0].ID != r.h.ID() {
		t.Fatalf("peer = %s, want the router %s", peers[0].ID, r.h.ID())
	}
	if len(peers[0].Addrs) != 2 {
		t.Fatalf("peer carries %d addresses, want its 2 distinct ones: %v", len(peers[0].Addrs), peers[0].Addrs)
	}
}

// A router that restarts drops every member's session. The connection
// monitor only acts once a member has no router left, and only on its own
// interval, so a rollout that restarts the routers one after another took
// every session before any came back, and a router restarting alone was
// never re-handshaken. The member must redial a dropped router by itself.
func TestRouterDropIsRedialed(t *testing.T) {
	r := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	h, handshakes := r.h, r.handshakes

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The monitor is kept out of the way: the redial must do this alone.
	node := r.startNode(t, ctx, r.routerAddrs, func(o *Options) {
		o.RouterRedialDelay = 100 * time.Millisecond
		o.MonitorBootstrap = time.Hour
	})
	if got := handshakes.Load(); got != 1 {
		t.Fatalf("handshakes after Start = %d, want 1", got)
	}

	// The router side closes, as a restarting pod does.
	if err := h.Network().ClosePeer(node.Host.ID()); err != nil {
		t.Fatal(err)
	}
	waitForConnectedness(t, node, h.ID(), network.NotConnected)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (!node.IsConnected() || handshakes.Load() != 2) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes after the drop = %d, want 2: the node must handshake the router again", got)
	}
	if !node.IsConnected() {
		t.Fatal("node does not report an authenticated session after the redial")
	}

	// Dropping it again while the first redial has finished starts another;
	// the session comes back a second time.
	if err := h.Network().ClosePeer(node.Host.ID()); err != nil {
		t.Fatal(err)
	}
	waitForConnectedness(t, node, h.ID(), network.NotConnected)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && handshakes.Load() < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := handshakes.Load(); got != 3 {
		t.Fatalf("handshakes after the second drop = %d, want 3", got)
	}
}
