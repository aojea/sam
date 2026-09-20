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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/node"
	"github.com/libp2p/go-libp2p/core/peer"
)

// These tests cover how a node comes to hold, keep and lose its mesh
// identity. They watch the two places that identity lives: the control
// plane's enrollment records and the node's own store. What the node prints
// along the way is not asserted on.

const testAdminToken = "test-admin-token"

// jwtUser is the subject of tokens handed to a node directly with --jwt.
const jwtUser = "mock-user"

// meshPolicyFile writes the policy these tests run under: the mock
// provider's browser user and jwtUser are seated as nodes.
func meshPolicyFile(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := fmt.Sprintf("bindings:\n  - role: %s\n    members: [%q, %q]\nroles: []\n",
		api.RoleNode, "user:"+mockOIDCUser, "user:"+jwtUser)
	policyFile := filepath.Join(dir, "policies.yaml")
	if err := os.WriteFile(policyFile, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	return policyFile
}

// startMesh starts a control plane and a router under dir, with the policy
// from meshPolicyFile. It returns the control plane's port and URL.
func startMesh(t *testing.T, dir, oidcURL string, mintToken func(map[string]interface{}) string) (int, string) {
	t.Helper()
	cpPort, cleanup := startControlPlaneAndRouter(t, dir, oidcURL, mintToken, meshPolicyFile(t, dir))
	t.Cleanup(cleanup)
	return cpPort, fmt.Sprintf("http://127.0.0.1:%d", cpPort)
}

// nodeHome is the environment of a node whose files live under home, and
// the store that environment resolves to.
func nodeHome(home string) ([]string, string) {
	env := append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	return env, nodeDataDir(env, nil)
}

// storedMembership is what the node keeps of its enrollment: the biscuit
// and the control plane that issued it, both empty when there is none. The
// store is the node's while it runs, so this is for a node that has stopped.
func storedMembership(t *testing.T, dataDir string) ([]byte, string) {
	t.Helper()
	store, err := node.NewStore(dataDir)
	if err != nil {
		t.Fatalf("open node store: %v", err)
	}
	defer func() { _ = store.Close() }()
	identity, _ := store.LoadIdentity()
	cpURL, err := store.LoadControlPlaneURL()
	if err != nil {
		t.Fatalf("load control plane URL: %v", err)
	}
	return identity, cpURL
}

// join runs `sam-node join` for the node living under env, with the mock
// provider playing the user who signs in.
func join(t *testing.T, nodeBin string, env []string, cpURL string, extra ...string) {
	t.Helper()
	args := append([]string{"join"}, extra...)
	args = append(args, cpURL)
	stdout, stderr, err := runCommandWithCallback(t, repoRoot(t), 15*time.Second, env, "", nodeBin, args...)
	if err != nil {
		t.Fatalf("sam-node %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
}

// enrolledAs fails unless the control plane holds an enrollment for id made
// by subject, and returns it.
func enrolledAs(t *testing.T, cpPort int, id peer.ID, subject string) []byte {
	t.Helper()
	record := fetchAdminStatus(t, cpPort, testAdminToken).enrolledNode(id)
	if record == nil {
		t.Fatalf("control plane :%d has no enrollment for %s", cpPort, id)
	}
	if record.Role != api.RoleNode {
		t.Fatalf("enrollment role = %q, want %q", record.Role, api.RoleNode)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal([]byte(record.ClaimsJSON), &claims); err != nil {
		t.Fatalf("enrollment claims %q: %v", record.ClaimsJSON, err)
	}
	if claims.Sub != subject {
		t.Fatalf("enrolled by %q, want %q", claims.Sub, subject)
	}
	return record.Biscuit
}

func TestSamNodeJoin(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpDir := t.TempDir()
	oidcURL, mintToken := startCustomMockOIDC(t)
	cpPort, cpURL := startMesh(t, filepath.Join(tmpDir, "mesh"), oidcURL, mintToken)

	home := filepath.Join(tmpDir, "home")
	env, dataDir := nodeHome(home)
	peerID := ensureNodeKey(t, dataDir)

	join(t, nodeBin, env, cpURL)

	// The control plane enrolled this node for the user who signed in, and
	// the node holds the identity it was issued for that mesh.
	issued := enrolledAs(t, cpPort, peerID, mockOIDCUser)
	identity, storedURL := storedMembership(t, dataDir)
	if !bytes.Equal(identity, issued) {
		t.Fatal("the node's stored identity is not the biscuit the control plane issued")
	}
	if storedURL != cpURL {
		t.Fatalf("stored control plane = %q, want %q", storedURL, cpURL)
	}

	// From here on the node needs neither a login nor a --control-plane: it
	// comes up on what it stored and the router admits it.
	n := launchNode(t, nodeBin, env, home, "run", "--allow-loopback", "--api-token-path", tokenPath(t, "node-token"))
	n.waitForAPI(t)
	waitForPeerOnRouter(t, cpPort, testAdminToken, peerID, 10*time.Second)
}

// TestSamNodeJoinOfflineAccessSavesRefreshToken guards against a real bug:
// interactiveJoin used a *node.SamNode with no Store, so InteractiveLogin's
// "if n.Store != nil" guard silently skipped persisting the refresh token,
// breaking --offline-access for both "join" and "run --join".
func TestSamNodeJoinOfflineAccessSavesRefreshToken(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpDir := t.TempDir()
	oidcURL, mintToken := startCustomMockOIDC(t)
	cpPort, cpURL := startMesh(t, filepath.Join(tmpDir, "mesh"), oidcURL, mintToken)

	home := filepath.Join(tmpDir, "home")
	env, dataDir := nodeHome(home)
	peerID := ensureNodeKey(t, dataDir)

	join(t, nodeBin, env, cpURL, "--offline-access")
	enrolledAs(t, cpPort, peerID, mockOIDCUser)

	store, err := node.NewStore(dataDir)
	if err != nil {
		t.Fatalf("open node store: %v", err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.LoadRefreshToken(); err != nil {
		t.Fatalf("no refresh token saved after join --offline-access: %v", err)
	}
	issuer, clientID, _, err := store.LoadOIDCConfig()
	if err != nil {
		t.Fatal(err)
	}
	if issuer != oidcURL || clientID == "" {
		t.Fatalf("stored OIDC config (%q, %q) is not what the control plane advertised (%q)", issuer, clientID, oidcURL)
	}
}

// TestSamNodeRunWithoutIdentityServesEnrollment covers a node that has never
// enrolled: it must not fail, nor block on a login it cannot complete
// without a terminal, but serve the enrollment-only MCP surface until
// someone enrolls it.
func TestSamNodeRunWithoutIdentityServesEnrollment(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plain run", nil},
		// --join without a TTY has nobody to log in; the node falls back
		// rather than hanging on a prompt. The control plane is never
		// contacted, so any address will do.
		{"run --join without a TTY", []string{"--join", "--control-plane", fmt.Sprintf("http://127.0.0.1:%d", getFreePort(t))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env, dataDir := nodeHome(home)
			n := launchNode(t, nodeBin, env, home, append([]string{"run"}, tc.args...)...)
			n.waitForEnrollmentSidecar(t)
			n.staysUp(t, time.Second)
			n.kill()
			if identity, _ := storedMembership(t, dataDir); len(identity) != 0 {
				t.Fatal("a node that never enrolled holds an identity")
			}
		})
	}
}

// TestSamNodeRunWithStoredIdentity covers the everyday restart: a node that
// enrolled once comes back with no token and no --control-plane, on what it
// stored, and the router admits it again.
func TestSamNodeRunWithStoredIdentity(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpDir := t.TempDir()
	oidcURL, mintToken := startCustomMockOIDC(t)
	cpPort, cpURL := startMesh(t, filepath.Join(tmpDir, "mesh"), oidcURL, mintToken)

	home := filepath.Join(tmpDir, "home")
	env, dataDir := nodeHome(home)
	token := tokenPath(t, "node-token")

	first := launchNode(t, nodeBin, env, home, "run",
		"--control-plane", cpURL,
		"--jwt", mintToken(map[string]interface{}{"sub": jwtUser}),
		"--allow-loopback",
		"--api-token-path", token,
	)
	first.waitForAPI(t)
	issued := enrolledAs(t, cpPort, first.peerID, jwtUser)
	first.kill()

	// /healthz answers only once Start succeeded, and Start fails unless a
	// router authenticated the node: coming up is the proof.
	again := launchNode(t, nodeBin, env, home, "run", "--allow-loopback", "--api-token-path", token)
	again.waitForAPI(t)
	if again.peerID != first.peerID {
		t.Fatalf("peer ID changed across restarts: %s then %s", first.peerID, again.peerID)
	}
	again.kill()
	identity, _ := storedMembership(t, dataDir)
	if !bytes.Equal(identity, issued) {
		t.Fatal("restart replaced the stored identity instead of using it")
	}
}

// TestSamNodeRunControlPlaneMismatch covers a node enrolled with one mesh
// being pointed at another: it must refuse, with or without --join, keeping
// its identity for the first mesh and leaving no trace on the second, and
// "reset" must clear the way for a fresh enrollment there.
func TestSamNodeRunControlPlaneMismatch(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpDir := t.TempDir()
	oidcURL, mintToken := startCustomMockOIDC(t)
	cpPortA, cpURLA := startMesh(t, filepath.Join(tmpDir, "meshA"), oidcURL, mintToken)
	cpPortB, cpURLB := startMesh(t, filepath.Join(tmpDir, "meshB"), oidcURL, mintToken)

	home := filepath.Join(tmpDir, "home")
	env, dataDir := nodeHome(home)
	token := tokenPath(t, "node-token")
	jwt := mintToken(map[string]interface{}{"sub": jwtUser})

	// Enroll with mesh A.
	onA := launchNode(t, nodeBin, env, home, "run", "--control-plane", cpURLA, "--jwt", jwt, "--allow-loopback", "--api-token-path", token)
	onA.waitForAPI(t)
	peerID := onA.peerID
	issuedByA := enrolledAs(t, cpPortA, peerID, jwtUser)
	onA.kill()

	// Pointed at B, with or without --join (no TTY to confirm a switch),
	// the node refuses rather than reusing A's identity or enrolling anew.
	for _, args := range [][]string{
		{"run", "--control-plane", cpURLB},
		{"run", "--join", "--control-plane", cpURLB},
	} {
		n := launchNode(t, nodeBin, env, home, append(args, "--allow-loopback", "--api-token-path", token)...)
		if err := n.exitsWithin(t, 10*time.Second); err == nil {
			t.Fatalf("sam-node %s exited cleanly against a mesh it is not enrolled with", strings.Join(args, " "))
		}
		identity, storedURL := storedMembership(t, dataDir)
		if !bytes.Equal(identity, issuedByA) || storedURL != cpURLA {
			t.Fatalf("sam-node %s changed the stored membership (control plane now %q)", strings.Join(args, " "), storedURL)
		}
		if fetchAdminStatus(t, cpPortB, testAdminToken).enrolledNode(peerID) != nil {
			t.Fatalf("sam-node %s enrolled with mesh B", strings.Join(args, " "))
		}
	}

	// "reset" forgets the membership and nothing else: the key, hence the
	// peer ID, stays.
	stdout, stderr, err := runCommand(t, repoRoot(t), 5*time.Second, env, "", nodeBin, "reset")
	if err != nil {
		t.Fatalf("sam-node reset: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if identity, storedURL := storedMembership(t, dataDir); len(identity) != 0 || storedURL != "" {
		t.Fatalf("reset left a membership behind (control plane %q)", storedURL)
	}

	onB := launchNode(t, nodeBin, env, home, "run", "--control-plane", cpURLB, "--jwt", jwt, "--allow-loopback", "--api-token-path", token)
	onB.waitForAPI(t)
	if onB.peerID != peerID {
		t.Fatalf("reset changed the peer ID: %s then %s", peerID, onB.peerID)
	}
	enrolledAs(t, cpPortB, peerID, jwtUser)
	onB.kill()
	if _, storedURL := storedMembership(t, dataDir); storedURL != cpURLB {
		t.Fatalf("stored control plane = %q, want %q", storedURL, cpURLB)
	}
}
