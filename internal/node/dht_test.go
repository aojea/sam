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

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/protocol"

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
	memberDHT, err := dht.New(member, dhtOptions(Options{})...)
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
