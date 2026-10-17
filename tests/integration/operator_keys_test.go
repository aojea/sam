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
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	cpclient "github.com/google/agentmesh/internal/controlplane/client"
	"github.com/google/agentmesh/internal/identity"
	"github.com/google/agentmesh/internal/node"
	"github.com/google/agentmesh/internal/standalone"
	"github.com/libp2p/go-libp2p/core/crypto"
	"google.golang.org/protobuf/proto"
)

// TestOperatorSigningKeyRotationProcess runs the rotation an operator
// performs on a control plane whose signing keys come from a file they own
// (--signing-key-path), end to end against a stock node:
//
//  1. live rotation: the operator publishes a new file with the new key
//     first and the previous one kept; the running control plane picks it
//     up, publishes both on /keys, signs with the new one, and the node
//     refreshes to a credential under the new key without a restart;
//  2. a control plane that restarts with an empty store (agentmesh-one on
//     Cloud Run without a database) comes back on the same keys, the node's
//     credential still verifies, and the node, which the control plane no
//     longer knows, enrolls again with its bootstrap token on its own;
//  3. the operator drops the previous key: a credential under it is refused,
//     the node already holds one under the new key and keeps working.
func TestOperatorSigningKeyRotationProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const joinToken = "mesh_tok_operator_keys_test"
	const adminToken = "mesh_adm_operator_keys_test"
	port := getFreePort(t)
	bind := fmt.Sprintf("127.0.0.1:%d", port)
	controlPlaneURL := "http://" + bind

	keyFile := filepath.Join(t.TempDir(), "signing-keys.pem")
	v1, v2 := newOperatorEd25519(t), newOperatorEd25519(t)
	es256 := newOperatorP256(t)
	writeOperatorKeyFile(t, keyFile, es256, v1)

	// The operator's grace period is the overlap during which a credential
	// under the previous key still verifies.
	startHub := func(t *testing.T, dataDir string) *standalone.Server {
		t.Helper()
		srv, err := standalone.New(standalone.Options{
			BindAddress: bind,
			DataDir:     dataDir,
			JoinToken:   joinToken,
			AdminToken:  adminToken,
			ControlPlane: standalone.ControlPlaneTunables{
				SigningKeyPath: keyFile,
				KeyGracePeriod: time.Hour,
			},
		})
		if err != nil {
			t.Fatalf("standalone.New: %v", err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("standalone.Start: %v", err)
		}
		waitForActiveRouters(t, port, 1, 10*time.Second)
		return srv
	}
	hub := startHub(t, t.TempDir())

	// A stock node with the bootstrap token in a file, as a Kubernetes
	// Secret or a Cloud Run secret mounts it, and a short sync so the test
	// sees it act.
	tokenPath := filepath.Join(t.TempDir(), "join-token")
	if err := os.WriteFile(tokenPath, []byte(joinToken), 0o600); err != nil {
		t.Fatal(err)
	}
	meshNode := newOperatorKeysTestNode(t, ctx, tokenPath)
	if err := meshNode.EnrollBootstrap(ctx, controlPlaneURL, joinToken); err != nil {
		t.Fatalf("EnrollBootstrap: %v", err)
	}
	// The CLI records the control plane the node joined; the sync and the
	// recovery read it from there.
	if err := meshNode.Store.SaveControlPlaneURL(controlPlaneURL); err != nil {
		t.Fatalf("SaveControlPlaneURL: %v", err)
	}
	credV1 := append([]byte(nil), meshNode.GetIdentity()...)
	assertKeysOnWire(t, controlPlaneURL, pubOf(v1))
	if err := identity.VerifyBiscuitRole(credV1, pubOf(v1), api.RoleNode, 5*time.Second); err != nil {
		t.Fatalf("credential issued under v1 does not verify under v1: %v", err)
	}

	t.Run("live rotation without a restart", func(t *testing.T) {
		writeOperatorKeyFile(t, keyFile, es256, v2, v1)
		// The control plane re-reads the file on its own timer; the test is
		// time-bounded, so ask for the pass directly.
		hub.ReloadOperatorSigningKeys()
		assertKeysOnWire(t, controlPlaneURL, pubOf(v1), pubOf(v2))

		// The node learns the new key and refreshes to a credential under it;
		// the credential it held under v1 keeps verifying meanwhile.
		if err := meshNode.SyncControlPlane(ctx); err != nil {
			t.Fatalf("sync after rotation: %v", err)
		}
		credV2 := meshNode.GetIdentity()
		if err := identity.VerifyBiscuitRole(credV2, pubOf(v2), api.RoleNode, 5*time.Second); err != nil {
			t.Fatalf("node's credential after the rotation does not verify under v2: %v", err)
		}
		if err := identity.VerifyBiscuitRole(credV1, pubOf(v1), api.RoleNode, 5*time.Second); err != nil {
			t.Fatalf("credential under the previous key stopped verifying during the overlap: %v", err)
		}
	})

	t.Run("restart with an empty store keeps the keys", func(t *testing.T) {
		if err := hub.Close(); err != nil {
			t.Fatalf("hub.Close: %v", err)
		}
		hub = startHub(t, t.TempDir())
		assertKeysOnWire(t, controlPlaneURL, pubOf(v1), pubOf(v2))

		// The control plane does not know the node any more; the node's
		// sync answers the 401 by enrolling again with the token it holds
		// and completes the pull, which shows as a new credential.
		before := append([]byte(nil), meshNode.GetIdentity()...)
		if err := meshNode.SyncControlPlane(ctx); err != nil {
			t.Fatalf("sync against the fresh store did not recover: %v", err)
		}
		after := meshNode.GetIdentity()
		if string(after) == string(before) {
			t.Fatal("node kept its credential; the control plane cannot know it without a new enrollment")
		}
		if err := identity.VerifyBiscuitRole(after, pubOf(v2), api.RoleNode, 5*time.Second); err != nil {
			t.Fatalf("re-enrolled credential does not verify under v2: %v", err)
		}
	})

	t.Run("dropping the previous key refuses what it signed", func(t *testing.T) {
		writeOperatorKeyFile(t, keyFile, es256, v2)
		if err := hub.Close(); err != nil {
			t.Fatalf("hub.Close: %v", err)
		}
		hub = startHub(t, t.TempDir())
		t.Cleanup(func() { _ = hub.Close() })
		assertKeysOnWire(t, controlPlaneURL, pubOf(v2))
		if err := identity.VerifyBiscuitRole(credV1, pubOf(v2), api.RoleNode, 5*time.Second); err == nil {
			t.Fatal("a credential under the dropped key still verifies under the only published key")
		}
		// The node's credential, issued under v2 by the previous recovery,
		// still verifies; that recovery was moments ago, so the control
		// plane's 401 on this fresh store is reported, not answered with
		// another spend of the bootstrap token until the cooldown passes.
		if err := identity.VerifyBiscuitRole(meshNode.GetIdentity(), pubOf(v2), api.RoleNode, 5*time.Second); err != nil {
			t.Fatalf("node's credential after the drop does not verify under v2: %v", err)
		}
		var se *cpclient.StatusError
		if err := meshNode.SyncControlPlane(ctx); !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
			t.Fatalf("sync within the re-enrollment cooldown = %v, want the control plane's 401 reported", err)
		}
	})
}

func newOperatorEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return priv
}

func newOperatorP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	return priv
}

func pubOf(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// writeOperatorKeyFile writes keys as PKCS#8 PEM blocks in order; for each
// kind the first is the signing key, the way an operator publishes a
// rotation. The ES256 key is listed first here only so the Ed25519 order
// reads left to right.
func writeOperatorKeyFile(t *testing.T, path string, es256 *ecdsa.PrivateKey, keys ...ed25519.PrivateKey) {
	t.Helper()
	var data []byte
	der, err := x509.MarshalPKCS8PrivateKey(es256)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey (P-256): %v", err)
	}
	data = append(data, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})...)
	for _, k := range keys {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
		}
		data = append(data, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})...)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// assertKeysOnWire checks that /keys publishes exactly want.
func assertKeysOnWire(t *testing.T, controlPlaneURL string, want ...ed25519.PublicKey) {
	t.Helper()
	resp, err := http.Get(controlPlaneURL + "/keys")
	if err != nil {
		t.Fatalf("GET /keys: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read /keys: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /keys = %d: %s", resp.StatusCode, body)
	}
	var keys api.KeysResponse
	if err := proto.Unmarshal(body, &keys); err != nil {
		t.Fatalf("Unmarshal KeysResponse: %v", err)
	}
	got := map[string]bool{}
	for _, k := range keys.PublicKeys {
		got[string(k)] = true
	}
	if len(got) != len(want) {
		t.Fatalf("/keys publishes %d keys, want %d", len(got), len(want))
	}
	for _, w := range want {
		if !got[string(w)] {
			t.Fatalf("/keys is missing a key the operator's file holds")
		}
	}
}

func newOperatorKeysTestNode(t *testing.T, ctx context.Context, bootstrapTokenPath string) *node.AgentMeshNode {
	t.Helper()
	nodeStore, err := node.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("node.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = nodeStore.Close() })
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	// The CLI persists the node key; a credential refresh signs with it.
	privBytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if err := nodeStore.SaveKey(privBytes); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	meshNode, err := node.NewAgentMeshNode(node.Options{
		PrivKey:            priv,
		Store:              nodeStore,
		ListenAddrs:        []string{"/ip4/127.0.0.1/tcp/0"},
		AllowLoopback:      true,
		RequiredRole:       api.RoleNode,
		BootstrapTokenPath: bootstrapTokenPath,
	})
	if err != nil {
		t.Fatalf("NewAgentMeshNode: %v", err)
	}
	if err := meshNode.Start(ctx); err != nil {
		t.Fatalf("node.Start: %v", err)
	}
	t.Cleanup(func() {
		if meshNode.Host != nil {
			_ = meshNode.Host.Close()
		}
	})
	return meshNode
}
