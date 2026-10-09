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
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/identity"
	"github.com/google/agentmesh/internal/node"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// testBiscuitTTL is short enough to expire inside the test's time budget while
// still leaving room for the enrollment round trip to be observed as valid.
const testBiscuitTTL = 2 * time.Second

// Biscuit date terms carry whole seconds, so the injected time($now) fact only
// compares greater than expiration() a full second past it.
const expiryMargin = 2 * time.Second

// TestBiscuitExpiryIsEnforcedOnEveryPath is the end-to-end reproduction of #296.
//
// A real control plane mints a real biscuit over the real /register flow, and
// the same token is then presented to both verification paths: the generic
// verifier (identity.VerifyBiscuit) and the node dataplane authorizer
// (AgentMeshNode.VerifyBiscuitToken, the tool-invocation path). Before the expiry
// both accept it; after the expiry both must reject it. The bug was that the
// dataplane kept accepting, because it never injected the time fact that the
// expiration check reads.
//
// It also pins the second half of the fix: the biscuit's lifetime is the
// admin-configured --biscuit-ttl, and EnrollResponse.expire_time (which drives
// the node's proactive refresh) reports that same instant rather than the OIDC
// token's own, much later, expiry.
func TestBiscuitExpiryIsEnforcedOnEveryPath(t *testing.T) {
	cpBin := buildBinary(t, "./cmd/agentmesh-control-plane")
	tmpDir := t.TempDir()

	oidcURL, mintToken := startCustomMockOIDC(t)
	cpPort := getFreePort(t)

	cpCmd := exec.Command(cpBin,
		"--bind-address", fmt.Sprintf("127.0.0.1:%d", cpPort),
		"--admin-token-path", tokenPath(t, "test-admin-token"),
		"--db-dsn", filepath.Join(tmpDir, "cp-keys.db"),
		"--issuer", oidcURL,
		"--insecure-skip-tls-verify",
		"--biscuit-ttl", testBiscuitTTL.String(),
	)
	cpCmd.Stdout = os.Stdout
	cpCmd.Stderr = os.Stderr
	if err := cpCmd.Start(); err != nil {
		t.Fatalf("failed to start control plane: %v", err)
	}
	defer func() { _ = cpCmd.Process.Kill(); _ = cpCmd.Wait() }()
	waitForControlPlane(t, cpPort)

	// Node enrollment requires the requested role to resolve from a binding.
	// The grants mirror the shape this test always ran with (the no-policy
	// mint fallback): unrestricted, because the subject here is expiry.
	policyFile := filepath.Join(tmpDir, "policies.yaml")
	policyYAML := "roles:\n  - name: mesh:role:node\n    allowed_services: [\"*\"]\n    allowed_targets: []\n    custom_datalog: [\"target_unrestricted(true);\"]\nbindings:\n  - role: mesh:role:node\n    members: [\"user:expiry-user\"]\n"
	if err := os.WriteFile(policyFile, []byte(policyYAML), 0644); err != nil {
		t.Fatal(err)
	}
	injectPolicyYAML(t, cpPort, "test-admin-token", policyFile)

	privKey, pubKey, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerID, err := peer.IDFromPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}

	jwtToken := mintToken(map[string]interface{}{
		"sub":   "expiry-user",
		"roles": []string{api.RoleNode},
	})

	mintedAt := time.Now()
	enrollResp := registerOnControlPlane(t, cpPort, peerID, privKey, jwtToken)
	biscuitToken := enrollResp.BiscuitToken
	cpPubKey := ed25519.PublicKey(enrollResp.ControlPlanePublicKey)

	// The advertised expiration is the biscuit's, not the OIDC token's (1h).
	reported := enrollResp.GetExpireTime().AsTime()
	if skew := reported.Sub(mintedAt.Add(testBiscuitTTL)); skew < -2*time.Second || skew > 2*time.Second {
		t.Errorf("EnrollResponse.expire_time is %v, want ~%v (--biscuit-ttl %v after minting)",
			reported, mintedAt.Add(testBiscuitTTL), testBiscuitTTL)
	}

	store, err := node.NewStore(filepath.Join(tmpDir, "node-data"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	samNode, err := node.NewAgentMeshNode(node.Options{
		PrivKey:            privKey,
		Store:              store,
		ControlPlanePubKey: cpPubKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	samNode.BiscuitTimeout = 5 * time.Second

	reqCtx := node.RequestContext{PeerID: peerID, Protocol: string(api.MCPProtocolID)}

	// Fresh: both paths agree the token is good.
	if _, err := identity.VerifyBiscuit(biscuitToken, peerID, []ed25519.PublicKey{cpPubKey}, 5*time.Second); err != nil {
		t.Fatalf("generic verifier rejected a freshly minted biscuit: %v", err)
	}
	if err := samNode.VerifyBiscuitToken(biscuitToken, reqCtx); err != nil {
		t.Fatalf("node dataplane rejected a freshly minted biscuit: %v", err)
	}

	time.Sleep(time.Until(reported) + expiryMargin)

	// Expired: both paths must agree it is no longer good.
	if _, err := identity.VerifyBiscuit(biscuitToken, peerID, []ed25519.PublicKey{cpPubKey}, 5*time.Second); err == nil {
		t.Error("generic verifier accepted an expired biscuit")
	}
	if err := samNode.VerifyBiscuitToken(biscuitToken, reqCtx); err == nil {
		t.Error("node dataplane accepted an expired biscuit (#296)")
	}
}

func registerOnControlPlane(t *testing.T, cpPort int, clientID peer.ID, privKey crypto.PrivKey, jwtToken string) *api.EnrollResponse {
	t.Helper()

	pubBytes, err := crypto.MarshalPublicKey(privKey.GetPublic())
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UnixMilli()
	sig, err := privKey.Sign(api.RegisterChallenge(clientID.String(), ts))
	if err != nil {
		t.Fatal(err)
	}
	reqBytes, err := proto.Marshal(&api.EnrollRequest{
		Jwt:                jwtToken,
		PeerId:             clientID.String(),
		PublicKey:          pubBytes,
		RequestedRole:      api.RoleNode,
		ChallengeUnixMs:    ts,
		ChallengeSignature: sig,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/register", cpPort),
		"application/octet-stream",
		bytes.NewReader(reqBytes),
	)
	if err != nil {
		t.Fatalf("failed to send enroll request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var enrollResp api.EnrollResponse
	if err := proto.Unmarshal(body, &enrollResp); err != nil {
		t.Fatalf("failed to decode enroll response: %v", err)
	}
	return &enrollResp
}
