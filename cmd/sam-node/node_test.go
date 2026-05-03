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

package main

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

func TestHandleJoinEvent(t *testing.T) {
	node := &SamNode{
		knownPeers: make(map[string]bool),
	}

	event := &api.MeshEvent{
		Type:   api.MeshEvent_JOIN,
		PeerId: "12D3KooWAFv4iJst5G6MjwXhZ66K5zS1tP7A9vSg4vK8f1T7X8t9",
	}

	node.handleJoinEvent(event)

	if !node.knownPeers[event.PeerId] {
		t.Error("Expected peer to be added to knownPeers")
	}
}

func TestHandleExitEvent(t *testing.T) {
	node := &SamNode{
		knownPeers: map[string]bool{
			"12D3KooWAFv4iJst5G6MjwXhZ66K5zS1tP7A9vSg4vK8f1T7X8t9": true,
		},
	}

	event := &api.MeshEvent{
		Type:   api.MeshEvent_EXIT,
		PeerId: "12D3KooWAFv4iJst5G6MjwXhZ66K5zS1tP7A9vSg4vK8f1T7X8t9",
	}

	node.handleExitEvent(event)

	if node.knownPeers[event.PeerId] {
		t.Error("Expected peer to be removed from knownPeers")
	}
}

func TestHandleBannedEvent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	node := &SamNode{
		knownPeers: map[string]bool{
			"12D3KooWAFv4iJst5G6MjwXhZ66K5zS1tP7A9vSg4vK8f1T7X8t9": true,
		},
		Store: store,
	}

	event := &api.MeshEvent{
		Type:      api.MeshEvent_BANNED,
		PeerId:    "12D3KooWAFv4iJst5G6MjwXhZ66K5zS1tP7A9vSg4vK8f1T7X8t9",
		Timestamp: time.Now().Unix(),
	}

	node.handleBannedEvent(event)

	if node.knownPeers[event.PeerId] {
		t.Error("Expected peer to be removed from knownPeers")
	}

	// We can't directly check Store via `IsBanned` because `peer.Decode` in `handleBannedEvent`
	// might fail if the ID string isn't valid, but for a valid ID it should be banned.
	// Since 12D3... is a valid CID/peer ID format, it should work.
	p, _ := peer.Decode(event.PeerId)
	if !node.Store.IsBanned(p) {
		t.Error("Expected peer to be added to Store as banned")
	}
}

func TestHandleKeyRotationEvent(t *testing.T) {
	node := &SamNode{}

	_, pub, _ := ed25519.GenerateKey(nil)

	event := &api.MeshEvent{
		Type:         api.MeshEvent_KEY_ROTATION,
		NewPublicKey: pub,
	}

	node.handleKeyRotationEvent(event)

	if len(node.trustedKeys) != 1 {
		t.Errorf("Expected 1 trusted key, got %d", len(node.trustedKeys))
	}
}

func TestSamNode_VerifyEvent(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	node := &SamNode{
		trustedKeys: []TrustedKey{
			{Key: pub, ReceivedAt: time.Now()},
		},
	}

	event := &api.MeshEvent{
		Type:      api.MeshEvent_JOIN,
		PeerId:    "some-peer",
		Timestamp: time.Now().Unix(),
	}

	// Sign event
	event.Signature = nil
	data, err := proto.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, data)
	event.Signature = sig

	// Test valid signature
	if !node.verifyEvent(event) {
		t.Error("Expected event to be verified")
	}

	// Test missing signature
	event.Signature = nil
	if node.verifyEvent(event) {
		t.Error("Expected verification to fail with missing signature")
	}

	// Test invalid signature
	event.Signature = []byte("invalid-sig")
	if node.verifyEvent(event) {
		t.Error("Expected verification to fail with invalid signature")
	}

	// Test no trusted keys
	node.keysMu.Lock()
	node.trustedKeys = nil
	node.keysMu.Unlock()
	event.Signature = sig
	if node.verifyEvent(event) {
		t.Error("Expected verification to fail with no trusted keys")
	}

	// Test with empty key in trustedKeys
	node.keysMu.Lock()
	node.trustedKeys = []TrustedKey{{Key: nil, ReceivedAt: time.Now()}}
	node.keysMu.Unlock()
	event.Signature = sig
	if node.verifyEvent(event) {
		t.Error("Expected verification to fail with empty key in trustedKeys")
	}
}

func TestVerifyEvent_Concurrent(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	node := &SamNode{
		trustedKeys: []TrustedKey{
			{Key: pub, ReceivedAt: time.Now()},
		},
	}

	event := &api.MeshEvent{
		Type:      api.MeshEvent_JOIN,
		PeerId:    "some-peer",
		Timestamp: time.Now().Unix(),
	}

	// Sign event
	event.Signature = nil
	data, err := proto.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, data)
	event.Signature = sig

	done := make(chan bool)
	
	// Reader goroutines
	for i := 0; i < 10; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					// Clone the event to simulate independent events being verified
					eventCopy := proto.Clone(event).(*api.MeshEvent)
					node.verifyEvent(eventCopy)
				}
			}
		}()
	}

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			newPub, _, _ := ed25519.GenerateKey(nil)
			node.keysMu.Lock()
			node.trustedKeys = append(node.trustedKeys, TrustedKey{Key: newPub, ReceivedAt: time.Now()})
			if len(node.trustedKeys) > 5 {
				node.trustedKeys = node.trustedKeys[1:] // Prune
			}
			node.keysMu.Unlock()
			time.Sleep(1 * time.Millisecond)
		}
		for i := 0; i < 10; i++ {
			done <- true
		}
	}()

	for i := 0; i < 10; i++ {
		<-done
	}
}
