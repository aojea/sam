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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/controlplane"
	"github.com/google/agentmesh/internal/identity"
	"github.com/google/agentmesh/internal/router"
	"github.com/google/agentmesh/internal/storage"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// A router admits a peer until the biscuit it was shown on the auth
// handshake expires, whatever the connection does, and refuses relay
// circuits to it after that. A member whose platform token is short-lived
// (a projected service account token lasts an hour) gets a credential capped
// to it, refreshes that credential with the control plane well before it
// lapses, and must show the new one to every router it is connected to:
// otherwise the router keeps the first expiry and the member turns
// unreachable an hour after it started while every connection looks fine.
// Every member implementation is run against a real router: sam-node, whose
// own renewal loop refreshes, and each SDK, which refreshes right after
// joining since the credential is shorter than its refresh lead. Two things
// are checked for each: the router's admission of the member moves to the
// refreshed credential, and once the first credential has lapsed a peer
// still opens a relay circuit to the member through the router and gets the
// refreshed credential back on the handshake.
func TestMembersStayAdmittedAcrossCredentialRefresh(t *testing.T) {
	// Parallel subtests outlive this function's body.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	mesh := startReadmissionMesh(t, ctx)
	nodeBin := buildBinary(t, "./cmd/sam-node")

	// How long the token every member joins with vouches for it: past each
	// runner's startup, short enough for sam-node's renewal loop, which
	// refreshes halfway through a credential this short, and for the test to
	// outlive it.
	const lifetime = 10 * time.Second

	t.Run("sam-node", func(t *testing.T) {
		t.Parallel()
		jwtFile := writeJWTFile(t, mesh.memberJWT("node-member", lifetime))
		home := t.TempDir()
		firstExpiry := time.Now().Add(lifetime)
		samNode := launchNode(t, nodeBin,
			append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
			home, "run",
			"--control-plane", mesh.baseURL,
			"--jwt-path", jwtFile,
			"--allow-loopback",
			"--api-token-path", tokenPath(t, "node-token"),
		)
		samNode.waitForAPI(t)
		mesh.requireAdmittedUntil(t, samNode.peerID, firstExpiry)
		// The platform rotated the token file, as kubelet does, before the
		// node's renewal reads it.
		rewriteJWTFile(t, jwtFile, mesh.memberJWT("node-member", time.Hour))
		mesh.waitForReadmission(t, samNode.peerID, firstExpiry)
		mesh.requireReachableAcrossExpiry(t, ctx, samNode.peerID, firstExpiry)
	})

	for _, runner := range sdkMemberLaunchers {
		t.Run(runner.name, func(t *testing.T) {
			t.Parallel()
			cmd, skip := runner.cmd(mesh.root)
			if cmd == nil {
				t.Skip(skip)
			}
			jwtFile := writeJWTFile(t, mesh.memberJWT(runner.name+"-member", lifetime))
			firstExpiry := time.Now().Add(lifetime)
			m := launchSDKRunner(t, runner.name, cmd, mesh.root, mesh.baseURL, "SAM_JWT_PATH="+jwtFile, "SAM_SDK_LISTEN_ADDRS=/ip4/127.0.0.1/tcp/0")
			rewriteJWTFile(t, jwtFile, mesh.memberJWT(runner.name+"-member", time.Hour))
			peerID, err := peer.Decode(m.report.PeerID)
			if err != nil {
				t.Fatal(err)
			}
			mesh.requireAdmittedUntil(t, peerID, firstExpiry)
			mesh.waitForReadmission(t, peerID, firstExpiry)
			mesh.requireReachableAcrossExpiry(t, ctx, peerID, firstExpiry)
		})
	}
}

// readmissionMesh is a control plane and one router, both in this process,
// so the test can read the router's admission of each member.
type readmissionMesh struct {
	root       string
	baseURL    string
	router     *router.Router
	routerAddr string
	cpPriv     ed25519.PrivateKey
	mintToken  func(map[string]interface{}) string
}

