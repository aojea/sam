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

package node

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/agentmesh/api"
)

// TestSyncRecoversWhenControlPlaneForgotNode is the stateless control plane
// case: the signing key is unchanged, so this node's Biscuit still verifies
// everywhere, but the control plane lost its enrollment record and answers
// 401 to the node's own calls. A node with a bootstrap token file enrolls
// again at once and the same sync pass succeeds; without one it says so and
// leaves recovery to the renewal loop.
func TestSyncRecoversWhenControlPlaneForgotNode(t *testing.T) {
	newHarness := func(t *testing.T) (*refreshHarness, *atomic.Bool) {
		t.Helper()
		var known atomic.Bool // the control plane has this node's record
		harness := newRefreshHarness(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unexpected refresh", http.StatusInternalServerError)
		})
		mux := http.NewServeMux()
		mux.HandleFunc("/keys", keysHandler(t, []ed25519.PublicKey{harness.cpPub}, []ed25519.PrivateKey{harness.cpPriv}))
		mux.HandleFunc("/info", protoHandler(t, &api.ControlPlaneInfoResponse{}))
		mux.HandleFunc("/policies", func(w http.ResponseWriter, r *http.Request) {
			if !known.Load() {
				http.Error(w, "Unauthorized: node credential required", http.StatusUnauthorized)
				return
			}
			protoHandler(t, &api.PolicyConfigGetResponse{})(w, r)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		if err := harness.node.Store.SaveControlPlaneURL(server.URL); err != nil {
			t.Fatal(err)
		}
		return harness, &known
	}

	t.Run("re-enrolls with the bootstrap token", func(t *testing.T) {
		harness, known := newHarness(t)
		tokenPath := filepath.Join(t.TempDir(), "join-token")
		if err := os.WriteFile(tokenPath, []byte("mesh_tok_test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		harness.node.config.BootstrapTokenPath = tokenPath
		var enrolledWith string
		harness.node.enrollBootstrapFn = func(ctx context.Context, controlPlaneURL, token string) error {
			enrolledWith = token
			known.Store(true)
			return nil
		}

		err := harness.node.syncControlPlaneOnce(context.Background())
		if !harness.node.controlPlaneForgotUs(err) {
			t.Fatalf("sync error = %v, want a 401 the node recognizes as a forgotten enrollment", err)
		}
		// One SyncControlPlane recovers and completes the pull.
		if err := harness.node.SyncControlPlane(context.Background()); err != nil {
			t.Fatalf("sync with recovery: %v", err)
		}
		if enrolledWith != "mesh_tok_test" {
			t.Fatalf("enrolled with %q, want the trimmed token from the file", enrolledWith)
		}
	})

	t.Run("without a token path it waits for the renewal loop", func(t *testing.T) {
		harness, _ := newHarness(t)
		called := false
		harness.node.enrollBootstrapFn = func(context.Context, string, string) error { called = true; return nil }
		err := harness.node.SyncControlPlane(context.Background())
		if !harness.node.controlPlaneForgotUs(err) {
			t.Fatalf("sync error = %v, want the 401 passed through when there is no token to recover with", err)
		}
		if called {
			t.Fatal("enrollment attempted without a token")
		}
	})

	t.Run("other failures are not a forgotten enrollment", func(t *testing.T) {
		harness, _ := newHarness(t)
		if err := harness.node.Store.SaveControlPlaneURL("http://127.0.0.1:1"); err != nil {
			t.Fatal(err)
		}
		err := harness.node.SyncControlPlane(context.Background())
		if err == nil {
			t.Fatal("sync against a closed port succeeded")
		}
		if harness.node.controlPlaneForgotUs(err) {
			t.Fatalf("a connection failure (%v) was taken for a 401", err)
		}
	})
}
