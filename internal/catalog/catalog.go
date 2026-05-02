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

package catalog

import (
	"context"
	"fmt"

	"github.com/ipfs/go-cid"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
)

// MeshCatalog manages the advertisement and discovery of agent capabilities.
type MeshCatalog struct {
	host host.Host
	dht  *dht.IpfsDHT
}

// New creates a new MeshCatalog using the provided libp2p host and DHT.
func New(h host.Host, routingDHT *dht.IpfsDHT) *MeshCatalog {
	return &MeshCatalog{
		host: h,
		dht:  routingDHT,
	}
}

// CapabilityToCID deterministically converts a namespace string into a libp2p CID.
func CapabilityToCID(namespace string) (cid.Cid, error) {
	// We use SHA2-256 and the Raw codec to generate a consistent hash
	pref := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.Raw),
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}
	
	c, err := pref.Sum([]byte(namespace))
	if err != nil {
		return cid.Undef, fmt.Errorf("failed to create CID for capability: %w", err)
	}
	
	return c, nil
}

// Advertise tells the global mesh that THIS node provides a specific capability.
// It lights up the "Neon Sign" in the Kademlia DHT.
func (c *MeshCatalog) Advertise(ctx context.Context, namespace string) error {
	capabilityCID, err := CapabilityToCID(namespace)
	if err != nil {
		return err
	}

	// Provide announces this node as a provider of the CID.
	// The boolean 'true' means it will be announced to the network immediately.
	if err := c.dht.Provide(ctx, capabilityCID, true); err != nil {
		return fmt.Errorf("dht provide failed: %w", err)
	}

	return nil
}

// FindProviders searches the global mesh for active nodes offering a capability.
func (c *MeshCatalog) FindProviders(ctx context.Context, namespace string) ([]peer.AddrInfo, error) {
	capabilityCID, err := CapabilityToCID(namespace)
	if err != nil {
		return nil, err
	}

	// FindProviders searches the DHT. We return up to 10 peers for redundancy.
	// You can adjust this limit based on mesh size.
	providers, err := c.dht.FindProviders(ctx, capabilityCID)
	if err != nil {
		return nil, fmt.Errorf("dht find providers failed: %w", err)
	}

	// Filter out ourselves if we happen to provide it too
	var externalProviders []peer.AddrInfo
	for _, p := range providers {
		if p.ID != c.host.ID() {
			externalProviders = append(externalProviders, p)
		}
	}

	return externalProviders, nil
}
