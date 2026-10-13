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
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestConnectingToAnUnattachedRouterHandshakes: a connection the node opens
// to a catalog router it is not attached to, as its DHT does for a lookup,
// is handshaken once and counted as a directory session; the attached
// router's handshake is not repeated by it.
func TestConnectingToAnUnattachedRouterHandshakes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	r1 := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	r2 := newFakeRouterWithKey(t, r1.cpPub, r1.cpPriv, "/ip4/127.0.0.1/tcp/0")
	node := r1.startNode(t, ctx, append(r1.routerAddrs, r2.routerAddrs...), func(o *Options) { o.Routers = 1 })

	attached := node.attachedRouters()
	if len(attached) != 1 {
		t.Fatalf("attached routers = %v, want one", attached)
	}
	other := r2
	if attached[0] == r2.h.ID() {
		other = r1
	}
	if got := node.DirectoryRouters(); got != 0 {
		t.Fatalf("directory routers before any lookup = %d, want 0", got)
	}

	// What the DHT client does when a key lands on the other router.
	if err := node.Host.Connect(ctx, peer.AddrInfo{ID: other.h.ID(), Addrs: other.h.Addrs()}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !node.isAuthenticatedAndConnected(other.h.ID()) {
		if time.Now().After(deadline) {
			t.Fatalf("node did not authenticate to %s after connecting to it; handshakes seen there: %d", other.h.ID(), other.handshakes.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := other.handshakes.Load(); got != 1 {
		t.Errorf("unattached router saw %d handshakes, want 1", got)
	}
	if got := node.DirectoryRouters(); got != 1 {
		t.Errorf("directory routers = %d, want 1", got)
	}
	if got := node.AttachedRouters(); got != 1 {
		t.Errorf("attached routers after the directory handshake = %d, want 1", got)
	}
	if r1.handshakes.Load()+r2.handshakes.Load() != 2 {
		t.Errorf("handshakes in all = %d, want 2: one per router", r1.handshakes.Load()+r2.handshakes.Load())
	}
}
