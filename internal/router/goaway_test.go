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
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
)

// goAwayListener is a peer that takes go-aways and records them.
type goAwayListener struct {
	host host.Host
	mu   sync.Mutex
	got  []*api.RouterGoAway
}

func newGoAwayListener(t *testing.T) *goAwayListener {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	l := &goAwayListener{host: h}
	h.SetStreamHandler(api.GoAwayProtocolID, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		msg, err := msgio.NewVarintReaderSize(s, 1024).ReadMsg()
		if err != nil {
			return
		}
		var goAway api.RouterGoAway
		if err := proto.Unmarshal(msg, &goAway); err != nil {
			return
		}
		l.mu.Lock()
		l.got = append(l.got, &goAway)
		l.mu.Unlock()
	})
	return l
}

func (l *goAwayListener) received() []*api.RouterGoAway {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*api.RouterGoAway(nil), l.got...)
}

// connectAuthenticated dials the router from p and marks p as a member.
func connectAuthenticated(t *testing.T, r *Router, p host.Host) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Connect(ctx, peer.AddrInfo{ID: r.Host.ID(), Addrs: r.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	r.authenticatedPeers.Store(p.ID(), time.Now().Add(time.Hour))
}

func newGoAwayRouter(t *testing.T, high int) *Router {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return &Router{Host: h, config: Options{HighWaterMark: high, LowWaterMark: DefaultLowWaterMark(high)}}
}

// A router at its high watermark sends enough members away to be back at
// the low one, says why and for how long, and closes their connections
// after the grace; a peer router is never among them, and a member that
// does not speak the protocol is closed all the same.
func TestShedIfOverloaded(t *testing.T) {
	prev := goAwayGrace
	goAwayGrace = 50 * time.Millisecond
	t.Cleanup(func() { goAwayGrace = prev })

	// High 4, low 3: at 4 inbound, one member goes.
	r := newGoAwayRouter(t, 4)
	peerRouter := newGoAwayListener(t)
	connectAuthenticated(t, r, peerRouter.host)
	r.peerRouters.Store(peerRouter.host.ID(), true)
	members := []*goAwayListener{newGoAwayListener(t), newGoAwayListener(t)}
	for _, m := range members {
		connectAuthenticated(t, r, m.host)
	}
	mute, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mute.Close() })
	connectAuthenticated(t, r, mute)

	if got := r.inboundConnections(); got != 4 {
		t.Fatalf("inbound = %d, want 4", got)
	}
	if got := len(r.members()); got != 3 {
		t.Fatalf("members = %d, want 3 (the peer router is not one)", got)
	}

	shed := r.shedIfOverloaded(context.Background())
	if shed != 1 {
		t.Fatalf("shed %d members, want 1", shed)
	}
	var told int
	for _, m := range members {
		for _, g := range m.received() {
			told++
			if g.GetReason() != api.RouterGoAway_OVERLOADED || g.GetRetryAfter().AsDuration() != DefaultOverloadRetryAfter {
				t.Fatalf("go-away = %v, want OVERLOADED for %s", g, DefaultOverloadRetryAfter)
			}
		}
	}
	gone := 0
	for _, p := range []host.Host{members[0].host, members[1].host, mute} {
		if r.Host.Network().Connectedness(p.ID()) != network.Connected {
			gone++
			if r.isPeerAuthenticated(p.ID()) {
				t.Fatalf("%s was sent away but is still authenticated", p.ID())
			}
		}
	}
	if gone != 1 || told > 1 {
		t.Fatalf("after the shed: %d connections closed, %d go-aways delivered; want 1 and at most 1", gone, told)
	}
	if r.Host.Network().Connectedness(peerRouter.host.ID()) != network.Connected || len(peerRouter.received()) != 0 {
		t.Fatal("the peer router was sent away")
	}
	if r.inboundConnections() != 3 {
		t.Fatalf("inbound after the shed = %d, want 3", r.inboundConnections())
	}
	if again := r.shedIfOverloaded(context.Background()); again != 0 {
		t.Fatalf("below the high watermark the router shed %d more", again)
	}
}

// drainMembers tells every member the router is stopping, for the time the
// control plane will keep it listed.
func TestDrainMembersSaysDraining(t *testing.T) {
	prev := goAwayGrace
	goAwayGrace = 50 * time.Millisecond
	t.Cleanup(func() { goAwayGrace = prev })

	r := newGoAwayRouter(t, 100)
	r.config.ShutdownLeaseTTL = 45 * time.Second
	a, b := newGoAwayListener(t), newGoAwayListener(t)
	connectAuthenticated(t, r, a.host)
	connectAuthenticated(t, r, b.host)

	r.drainMembers()

	for _, m := range []*goAwayListener{a, b} {
		got := m.received()
		if len(got) != 1 || got[0].GetReason() != api.RouterGoAway_DRAINING || got[0].GetRetryAfter().AsDuration() != 45*time.Second {
			t.Fatalf("%s received %v, want one DRAINING go-away for 45s", m.host.ID(), got)
		}
		if r.Host.Network().Connectedness(m.host.ID()) == network.Connected {
			t.Fatalf("%s is still connected after the drain", m.host.ID())
		}
	}
}
