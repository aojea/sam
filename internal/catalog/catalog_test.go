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

package catalog_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/sam/internal/catalog"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2p "github.com/libp2p/go-libp2p"
)

// setupLocalNode creates an isolated libp2p host and DHT for testing.
func setupLocalNode(t *testing.T, ctx context.Context) (host.Host, *dht.IpfsDHT) {
	t.Helper()
	
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create host: %v", err)
	}

	// Create a local DHT running in Server Mode so it actively routes
	kdht, err := dht.New(ctx, h, dht.Mode(dht.ModeServer), dht.ProtocolPrefix("/sam"))
	if err != nil {
		t.Fatalf("failed to create DHT: %v", err)
	}

	return h, kdht
}

func TestCatalog_AdvertiseAndDiscover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Setup Node A (The Provider) and Node B (The Consumer)
	hostA, dhtA := setupLocalNode(t, ctx)
	hostB, dhtB := setupLocalNode(t, ctx)
	
	defer func() { _ = hostA.Close() }()
	defer func() { _ = hostB.Close() }()

	// 2. Connect Node A and Node B to form a mini-mesh
	err := hostB.Connect(ctx, peer.AddrInfo{ID: hostA.ID(), Addrs: hostA.Addrs()})
	if err != nil {
		t.Fatalf("Nodes must connect: %v", err)
	}

	// Bootstrap the DHTs so they know about each other
	if err := dhtA.Bootstrap(ctx); err != nil {
		t.Fatalf("dhtA bootstrap failed: %v", err)
	}
	if err := dhtB.Bootstrap(ctx); err != nil {
		t.Fatalf("dhtB bootstrap failed: %v", err)
	}

	// Wait for the routing tables to update
	for i := 0; i < 10; i++ {
		if len(dhtA.RoutingTable().ListPeers()) > 0 && len(dhtB.RoutingTable().ListPeers()) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3. Initialize the Catalogs
	catalogA := catalog.New(hostA, dhtA)
	catalogB := catalog.New(hostB, dhtB)

	// 4. TEST: Node A advertises a capability
	capability := "sam:v1:mcp:tool:test_database"
	err = catalogA.Advertise(ctx, capability)
	if err != nil {
		t.Errorf("Node A should advertise without error: %v", err)
	}

	// 5. TEST: Node B searches for the capability
	// FindProviders can take a moment to traverse the DHT, so we let it run
	providers, err := catalogB.FindProviders(ctx, capability)
	if err != nil {
		t.Errorf("Node B should search without error: %v", err)
	}
	
	// 6. Assertions
	if len(providers) == 0 {
		t.Fatal("Node B should find at least one provider")
	}
	if providers[0].ID != hostA.ID() {
		t.Errorf("The provider found should be Node A, got %s", providers[0].ID)
	}
}

func TestCapabilityToCID_Consistency(t *testing.T) {
	// Ensure the same string always generates the exact same CID
	uri := "sam:v1:a2a:agent:researcher"
	
	cid1, err := catalog.CapabilityToCID(uri)
	if err != nil {
		t.Fatalf("CapabilityToCID failed: %v", err)
	}
	
	cid2, err := catalog.CapabilityToCID(uri)
	if err != nil {
		t.Fatalf("CapabilityToCID failed: %v", err)
	}
	
	if !cid1.Equals(cid2) {
		t.Error("CIDs must be deterministic and identical")
	}
}
