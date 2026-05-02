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
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

func TestNodeCardValidator_Validate(t *testing.T) {
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}

	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	card := &api.NodeCard{
		PeerId:    pid.String(),
		Name:      "test-node",
		Timestamp: time.Now().Unix(),
	}

	// Sign the card
	payload, _ := proto.Marshal(card)
	sig, err := priv.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	card.Signature = sig

	value, _ := proto.Marshal(card)

	v := NodeCardValidator{}

	// Valid case
	key := "/sam/" + pid.String()
	if err := v.Validate(key, value); err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Invalid key namespace
	if err := v.Validate("/wrong/" + pid.String(), value); err == nil {
		t.Error("Expected error for invalid namespace")
	}

	// Record key does not match PeerID
	if err := v.Validate("/sam/another-peer-id", value); err == nil {
		t.Error("Expected error for mismatched peer ID")
	}

	// Invalid signature
	card.Signature = []byte("invalid-signature")
	invalidValue, _ := proto.Marshal(card)
	if err := v.Validate(key, invalidValue); err == nil {
		t.Error("Expected error for invalid signature")
	}
}

func TestNodeCardValidator_Select(t *testing.T) {
	v := NodeCardValidator{}
	key := "/sam/some-peer"

	card1 := &api.NodeCard{Timestamp: 100}
	card2 := &api.NodeCard{Timestamp: 200}
	card3 := &api.NodeCard{Timestamp: 150}

	val1, _ := proto.Marshal(card1)
	val2, _ := proto.Marshal(card2)
	val3, _ := proto.Marshal(card3)

	values := [][]byte{val1, val2, val3}

	idx, err := v.Select(key, values)
	if err != nil {
		t.Fatal(err)
	}

	if idx != 1 {
		t.Errorf("Expected index 1 (timestamp 200), got %d", idx)
	}
}
