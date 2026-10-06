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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// TestKeyRotationIntegration is the reauthentication CUJ: the control plane
// rotates its signing key, the retiring key leaves its grace period, and
// both the router and the node must by then hold credentials signed by a
// current key. Asserting only while the retiring key is still valid would
// pass with credentials nobody refreshed, which is the failure this pins.
func TestKeyRotationIntegration(t *testing.T) {
	cpBin := buildBinary(t, "./cmd/sam-control-plane")
	routerBin := buildBinary(t, "./cmd/sam-router")
	nodeBin := buildBinary(t, "./cmd/sam-node")

	tmpDir := t.TempDir()

	// 1. Create a mock policy file
	policyFile := filepath.Join(tmpDir, "policies.yaml")
	policyContent := `bindings:
  - members: ["user:mock-user"]
    role: admin
  - members: ["user:mock-user"]
    role: sam:role:node
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

	// 2. Start Control Plane with aggressive key rotation
	cpCmd := exec.Command(cpBin,
		"--bind-address", fmt.Sprintf("127.0.0.1:%d", cpPort),
		"--admin-token-path", tokenPath(t, "test-admin-token"),
		"--db-dsn", filepath.Join(tmpDir, "cp-keys.db"),
		"--issuer", oidcURL,
		"--key-rotation-interval", "2s",
		"--key-grace-period", "1s",
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

	// 3. Start Router with aggressive key sync interval
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
		"--lease-renew-interval", "1s",
		"--keys-sync-interval", "500ms",
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

	// 4. Start Node
	nodeJWT := mintToken(map[string]interface{}{
		"sub":   "mock-user",
		"roles": []string{api.RoleNode},
	})
	nodeApiPort := getFreePort(t)
	nodeHome := t.TempDir()

	nodeEnv := append(os.Environ(),
		"HOME="+nodeHome,
		"XDG_CONFIG_HOME="+filepath.Join(nodeHome, ".config"),
	)

	nodeLogPath := filepath.Join(tmpDir, "node.log")
	nodeLogFile, _ := os.Create(nodeLogPath)
	defer func() { _ = nodeLogFile.Close() }()

	// The owner socket serves /sam/identity, the node's own credential;
	// t.TempDir is too long for a socket path.
	socketDir, err := os.MkdirTemp("", "sam-rot-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(socketDir) }()
	nodeSocket := filepath.Join(socketDir, "node.sock")

	nodeCmd := exec.Command(nodeBin, "run",
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
		"--jwt", nodeJWT,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--allow-loopback",
		"--bind-addr", fmt.Sprintf("127.0.0.1:%d", nodeApiPort),
		"--socket-path", nodeSocket,
		"--api-token-path", tokenPath(t, "dummy-token"),
		"--control-plane-sync-interval", "500ms",
		"--log-level", "debug",
	)
	nodeCmd.Env = nodeEnv
	nodeCmd.Stdout = nodeLogFile
	nodeCmd.Stderr = nodeLogFile
	if err := nodeCmd.Start(); err != nil {
		t.Fatalf("failed to start node: %v", err)
	}
	defer func() {
		_ = nodeCmd.Process.Kill()
		_ = nodeCmd.Wait()
	}()

	// Wait for Node to be online actively
	waitForAPI(t, fmt.Sprintf("127.0.0.1:%d", nodeApiPort))
	socketClient := identityEvidenceSocketClient(nodeSocket)
	waitForIdentityEvidenceSocket(t, socketClient)

	// The key that signed the node's enrollment credential is the one whose
	// retirement matters: read the credential the way an owner application
	// would and find its signer among the control plane's live keys.
	var initial api.IdentityEvidenceResponse
	getIdentityEvidenceJSON(t, socketClient, "/sam/identity", &initial)
	nodePeer, err := peer.Decode(initial.PeerId)
	if err != nil {
		t.Fatalf("decode node PeerID from evidence: %v", err)
	}
	signer := signerOf(t, initial.Biscuit, nodePeer, fetchPublicKeys(t, cpPort))

	// Once the signer has left /keys, a credential it signed verifies
	// nowhere; both components must hold re-issued credentials by then.
	retiredAt := waitForKeysRetired(t, cpPort, [][]byte{signer})

	// 6. Verify Node is still active and can communicate (it should renew successfully and remain online)
	if nodeCmd.ProcessState != nil && nodeCmd.ProcessState.Exited() {
		t.Fatalf("node process exited unexpectedly")
	}

	var current api.IdentityEvidenceResponse
	getIdentityEvidenceJSON(t, socketClient, "/sam/identity", &current)
	if _, err := verifyBiscuitForApplication(current.Biscuit, nodePeer, publicKeysFrom(fetchPublicKeys(t, cpPort))); err != nil {
		t.Fatalf("node credential does not verify under the current control plane keys after its signer retired: %v\n--- node.log ---\n%s", err, readLog(t, nodeLogPath))
	}
	if _, err := verifyBiscuitForApplication(current.Biscuit, nodePeer, []ed25519.PublicKey{signer}); err == nil {
		t.Fatal("node credential still verifies under the retired key: it was never refreshed")
	}

	// The router renews its lease with its credential; a renewal accepted
	// after retirement proves the router's credential was re-issued too.
	waitForLeaseRenewedAfter(t, cpPort, retiredAt)

	// Double check by looking at the node's API or verifying it doesn't crash
	clientBin := buildBinary(t, "./cmd/mcp-client")
	stdout, stderr, err := runCommand(t, repoRoot(t), 5*time.Second, nil, "",
		clientBin,
		"-url", fmt.Sprintf("http://127.0.0.1:%d/mcp", nodeApiPort),
		"-token", "dummy-token",
		"-tool", "get_mesh_info",
		"-args", "{}",
	)
	if err != nil {
		t.Fatalf("failed to query node: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "dht_size") {
		t.Fatalf("unexpected node response: %s", stdout)
	}
}

func fetchPublicKeys(t *testing.T, cpPort int) [][]byte {
	t.Helper()
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/keys", cpPort))
	if err != nil {
		t.Fatalf("failed to get initial keys: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read keys body: %v", err)
	}
	var keysResp api.KeysResponse
	if err := proto.Unmarshal(body, &keysResp); err != nil {
		t.Fatalf("failed to unmarshal KeysResponse: %v", err)
	}
	return keysResp.PublicKeys
}

// signerOf returns the one key among candidates that verifies biscuit for
// peerID.
func signerOf(t *testing.T, biscuit []byte, peerID peer.ID, candidates [][]byte) ed25519.PublicKey {
	t.Helper()
	for _, candidate := range candidates {
		key := ed25519.PublicKey(candidate)
		if _, err := verifyBiscuitForApplication(biscuit, peerID, []ed25519.PublicKey{key}); err == nil {
			return key
		}
	}
	t.Fatalf("no live control plane key verifies the node's credential")
	return nil
}

// waitForKeysRetired returns once none of retiring is served by /keys any
// more, and when that was observed.
func waitForKeysRetired(t *testing.T, cpPort int, retiring [][]byte) time.Time {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		stillValid := false
		for _, key := range fetchPublicKeys(t, cpPort) {
			for _, old := range retiring {
				if bytes.Equal(key, old) {
					stillValid = true
				}
			}
		}
		if !stillValid {
			return time.Now()
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the retiring key to leave /keys")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForLeaseRenewedAfter returns once the control plane reports a router
// lease renewal later than after; the renewal carries the router's biscuit
// and is refused when that biscuit's key is no longer valid.
func waitForLeaseRenewedAfter(t *testing.T, cpPort int, after time.Time) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, lease := range fetchAdminStatus(t, cpPort, "test-admin-token").ActiveRouters {
			if lease.GetLastRenewalTime().AsTime().After(after) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no router lease renewed after %s: the router's credential was not re-issued", after.Format(time.RFC3339Nano))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func publicKeysFrom(raw [][]byte) []ed25519.PublicKey {
	keys := make([]ed25519.PublicKey, 0, len(raw))
	for _, key := range raw {
		keys = append(keys, ed25519.PublicKey(key))
	}
	return keys
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(no log: %v)", err)
	}
	return string(data)
}
