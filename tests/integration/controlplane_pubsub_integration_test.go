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

package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/controlplane"
	"github.com/google/agentmesh/internal/storage"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

func TestControlPlanePubSubEventIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cp.db")
	store, err := storage.NewSQLStore("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 1. Create Control Plane Server
	opts := controlplane.Options{
		ListenAddr: "127.0.0.1:0",
		AdminToken: "test-admin-token",
	}
	cpServer, err := controlplane.NewServer(opts, store)
	if err != nil {
		t.Fatalf("failed to create control plane server: %v", err)
	}

	if err := cpServer.Start(); err != nil {
		t.Fatalf("failed to start control plane server: %v", err)
	}
	defer func() { _ = cpServer.Close() }()

	// Retrieve initial CP public key
	_, pubKey, err := store.GetCurrentKey(ctx)
	if err != nil {
		t.Fatalf("failed to retrieve CP public key: %v", err)
	}

	// 2. Set up P2PMeshAdapter for Control Plane
	cpHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create cp libp2p host: %v", err)
	}
	defer func() { _ = cpHost.Close() }()

	cpPS, err := pubsub.NewGossipSub(ctx, cpHost)
	if err != nil {
		t.Fatalf("failed to create cp pubsub: %v", err)
	}
	cpTopic, err := cpPS.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join cp topic: %v", err)
	}

	meshAdapter, err := controlplane.NewP2PMeshAdapter(cpHost, cpTopic, store)
	if err != nil {
		t.Fatalf("failed to create P2PMeshAdapter: %v", err)
	}
	defer func() { _ = meshAdapter.Close() }()

	cpServer.SetMeshAdapter(meshAdapter)

	// 3. Create a subscriber node listening to api.GossipEvents
	subHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create subscriber host: %v", err)
	}
	defer func() { _ = subHost.Close() }()

	subPS, err := pubsub.NewGossipSub(ctx, subHost)
	if err != nil {
		t.Fatalf("failed to create subscriber pubsub: %v", err)
	}

	topic, err := subPS.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join topic: %v", err)
	}
	sub, err := topic.Subscribe()
	if err != nil {
		t.Fatalf("failed to subscribe to topic: %v", err)
	}

	// Connect subscriber node to Control Plane host
	subHost.Peerstore().AddAddrs(cpHost.ID(), cpHost.Addrs(), 10*time.Second)
	if err := subHost.Connect(ctx, peer.AddrInfo{ID: cpHost.ID(), Addrs: cpHost.Addrs()}); err != nil {
		t.Fatalf("failed to connect subscriber node to cpHost: %v", err)
	}

	// Allow mesh topology to settle
	time.Sleep(500 * time.Millisecond)

	// 4. Trigger POST /policies on Control Plane
	policyReq := &api.PolicyConfig{
		Roles: []*api.PolicyRole{
			{
				Name:            "developer",
				AllowedServices: []string{"mcp://api"},
				AllowedTargets:  []string{"group:db-nodes"},
			},
		},
		Bindings: []*api.PolicyBinding{
			{
				Role:    "developer",
				Members: []string{"user:dev1"},
			},
		},
	}
	policyReqData, err := proto.Marshal(policyReq)
	if err != nil {
		t.Fatalf("failed to marshal policy request: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cpServer.Addr()+"/policies", bytes.NewReader(policyReqData))
	if err != nil {
		t.Fatalf("failed to create policy request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-admin-token")
	req.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to send policy request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK from /policies, got %d", resp.StatusCode)
	}

	// Verify subscriber receives POLICY_UPDATE MeshEvent
	msg, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("failed to receive POLICY_UPDATE event: %v", err)
	}

	var event api.MeshEvent
	if err := proto.Unmarshal(msg.Data, &event); err != nil {
		t.Fatalf("failed to unmarshal MeshEvent: %v", err)
	}
	if event.Type != api.MeshEvent_POLICY_UPDATE {
		t.Fatalf("expected event type POLICY_UPDATE, got %v", event.Type)
	}

	// Verify signature
	sig := event.Signature
	event.Signature = nil
	signedBytes, _ := proto.Marshal(&event)
	if !ed25519.Verify(pubKey, signedBytes, sig) {
		t.Fatalf("signature verification failed for POLICY_UPDATE event")
	}

	// 5. Enroll a dummy node in storage and trigger POST /admin/revoke (BANNED event)
	targetPriv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("failed to generate target key: %v", err)
	}
	targetPeer, err := peer.IDFromPrivateKey(targetPriv)
	if err != nil {
		t.Fatalf("failed to derive target peer ID: %v", err)
	}
	targetPeerID := targetPeer.String()
	nodeRecord := &storage.EnrolledNode{
		PeerID:         targetPeerID,
		PublicKey:      []byte("dummy-key"),
		Biscuit:        []byte("dummy-token"),
		Role:           "node",
		EnrollmentType: "BOOTSTRAP",
		EnrolledAt:     time.Now(),
	}
	if err := store.EnrollNode(ctx, nodeRecord); err != nil {
		t.Fatalf("failed to enroll dummy node: %v", err)
	}

	revokeReq := &api.TokenRevokeRequest{PeerId: targetPeerID}
	revokeReqData, err := proto.Marshal(revokeReq)
	if err != nil {
		t.Fatalf("failed to marshal revoke request: %v", err)
	}

	reqBan, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cpServer.Addr()+"/admin/revoke", bytes.NewReader(revokeReqData))
	if err != nil {
		t.Fatalf("failed to create revoke request: %v", err)
	}
	reqBan.Header.Set("Authorization", "Bearer test-admin-token")
	reqBan.Header.Set("Content-Type", "application/x-protobuf")

	respBan, err := http.DefaultClient.Do(reqBan)
	if err != nil {
		t.Fatalf("failed to send revoke request: %v", err)
	}
	_ = respBan.Body.Close()
	if respBan.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK from /admin/revoke, got %d", respBan.StatusCode)
	}

	// Verify subscriber receives BANNED MeshEvent
	msgBan, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("failed to receive BANNED event: %v", err)
	}

	var eventBan api.MeshEvent
	if err := proto.Unmarshal(msgBan.Data, &eventBan); err != nil {
		t.Fatalf("failed to unmarshal BANNED MeshEvent: %v", err)
	}
	if eventBan.Type != api.MeshEvent_BANNED {
		t.Fatalf("expected event type BANNED, got %v", eventBan.Type)
	}
	if eventBan.PeerId != targetPeerID {
		t.Fatalf("expected peer ID %q, got %q", targetPeerID, eventBan.PeerId)
	}

	// Verify signature
	sigBan := eventBan.Signature
	eventBan.Signature = nil
	signedBanBytes, _ := proto.Marshal(&eventBan)
	if !ed25519.Verify(pubKey, signedBanBytes, sigBan) {
		t.Fatalf("signature verification failed for BANNED event")
	}
}
