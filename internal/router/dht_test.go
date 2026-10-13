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
	"testing"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"

	"github.com/google/agentmesh/api"
)

// TestRouterDHTTableHoldsEveryRouter: a router's table bucket takes more
// routers than the library's 20, so a member's lookup gets the closest
// routers to its key from one answer.
func TestRouterDHTTableHoldsEveryRouter(t *testing.T) {
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
	if got := kdht.Mode(); got != dht.ModeServer {
		t.Fatalf("router DHT mode = %v, want %v", got, dht.ModeServer)
	}

	rt := kdht.RoutingTable()
	const routers = 40
	for i := 0; i < routers; i++ {
		p, err := rt.GenRandPeerID(0)
		if err != nil {
			t.Fatalf("GenRandPeerID: %v", err)
		}
		if ok, err := rt.TryAddPeer(p, true, false); !ok {
			t.Fatalf("router #%d refused from the table: %v; a bucket must hold at least %d", i+1, err, routers)
		}
	}
}
