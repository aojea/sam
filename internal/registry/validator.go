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
	"errors"
	"fmt"
	"strings"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

const Namespace = "sam"

// NodeCardValidator implements record.Validator for libp2p
type NodeCardValidator struct{}

// Validate checks the cryptographic integrity of the NodeCard
func (v NodeCardValidator) Validate(key string, value []byte) error {
	// Keys are formatted as /sam/<peer_id>
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[1] != Namespace {
		return errors.New("invalid key namespace")
	}
	expectedPeerID := parts[2]

	var card api.NodeCard
	if err := proto.Unmarshal(value, &card); err != nil {
		return errors.New("failed to unmarshal NodeCard")
	}

	if card.PeerId != expectedPeerID {
		return errors.New("record key does not match NodeCard PeerID")
	}

	// 1. Extract Public Key from the Peer ID
	pid, err := peer.Decode(card.PeerId)
	if err != nil {
		return err
	}
	pubKey, err := pid.ExtractPublicKey()
	if err != nil {
		return errors.New("could not extract public key from peer ID")
	}

	// 2. Verify the Signature
	sig := card.Signature
	card.Signature = nil
	payload, err := proto.Marshal(&card)
	if err != nil {
		return fmt.Errorf("failed to marshal NodeCard for verification: %w", err)
	}
	card.Signature = sig // restore

	// We need to be careful here. PublishMyCard marshals the card with Signature = nil,
	// signs it, sets the signature, and then marshals the FINAL card.
	// So the payload we signed was the marshalled card with Signature = nil.
	// That is exactly what we reconstructed above in `payload`.

	valid, err := pubKey.Verify(payload, sig)
	if err != nil || !valid {
		return errors.New("invalid NodeCard signature")
	}

	return nil
}

// Select resolves conflicts by returning the index of the newest NodeCard
func (v NodeCardValidator) Select(key string, values [][]byte) (int, error) {
	bestIdx := -1
	var maxTime int64 = -1

	for i, val := range values {
		var card api.NodeCard
		if err := proto.Unmarshal(val, &card); err != nil {
			continue // Skip corrupted records
		}
		if card.Timestamp > maxTime {
			maxTime = card.Timestamp
			bestIdx = i
		}
	}

	if bestIdx == -1 {
		return 0, errors.New("no valid NodeCards found in select")
	}
	return bestIdx, nil
}
