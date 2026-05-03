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

package registry

import (
	"context"
	"encoding/json"
	"fmt"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
)

// PublishMyCard signs and publishes the local NodeCard to the DHT.
func PublishMyCard(ctx context.Context, kdht *dht.IpfsDHT, privKey crypto.PrivKey, myCard *NodeCard) error {
	// 1. Sign the payload (without signature field set)
	myCard.Signature = nil
	payload, err := json.Marshal(myCard)
	if err != nil {
		return fmt.Errorf("failed to marshal NodeCard for signing: %w", err)
	}

	sig, err := privKey.Sign(payload)
	if err != nil {
		return fmt.Errorf("failed to sign NodeCard: %w", err)
	}
	myCard.Signature = sig

	// 2. Marshal the final card
	finalBytes, err := json.Marshal(myCard)
	if err != nil {
		return fmt.Errorf("failed to marshal final NodeCard: %w", err)
	}

	// 3. Put to the DHT (The Validator will automatically run here!)
	key := "/" + Namespace + "/" + myCard.PeerID
	return kdht.PutValue(ctx, key, finalBytes)
}

// ResolveNodeCard fetches and verifies a remote NodeCard from the DHT.
func ResolveNodeCard(ctx context.Context, kdht *dht.IpfsDHT, peerID string) (*NodeCard, error) {
	key := "/" + Namespace + "/" + peerID

	// GetValue fetches the records. If there are conflicts,
	// the Validator.Select() method automatically returns the newest one.
	val, err := kdht.GetValue(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get value from DHT: %w", err)
	}

	var card NodeCard
	if err := json.Unmarshal(val, &card); err != nil {
		return nil, fmt.Errorf("failed to unmarshal NodeCard: %w", err)
	}
	return &card, nil
}
