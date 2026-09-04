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
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/controlplane"
	"github.com/google/sam/internal/router"
	"github.com/google/sam/internal/storage"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// TestSinglePortWebSocketAndHTTP pins the single-binary (sam-one) topology:
// one TCP socket, owned by the router's WebSocket transport, carries both
// libp2p traffic (WebSocket upgrades) and plain HTTP requests (fallback
// handler), including control-plane routes registered on the shared mux. The
// router itself enrolls and leases against the control plane over loopback
// with a bootstrap token and no OIDC issuer configured.
func TestSinglePortWebSocketAndHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmp := t.TempDir()
	store, err := storage.NewSQLStore("sqlite", filepath.Join(tmp, "cp.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Bootstrap-token-only control plane: no OIDC issuer at all.
	cp, err := controlplane.NewServer(controlplane.Options{
		ListenAddr:            "127.0.0.1:0",
		AutoApproveEnrollment: true,
	}, store)
	if err != nil {
		t.Fatalf("failed to create control plane: %v", err)
	}
	if err := cp.Start(); err != nil {
		t.Fatalf("failed to start control plane: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	// Seed the router's bootstrap token directly in the store, the way a
	// single-binary distribution provisions its embedded router on first boot.
	const routerToken = "single-port-router-token"
	tokenID := fmt.Sprintf("%x", sha256.Sum256([]byte(routerToken)))
	now := time.Now()
	if err := store.SaveBootstrapToken(ctx, &storage.BootstrapToken{
		ID:        tokenID,
		TokenHash: tokenID,
		Role:      api.RoleRouter,
		MaxUsages: 1,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("failed to seed router bootstrap token: %v", err)
	}

	// Shared mux served as the WS transport's HTTP fallback: control-plane
	// routes plus a probe endpoint.
	mux := http.NewServeMux()
	cp.RegisterRoutes(mux)
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("single-port ok"))
	})

	rtr, err := router.NewRouter(ctx, router.Options{
		ControlPlaneURL:     "http://" + cp.Addr(),
		ListenAddrs:         []string{"/ip4/127.0.0.1/tcp/0/ws"},
		AllowLoopback:       true,
		KeysDBPath:          filepath.Join(tmp, "router.key"),
		BootstrapToken:      routerToken,
		HTTPFallbackHandler: mux,
	})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	if err := rtr.Start(); err != nil {
		t.Fatalf("failed to start router: %v", err)
	}
	t.Cleanup(func() { _ = rtr.Close() })

	// Locate the bound single port from the router's WS multiaddr.
	var wsAddr multiaddr.Multiaddr
	var port string
	for _, a := range rtr.Host.Addrs() {
		if _, err := a.ValueForProtocol(multiaddr.P_WS); err == nil {
			wsAddr = a
			if port, err = a.ValueForProtocol(multiaddr.P_TCP); err != nil {
				t.Fatalf("ws multiaddr %s has no tcp port: %v", a, err)
			}
			break
		}
	}
	if wsAddr == nil {
		t.Fatalf("router advertises no /ws listen addr, got %v", rtr.Host.Addrs())
	}

	// 1. Plain HTTP through the libp2p-owned socket reaches the fallback mux,
	// including the control-plane API.
	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://127.0.0.1:" + port
	for _, path := range []string{"/hello", "/healthz", "/info"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s over the single port failed: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s over the single port: status %s, want 200", path, resp.Status)
		}
		_ = resp.Body.Close()
	}

	// 2. A default-transports libp2p client (same dial path as sam-node)
	// connects over the very same port.
	dialer, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("failed to create dialer host: %v", err)
	}
	t.Cleanup(func() { _ = dialer.Close() })

	if err := dialer.Connect(ctx, peer.AddrInfo{ID: rtr.Host.ID(), Addrs: []multiaddr.Multiaddr{wsAddr}}); err != nil {
		t.Fatalf("libp2p dial over the single port failed: %v", err)
	}
	if got := dialer.Network().Connectedness(rtr.Host.ID()); got != network.Connected {
		t.Fatalf("connectedness to router = %s, want Connected", got)
	}

	// 3. The router self-enrolled with the bootstrap token and leased over
	// loopback: /info must advertise it.
	_, cpPortStr, err := net.SplitHostPort(cp.Addr())
	if err != nil {
		t.Fatalf("failed to parse control plane addr %q: %v", cp.Addr(), err)
	}
	cpPort, err := strconv.Atoi(cpPortStr)
	if err != nil {
		t.Fatalf("failed to parse control plane port %q: %v", cpPortStr, err)
	}
	waitForActiveRouters(t, cpPort, 1, 10*time.Second)
}
