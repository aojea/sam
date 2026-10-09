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
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/storage"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNodeRevocationIntegration(t *testing.T) {
	cpBin := buildBinary(t, "./cmd/sam-control-plane")
	routerBin := buildBinary(t, "./cmd/sam-router")
	nodeBin := buildBinary(t, "./cmd/sam-node")
	clientBin := buildBinary(t, "./cmd/mcp-client")

	tmpDir := t.TempDir()

	// 1. Create a mock policy file granting "router" role
	policyFile := filepath.Join(tmpDir, "policies.yaml")
	policyContent := `bindings:
  - members: ["user:mock-user"]
    role: admin
  - members: ["user:mock-user"]
    role: mesh:role:node
roles:
  - name: admin
    allowed_services: ["*"]
    allowed_targets: ["*"]
`
	writePolicyWithRouter(t, policyFile, policyContent)

	// Start Mock OIDC Server
	oidcURL, mintToken := startCustomMockOIDC(t)

	cpPort := getFreePort(t)
	routerPort := getFreePort(t)

	// 2. Start Control Plane (PostgreSQL is used in GKE/Kind, but SQLite is completely fine here for in-process integration test speed)
	cpCmd := exec.Command(cpBin,
		"--bind-address", fmt.Sprintf("127.0.0.1:%d", cpPort),
		"--admin-token-path", tokenPath(t, "test-admin-token"),
		"--db-dsn", filepath.Join(tmpDir, "cp-keys.db"),
		"--issuer", oidcURL,
		"--insecure-skip-tls-verify",
	)
	cpCmd.Stdout = os.Stdout
	cpCmd.Stderr = os.Stderr
	if err := cpCmd.Start(); err != nil {
		t.Fatalf("failed to start control plane: %v", err)
	}
	defer func() {
		_ = cpCmd.Process.Kill()
		_ = cpCmd.Wait()
	}()

	waitForControlPlane(t, cpPort)
	injectPolicyYAML(t, cpPort, "test-admin-token", policyFile)

	// 3. Start Router
	routerJWT := mintToken(map[string]interface{}{
		"sub":    "router-integration-1",
		"groups": []string{"routers"},
		"roles":  []string{api.RoleRouter},
	})

	routerCmd := exec.Command(routerBin,
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
		"--listen", fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", routerPort),
		"--keys-path", filepath.Join(tmpDir, "router-keys.db"),
		"--allow-loopback",
		"--oidc-token", routerJWT,
		"--lease-renew-interval", "2s",
		"--keys-sync-interval", "1s",
	)
	routerCmd.Stdout = os.Stdout
	routerCmd.Stderr = os.Stderr
	if err := routerCmd.Start(); err != nil {
		t.Fatalf("failed to start router: %v", err)
	}
	defer func() {
		_ = routerCmd.Process.Kill()
		_ = routerCmd.Wait()
	}()

	fetchPeerID(t, cpPort)

	// 4. Start Node 1
	node1Home := filepath.Join(tmpDir, "node1_home")
	node1 := launchNode(t, nodeBin,
		append(os.Environ(), "HOME="+node1Home, "XDG_CONFIG_HOME="+filepath.Join(node1Home, ".config")),
		node1Home, "run",
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
		"--jwt", mintToken(map[string]interface{}{"sub": "mock-user", "roles": []string{api.RoleNode}}),
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--allow-loopback",
		"--api-token-path", tokenPath(t, "node1-token"),
		"--log-level", "debug",
	)

	// 5. Start Node 2
	node2Home := filepath.Join(tmpDir, "node2_home")
	node2 := launchNode(t, nodeBin,
		append(os.Environ(), "HOME="+node2Home, "XDG_CONFIG_HOME="+filepath.Join(node2Home, ".config")),
		node2Home, "run",
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
		"--jwt", mintToken(map[string]interface{}{"sub": "mock-user", "roles": []string{api.RoleNode}}),
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--allow-loopback",
		"--api-token-path", tokenPath(t, "node2-token"),
		"--log-level", "debug",
	)

	node1API := node1.waitForAPI(t)
	node2.waitForAPI(t)

	node2AddrStr := node2.p2pAddr
	node2PeerID := node2.peerID.String()

	// 6. Request Node 1 to connect to Node 2
	connectPeerWithToken(t, node1API, "node1-token", node2AddrStr)

	// Verify Node 1 is connected to Node 2
	time.Sleep(1 * time.Second)
	stdout, stderr, err := runCommand(t, repoRoot(t), 5*time.Second, nil, "",
		clientBin,
		"-url", "http://"+node1API+"/mcp",
		"-token", "node1-token",
		"-tool", "get_mesh_info",
		"-args", "{}",
	)
	if err != nil {
		t.Fatalf("get_mesh_info failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, node2PeerID) {
		t.Fatalf("Node 1 is not connected to Node 2. peers: %s", stdout)
	}

	// 7. Ban Node 2 using CP admin CLI tool
	banCmd := exec.Command(cpBin, "admin", "ban",
		"--peer", node2PeerID,
		"--db-dsn", filepath.Join(tmpDir, "cp-keys.db"),
	)
	banCmd.Stdout = os.Stdout
	banCmd.Stderr = os.Stderr
	if err := banCmd.Run(); err != nil {
		t.Fatalf("failed to ban node via admin CLI: %v", err)
	}

	// Fetch current signing private key from DB
	store, err := storage.NewSQLStore("sqlite", filepath.Join(tmpDir, "cp-keys.db"))
	if err != nil {
		t.Fatalf("failed to open cp database: %v", err)
	}
	validKeys, err := store.GetAllValidKeys(context.Background())
	_ = store.Close()
	if err != nil {
		t.Fatalf("failed to get valid keys: %v", err)
	}
	var cpPrivKey ed25519.PrivateKey
	for _, k := range validKeys {
		if len(k.Private) == ed25519.PrivateKeySize {
			cpPrivKey = k.Private
			break
		}
	}
	if cpPrivKey == nil {
		t.Fatal("no valid CP signing private key found in DB")
	}

	// Create and sign MeshEvent_BANNED
	event := &api.MeshEvent{
		Type:      api.MeshEvent_BANNED,
		PeerId:    node2PeerID,
		EventTime: timestamppb.Now(),
	}
	eventData, err := proto.Marshal(event)
	if err != nil {
		t.Fatalf("failed to marshal event: %v", err)
	}
	event.Signature = ed25519.Sign(cpPrivKey, eventData)

	// Publish Gossip event directly to Node 1
	publishGossipEvent(t, node1.p2pAddr, event)

	// 8. Wait for revocation event to propagate and Node 1 to disconnect Node 2 actively
	waitForNodeDisconnection(t, clientBin, node1API, node2PeerID)
}

