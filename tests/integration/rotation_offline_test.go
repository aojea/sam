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
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
)

// TestKeyRotationWhileMembersOffline is the outage the testnets had twice:
// the control plane rotates its signing key while a member is not running.
// The routers refresh their credentials under the new key within a second
// of the rotation. The member comes back with what it persisted, a
// credential and a trusted key set that both predate the rotation, and
// must still join: pull the new key from the control plane, where the
// retiring key vouches for it, refresh its credential under it, and be
// admitted by routers whose credentials it could not have verified with the
// keys it had on disk. Both SDKs and the Go node are each enrolled, stopped,
// and started again from their state after the rotation, with no enrollment
// token to fall back on, so a member that re-enrolled instead of resuming
// would fail here too.
func TestKeyRotationWhileMembersOffline(t *testing.T) {
	mesh := startSDKMesh(t)
	ctx := context.Background()
	nodeBin := buildBinary(t, "./cmd/sam-node")

	// The members, enrolled under the current key, then stopped. Each keeps
	// its state directory; a token is read only to enroll, so the file can
	// go once the member holds a credential.
	type offlineSDKMember struct {
		launcher sdkRunner
		stateDir string
		peerID   string
		biscuit  string
	}
	var sdkMembers []offlineSDKMember
	for _, launcher := range sdkMemberLaunchers {
		cmd, skip := launcher.cmd(mesh.root)
		if skip != "" {
			t.Logf("%s SDK skipped: %s", launcher.name, skip)
			continue
		}
		stateDir := filepath.Join(t.TempDir(), "state")
		m := launchSDKMember(t, launcher.name, cmd, mesh.root, mesh.baseURL, mesh.adminToken, "SAM_SDK_STATE_DIR="+stateDir)
		sdkMembers = append(sdkMembers, offlineSDKMember{launcher: launcher, stateDir: stateDir, peerID: m.report.PeerID, biscuit: m.report.Biscuit})
		m.quit(t)
	}

	// The Go node: enrolled through OIDC, its identity and mesh config in
	// its data directory, its credential readable on the owner socket.
	nodeHome := filepath.Join(t.TempDir(), "node")
	if err := os.MkdirAll(nodeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	nodeEnv := append(os.Environ(), "HOME="+nodeHome, "XDG_CONFIG_HOME="+filepath.Join(nodeHome, ".config"))
	socketDir, err := os.MkdirTemp("", "sam-rot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	nodeSocket := filepath.Join(socketDir, "node.sock")
	nodeArgs := []string{"run",
		"--control-plane", mesh.baseURL,
		"--allow-loopback",
		"--api-token-path", tokenPath(t, "node-token"),
		"--socket-path", nodeSocket,
		"--log-level", "debug",
	}
	node := launchNode(t, nodeBin, nodeEnv, nodeHome,
		append(nodeArgs, "--jwt", mesh.mintToken(map[string]interface{}{"sub": "mock-user", "roles": []string{api.RoleNode}}))...)
	node.waitForAPI(t)
	socketClient := identityEvidenceSocketClient(nodeSocket)
	waitForIdentityEvidenceSocket(t, socketClient)
	var nodeBefore api.IdentityEvidenceResponse
	getIdentityEvidenceJSON(t, socketClient, "/sam/identity", &nodeBefore)
	nodePeer, err := peer.Decode(nodeBefore.PeerId)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBiscuitForApplication(nodeBefore.Biscuit, nodePeer, []ed25519.PublicKey{mesh.cpPub}); err != nil {
		t.Fatalf("node credential before the rotation does not verify under the current key: %v", err)
	}
	node.kill()

	// The rotation, as the sam-control-plane binary makes it: a new current
	// key, the old one retiring over a grace period, the event on the mesh.
	// Every router refreshes its credential under the new key; a member
	// that trusts only the old key can verify none of them any more.
	newPub, newPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mesh.store.RotateKeys(ctx, newPriv, newPub, time.Hour); err != nil {
		t.Fatalf("RotateKeys: %v", err)
	}
	if err := mesh.publisher.PublishEvent(ctx, api.MeshEvent_KEY_ROTATION, "", newPub); err != nil {
		t.Fatalf("publish KEY_ROTATION: %v", err)
	}
	waitForRoutersUnderKey(t, ctx, mesh, newPub)

	// The SDK members start again from their state directories. The token
	// path names no file: the member resumes or fails, it cannot enroll.
	for _, s := range sdkMembers {
		cmd, _ := s.launcher.cmd(mesh.root)
		m := launchSDKMember(t, s.launcher.name+" resumed", cmd, mesh.root, mesh.baseURL, mesh.adminToken,
			"SAM_SDK_STATE_DIR="+s.stateDir,
			"SAM_BOOTSTRAP_TOKEN_PATH="+filepath.Join(t.TempDir(), "no-token"))
		if m.report.PeerID != s.peerID {
			t.Fatalf("%s came back as %s, want the identity it persisted, %s", m.name, m.report.PeerID, s.peerID)
		}
		assertAdmittedByEveryRouter(t, m, mesh.routerAddrs)
		if m.report.Biscuit == s.biscuit {
			t.Fatalf("%s still holds the credential minted before the rotation", m.name)
		}
		assertSignedOnlyBy(t, m.name, m.report.Biscuit, m.report.PeerID, newPub, mesh.cpPub)
		if res := m.sync(t); !res.OK {
			t.Fatalf("%s: pull after the resume failed: %s", m.name, res.Error)
		}
		// The node across the mesh admits the resumed member too.
		if cred := m.auth(t, mesh.samNode.p2pAddr); cred.PeerID != mesh.samNode.peerID.String() {
			t.Fatalf("%s verified the node as %+v after the resume", m.name, cred)
		}
		m.quit(t)
	}

	// The Go node starts from its stored identity, with no token: it pulls
	// from the control plane before it starts and comes up under the new key.
	node = launchNode(t, nodeBin, nodeEnv, nodeHome, nodeArgs...)
	node.waitForAPI(t)
	waitForIdentityEvidenceSocket(t, socketClient)
	var nodeAfter api.IdentityEvidenceResponse
	getIdentityEvidenceJSON(t, socketClient, "/sam/identity", &nodeAfter)
	if nodeAfter.PeerId != nodeBefore.PeerId {
		t.Fatalf("node came back as %s, want its stored identity %s", nodeAfter.PeerId, nodeBefore.PeerId)
	}
	if _, err := verifyBiscuitForApplication(nodeAfter.Biscuit, nodePeer, []ed25519.PublicKey{newPub}); err != nil {
		t.Fatalf("node credential after the resume does not verify under the new key: %v\n--- node.log ---\n%s", err, node.log())
	}
	if _, err := verifyBiscuitForApplication(nodeAfter.Biscuit, nodePeer, []ed25519.PublicKey{mesh.cpPub}); err == nil {
		t.Fatal("node credential after the resume still verifies under the retiring key: it was not refreshed")
	}
	waitForPeerOnRouter(t, mesh.cpPort, mesh.adminToken, nodeBefore.PeerId, 10*time.Second)
	node.kill()
}

// waitForRoutersUnderKey returns once every router presents a credential
// signed by pub in the auth handshake. A Go peer holding a credential under
// the retiring key, still valid, runs the handshake.
func waitForRoutersUnderKey(t *testing.T, ctx context.Context, mesh *sdkMesh, pub ed25519.PublicKey) {
	t.Helper()
	h, err := libp2p.New(libp2p.NoListenAddrs, libp2p.Security(libp2ptls.ID, libp2ptls.New))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	for _, routerAddr := range mesh.routerAddrs {
		info, err := peer.AddrInfoFromString(routerAddr)
		if err != nil {
			t.Fatal(err)
		}
		connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = h.Connect(connectCtx, *info)
		cancel()
		if err != nil {
			t.Fatalf("could not connect to router %s: %v", routerAddr, err)
		}
		for {
			routerBiscuit := authHandshake(t, ctx, h, info.ID, goHostBiscuit(t, mesh.cpPriv, h.ID()))
			if err := identity.VerifyBiscuitRole(routerBiscuit, pub, api.RoleRouter, 5*time.Second); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("router %s did not refresh its credential under the new key", info.ID)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// assertAdmittedByEveryRouter checks the join report names every router the
// control plane lists, each verified as holding the router role.
func assertAdmittedByEveryRouter(t *testing.T, m *sdkMember, routerAddrs []string) {
	t.Helper()
	var admitted, want []string
	for _, r := range m.report.Routers {
		admitted = append(admitted, r.PeerID)
		if !contains(r.Roles, api.RoleRouter) {
			t.Fatalf("%s: router %s credential roles %v lack %s", m.name, r.PeerID, r.Roles, api.RoleRouter)
		}
	}
	for _, a := range routerAddrs {
		want = append(want, extractPeerID(a))
	}
	sort.Strings(admitted)
	sort.Strings(want)
	if fmt.Sprint(admitted) != fmt.Sprint(want) {
		t.Fatalf("%s was admitted by %v, want every router %v", m.name, admitted, want)
	}
}

// assertSignedOnlyBy checks a base64 credential verifies for peerID under
// signer and under none of the others.
func assertSignedOnlyBy(t *testing.T, who, encoded, peerID string, signer ed25519.PublicKey, others ...ed25519.PublicKey) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("%s credential is not base64: %v", who, err)
	}
	id, err := peer.Decode(peerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.VerifyBiscuitAndGetExpiry(raw, id, []ed25519.PublicKey{signer}, 5*time.Second); err != nil {
		t.Fatalf("%s credential does not verify under the new key: %v", who, err)
	}
	for _, other := range others {
		if _, err := identity.VerifyBiscuitAndGetExpiry(raw, id, []ed25519.PublicKey{other}, 5*time.Second); err == nil {
			t.Fatalf("%s credential still verifies under a retiring key", who)
		}
	}
}
