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
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/node"
	"github.com/google/sam/internal/standalone"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/encoding/protojson"
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
	samNode := newStandaloneTestNode(t, ctx)
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

	// The embedded web console is served through the same public port.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + srv.Addr() + "/console/")
	if err != nil {
		t.Fatalf("GET /console/ over the single port failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("failed to read console body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /console/ status = %s, want 200", resp.Status)
	}
	if !strings.Contains(string(body), "<html") {
		t.Errorf("GET /console/ did not return the embedded frontend")
	}

	// Device onboarding: the QR payload carries the public URL and a fresh
	// single-use token; the first device enrolls with it, the second is
	// refused because /enroll burned it.
	t.Run("DeviceEnrollmentToken", func(t *testing.T) {
		devTok, err := srv.MintDeviceEnrollmentToken(ctx, 0, 1)
		if err != nil {
			t.Fatalf("failed to mint device token: %v", err)
		}
		if devTok == srv.JoinToken() || !strings.HasPrefix(devTok, "sam_dev_") {
			t.Fatalf("device token %q must be distinct from the join token and prefixed sam_dev_", devTok)
		}
		server, token, err := api.ParseEnrollURI(api.EnrollURI(srv.PublicURL(), devTok))
		if err != nil {
			t.Fatalf("failed to parse enrollment URI: %v", err)
		}
		if server != "http://"+srv.Addr() || token != devTok {
			t.Fatalf("enrollment URI round trip = (%q, %q)", server, token)
		}

		device := newStandaloneTestNode(t, ctx)
		if err := device.EnrollBootstrap(ctx, server, token); err != nil {
			t.Fatalf("device enrollment with the QR token failed: %v", err)
		}
		if got := device.Host.Network().Connectedness(routerID); got != network.Connected {
			t.Fatalf("device connectedness to embedded router = %s, want Connected", got)
		}

		replay := newStandaloneTestNode(t, ctx)
		err = replay.EnrollBootstrap(ctx, server, token)
		if err == nil || !strings.Contains(err.Error(), "exhausted") && !strings.Contains(err.Error(), "max usages") {
			t.Fatalf("second device reusing the burned token: err = %v, want rejection", err)
		}
	})

	// A code projected to a room: one token, a usage budget, several devices.
	t.Run("SharedDeviceEnrollmentToken", func(t *testing.T) {
		shared, err := srv.MintDeviceEnrollmentToken(ctx, 0, 2)
		if err != nil {
			t.Fatalf("failed to mint shared device token: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := newStandaloneTestNode(t, ctx).EnrollBootstrap(ctx, srv.PublicURL(), shared); err != nil {
				t.Fatalf("device %d enrollment with the shared token failed: %v", i+1, err)
			}
		}
		if err := newStandaloneTestNode(t, ctx).EnrollBootstrap(ctx, srv.PublicURL(), shared); err == nil {
			t.Fatal("third device enrolled past the shared token's budget")
		}
	})
}

// TestStandaloneNoJoinToken pins the fleet configuration: no standing join
// token exists anywhere (not in the store, not on disk), the embedded
// router still enrolls with its per-boot token, and devices join only with
// explicitly minted tokens.
func TestStandaloneNoJoinToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir(), DisableJoinToken: true, JoinToken: "sam_tok_x"}); err == nil {
		t.Fatal("DisableJoinToken together with an explicit JoinToken must be rejected")
	}

	dataDir := t.TempDir()
	srv, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: dataDir, DisableJoinToken: true})
	if err != nil {
		t.Fatalf("failed to create standalone server: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start standalone server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if srv.JoinToken() != "" {
		t.Fatalf("JoinToken() = %q, want empty", srv.JoinToken())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "join-token")); !os.IsNotExist(err) {
		t.Fatalf("join-token file must not be written: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+"/admin/bootstrap-tokens", nil)
	req.Header.Set("Authorization", "Bearer "+srv.AdminToken())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/bootstrap-tokens: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "sam-one join token") {
		t.Fatalf("token list = %s %s; want 200 without a join token", resp.Status, body)
	}

	// The embedded router self-enrolled without it, and a minted token still
	// admits a device through the single port.
	_, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	waitForActiveRouters(t, port, 1, 10*time.Second)
	devTok, err := srv.MintDeviceEnrollmentToken(ctx, 0, 1)
	if err != nil {
		t.Fatalf("failed to mint device token: %v", err)
	}
	if err := newStandaloneTestNode(t, ctx).EnrollBootstrap(ctx, srv.PublicURL(), devTok); err != nil {
		t.Fatalf("device enrollment without a join token failed: %v", err)
	}
}

// TestStandalonePolicyFileSeedsEgress pins that a first-boot --policy-file
// is stored whole: the egress destinations it names are served, the same as
// when the document arrives through POST /policies.
func TestStandalonePolicyFileSeedsEgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	policyFile := filepath.Join(t.TempDir(), "policy.json")
	seed := `{
  "roles": [
    {"name": "sam:role:node", "allowed_targets": ["*"], "allowed_labels": ["site=office"]},
    {"name": "agent", "allowed_services": ["egress://api.github.com"], "allowed_targets": ["*"],
     "http": [{"service": "egress://api.github.com", "methods": ["GET"], "paths": ["/repos/acme/*"]}]}
  ],
  "egress": [{"name": "api.github.com", "credential": "github-ro", "served_by": ["site=office"]}]
}`
	if err := os.WriteFile(policyFile, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir(), PolicyFile: policyFile})
	if err != nil {
		t.Fatalf("failed to create standalone server: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start standalone server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+"/admin/policy", nil)
	if err != nil {
		t.Fatalf("GET /admin/policy request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.AdminToken())
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET /admin/policy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET /admin/policy body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/policy = %s %s", resp.Status, body)
	}
	var stored api.PolicyConfig
	if err := protojson.Unmarshal(body, &stored); err != nil {
		t.Fatalf("GET /admin/policy body: %v\n%s", err, body)
	}
	if len(stored.Egress) != 1 || stored.Egress[0].GetName() != "api.github.com" || stored.Egress[0].GetCredential() != "github-ro" {
		t.Fatalf("seeded egress = %v, want api.github.com with credential github-ro", stored.Egress)
	}
	if len(stored.Roles) != 2 {
		t.Fatalf("seeded roles = %d, want 2", len(stored.Roles))
	}
}

// newStandaloneTestNode returns a started, unenrolled loopback node.
func newStandaloneTestNode(t *testing.T, ctx context.Context) *node.SamNode {
	t.Helper()
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
	return samNode
}