func publishGossipEvent(t *testing.T, routerAddrStr string, event *api.MeshEvent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create libp2p host: %v", err)
	}
	defer func() { _ = h.Close() }()

	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		t.Fatalf("failed to create gossipsub: %v", err)
	}

	topic, err := ps.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join topic: %v", err)
	}
	defer func() { _ = topic.Close() }()

	targetAddr, err := multiaddr.NewMultiaddr(routerAddrStr)
	if err != nil {
		t.Fatalf("failed to parse router addr: %v", err)
	}
	targetInfo, err := peer.AddrInfoFromP2pAddr(targetAddr)
	if err != nil {
		t.Fatalf("failed to get AddrInfo: %v", err)
	}

	if err := h.Connect(ctx, *targetInfo); err != nil {
		t.Fatalf("failed to connect to router: %v", err)
	}

	// Wait for connection to settle in mesh (wait for peer to join the topic)
	peerID := targetInfo.ID
	checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
	defer checkCancel()

	found := false
	for !found {
		select {
		case <-checkCtx.Done():
			found = true
		default:
			peers := ps.ListPeers(api.GossipEvents)
			for _, p := range peers {
				if p == peerID {
					found = true
					break
				}
			}
			if !found {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	data, err := proto.Marshal(event)
	if err != nil {
		t.Fatalf("failed to marshal event: %v", err)
	}

	if err := topic.Publish(ctx, data); err != nil {
		t.Fatalf("failed to publish event: %v", err)
	}
}

func waitForNodeDisconnection(t *testing.T, clientBin string, node1API string, node2PeerID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		stdout, _, err := runCommand(t, repoRoot(t), 2*time.Second, nil, "",
			clientBin,
			"-url", "http://"+node1API+"/mcp",
			"-token", "node1-token",
			"-tool", "get_mesh_info",
			"-args", "{}",
		)
		if err == nil && !strings.Contains(stdout, node2PeerID) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Node 1 to disconnect Node 2")
		case <-time.After(200 * time.Millisecond):
		}
	}
}
