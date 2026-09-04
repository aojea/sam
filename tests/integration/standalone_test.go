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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/node"
	"github.com/google/sam/internal/standalone"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestStandaloneNodeJoin pins the sam-one first-boot CUJ end to end: one
// standalone server provisions its own tokens, policy and embedded router,
// and a stock sam-node enrolls with the generated join token and connects to
// the router over WebSocket through the single public port.
func TestStandaloneNodeJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	srv, err := standalone.New(standalone.Options{
		BindAddress: "127.0.0.1:0",
		DataDir:     dataDir,
	})
	if err != nil {
		t.Fatalf("failed to create standalone server: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start standalone server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// First boot persisted the generated credentials.
	for _, f := range []string{"join-token", "admin-token", "router.key", "sam.db"} {
		if _, err := os.Stat(filepath.Join(dataDir, f)); err != nil {
			t.Errorf("expected %s in data dir: %v", f, err)
		}
	}
	if srv.JoinToken() == "" || srv.AdminToken() == "" {
		t.Fatal("expected generated join and admin tokens")
	}

	// The embedded router self-enrolled and leased over loopback: /info on
	// the public single port advertises it.
	_, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("failed to parse public addr %q: %v", srv.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("failed to parse public port %q: %v", portStr, err)
	}
	waitForActiveRouters(t, port, 1, 10*time.Second)

	// A stock node joins through the public port with the generated token and
	// ends up connected to the router over WebSocket.
	nodeStore, err := node.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create node store: %v", err)
	}
	t.Cleanup(func() { _ = nodeStore.Close() })

	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("failed to generate node key: %v", err)
	}
	samNode, err := node.NewSamNode(node.Options{
		PrivKey:       priv,
		Store:         nodeStore,
		ListenAddrs:   []string{"/ip4/127.0.0.1/tcp/0"},
		AllowLoopback: true,
		RequiredRole:  api.RoleNode,
	})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}
	if err := samNode.Start(ctx); err != nil {
		t.Fatalf("failed to start node: %v", err)
	}
	t.Cleanup(func() {
		if samNode.Host != nil {
			_ = samNode.Host.Close()
		}
	})

	if err := samNode.EnrollBootstrap(ctx, "http://"+srv.Addr(), srv.JoinToken()); err != nil {
		t.Fatalf("node enrollment through the single port failed: %v", err)
	}

	routerID, err := peer.Decode(srv.PeerID())
	if err != nil {
		t.Fatalf("failed to decode router peer ID: %v", err)
	}
	if got := samNode.Host.Network().Connectedness(routerID); got != network.Connected {
		t.Fatalf("node connectedness to embedded router = %s, want Connected", got)
	}
}