func startReadmissionMesh(t *testing.T, ctx context.Context) *readmissionMesh {
	t.Helper()
	oidcURL, mintToken := startCustomMockOIDC(t)
	tmp := t.TempDir()
	store, err := storage.NewSQLStore("sqlite", filepath.Join(tmp, "cp.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Members hold the node role through group "users", the router its role
	// through group "routers"; what either may reach is not the subject here.
	roles := []*api.PolicyRole{
		{Name: api.RoleRouter, AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}},
		{Name: api.RoleNode, AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}},
	}
	bindings := []*api.PolicyBinding{
		{Role: api.RoleRouter, Members: []string{"group:routers"}},
		{Role: api.RoleNode, Members: []string{"group:users"}},
	}
	if err := store.SaveMeshPolicy(ctx, roles, bindings); err != nil {
		t.Fatalf("failed to save policy: %v", err)
	}
	cp, err := controlplane.NewServer(controlplane.Options{
		ListenAddr:            "127.0.0.1:0",
		OIDCIssuer:            oidcURL,
		AllowedAudiences:      []string{"sam-mesh-audience"},
		LeaseDuration:         5 * time.Second,
		InsecureSkipTLSVerify: true,
	}, store)
	if err != nil {
		t.Fatalf("failed to create control plane: %v", err)
	}
	if err := cp.Start(); err != nil {
		t.Fatalf("failed to start control plane: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })
	baseURL := "http://" + cp.Addr()
	_, portStr, _ := net.SplitHostPort(cp.Addr())
	cpPort, _ := strconv.Atoi(portStr)

	rtr, err := router.NewRouter(ctx, router.Options{
		ControlPlaneURL:    baseURL,
		ListenAddrs:        []string{"/ip4/127.0.0.1/tcp/0"},
		KeysDBPath:         filepath.Join(tmp, "router.key"),
		OIDCToken:          mintToken(map[string]interface{}{"sub": "router", "groups": []string{"routers"}, "roles": []string{api.RoleRouter}}),
		AllowLoopback:      true,
		KeysSyncInterval:   20 * time.Second,
		LeaseRenewInterval: 500 * time.Millisecond,
		BiscuitTimeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	if err := rtr.Start(); err != nil {
		t.Fatalf("failed to start router: %v", err)
	}
	t.Cleanup(func() { _ = rtr.Close() })
	routerAddr := ""
	for _, a := range rtr.Host.Addrs() {
		if _, err := a.ValueForProtocol(multiaddr.P_TCP); err == nil {
			routerAddr = a.String() + "/p2p/" + rtr.Host.ID().String()
			break
		}
	}
	if routerAddr == "" {
		t.Fatalf("router advertises no tcp address, got %v", rtr.Host.Addrs())
	}
	// Members learn the router from the control plane, which knows it from
	// its first lease.
	waitForActiveRouters(t, cpPort, 1, 10*time.Second)

	cpPriv, _, err := store.GetCurrentKey(ctx)
	if err != nil {
		t.Fatalf("failed to load control plane signing key: %v", err)
	}
	return &readmissionMesh{
		root:       repoRoot(t),
		baseURL:    baseURL,
		router:     rtr,
		routerAddr: routerAddr,
		cpPriv:     cpPriv,
		mintToken:  mintToken,
	}
}

// memberJWT is a platform token for a member, vouching for it for lifetime;
// the control plane caps the credential it issues to that.
func (m *readmissionMesh) memberJWT(subject string, lifetime time.Duration) string {
	return m.mintToken(map[string]interface{}{
		"sub":    subject,
		"groups": []string{"users"},
		"roles":  []string{api.RoleNode},
		"exp":    time.Now().Add(lifetime).Unix(),
	})
}

// requireAdmittedUntil checks the premise: the router admitted member on
// the credential it joined with, which expires no later than firstExpiry.
// Biscuit dates carry whole seconds.
func (m *readmissionMesh) requireAdmittedUntil(t *testing.T, member peer.ID, firstExpiry time.Time) {
	t.Helper()
	until, admitted := m.router.AdmittedUntil(member)
	if !admitted {
		t.Fatalf("router does not admit %s after it joined", member)
	}
	if until.After(firstExpiry.Add(time.Second)) {
		t.Fatalf("router admits %s until %s, past the %s expiry of the credential it joined with: the member refreshed before its first handshake, so this test proves nothing",
			member, until.Format(time.RFC3339), firstExpiry.Format(time.RFC3339))
	}
}

// waitForReadmission waits until the router's admission of member outlasts
// firstExpiry, the expiry of the credential the member joined with. A member
// that refreshed without showing the router keeps the first expiry. This is
// the router's own state, read directly, so a member that never re-presents
// is named within seconds rather than when the credential lapses.
func (m *readmissionMesh) waitForReadmission(t *testing.T, member peer.ID, firstExpiry time.Time) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		until, admitted := m.router.AdmittedUntil(member)
		if admitted && until.After(firstExpiry.Add(time.Minute)) {
			t.Logf("router admits %s until %s, %s past the credential it joined with", member, until.Format(time.RFC3339), until.Sub(firstExpiry).Round(time.Second))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("router still admits %s until %s (admitted=%v), the credential it was shown at join expired at %s: the refreshed credential was not presented",
				member, until.Format(time.RFC3339), admitted, firstExpiry.Format(time.RFC3339))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireReachableAcrossExpiry is the data plane's answer to the same
// question. Before firstExpiry a peer reaches member through the router, as
// it would with or without a refresh. Once firstExpiry has passed, a peer
// that was not connected before opens a relay circuit to member through the
// router, runs the auth handshake over it and is answered with a credential
// that outlives firstExpiry: the refreshed one. A router still holding the
// first expiry refuses that circuit with PERMISSION_DENIED.
func (m *readmissionMesh) requireReachableAcrossExpiry(t *testing.T, ctx context.Context, member peer.ID, firstExpiry time.Time) {
	t.Helper()
	// The member's reservation on the router is made shortly after it joins.
	var before time.Time
	var err error
	for {
		if before, err = m.handshakeThroughRelay(t, ctx, member); err == nil {
			break
		}
		if time.Now().After(firstExpiry.Add(-time.Second)) {
			t.Fatalf("no relay circuit to %s through the router before its first credential expired: %v", member, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Strictly after the expiry: the router compares against the biscuit's
	// whole-second date.
	time.Sleep(time.Until(firstExpiry.Add(time.Second)))
	after, err := m.handshakeThroughRelay(t, ctx, member)
	if err != nil {
		t.Fatalf("no relay circuit to %s through the router once the credential it joined with expired (it presented one valid until %s before): %v", member, before.Format(time.RFC3339), err)
	}
	if !after.After(firstExpiry) {
		t.Fatalf("%s answered the handshake with a credential expiring %s, not a refreshed one past %s", member, after.Format(time.RFC3339), firstExpiry.Format(time.RFC3339))
	}
	t.Logf("%s reached through the router after its first credential expired; it presents one valid until %s", member, after.Format(time.RFC3339))
}

// handshakeThroughRelay opens a relay circuit to member through the router
// from a freshly admitted peer, so no earlier connection is reused, and runs
// the auth handshake over it, the exchange every member answers. It returns
// the expiry of the credential member presented.
func (m *readmissionMesh) handshakeThroughRelay(t *testing.T, ctx context.Context, member peer.ID) (time.Time, error) {
	t.Helper()
	caller := newAdmittedGoPeer(t, ctx, m.cpPriv, []string{m.routerAddr})
	relayed := multiaddr.StringCast(m.routerAddr + "/p2p-circuit/p2p/" + member.String())
	dialCtx, cancel := context.WithTimeout(network.WithAllowLimitedConn(ctx, "test"), 10*time.Second)
	defer cancel()
	if err := caller.Connect(dialCtx, peer.AddrInfo{ID: member, Addrs: []multiaddr.Multiaddr{relayed}}); err != nil {
		return time.Time{}, err
	}
	presented := authHandshake(t, ctx, caller, member, goHostBiscuit(t, m.cpPriv, caller.ID()))
	return identity.VerifyBiscuitAndGetExpiry(presented, member, []ed25519.PublicKey{m.cpPriv.Public().(ed25519.PublicKey)}, 5*time.Second)
}

// writeJWTFile writes token where a member reads its platform token from.
func writeJWTFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jwt")
	rewriteJWTFile(t, path, token)
	return path
}

// rewriteJWTFile replaces the token at path in one step, as a platform
// rotating a projected token does, so a member never reads half a token.
func rewriteJWTFile(t *testing.T, path, token string) {
	t.Helper()
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
