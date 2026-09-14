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

package controlplane

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// TestReEnrollAfterExpiryMintsFreshBiscuit pins the fix for the bootstrap
// enrollment stale-biscuit-replay bug: a peer whose approved enrollment
// record outlives BiscuitTTL (e.g. a router restarting after several idle
// days) and re-submits /enroll or polls /enroll/status must get back a
// biscuit that actually verifies, not the original, now-expired one.
//
// Before the fix, both endpoints' "existing enrollment request" branch
// returned existingReq.BiscuitToken verbatim forever, so a router hitting a
// 401 on lease renewal and re-enrolling would receive the exact same expired
// token every time -- an unbreakable enroll -> reject -> re-enroll loop,
// identical to the one HandleRouterLease logs as "failed biscuit
// verification: ... $time <= $exp".
func TestReEnrollAfterExpiryMintsFreshBiscuit(t *testing.T) {
	issuer, _ := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer, func(o *Options) {
		o.BiscuitTTL = 1 * time.Second
	})
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()
	srv.config.AdminToken = "super-secret-admin-token"
	srv.config.AutoApproveEnrollment = true

	client := &http.Client{Timeout: 5 * time.Second}

	adminReqBody := []byte(`{"role": "sam:role:router", "ttl_hours": 2, "max_usages": 5, "description": "stale replay test"}`)
	req, err := http.NewRequest("POST", baseURL+"/admin/bootstrap-tokens", bytes.NewBuffer(adminReqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer super-secret-admin-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to create bootstrap token: %v", err)
	}
	var tokenDetails struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tokenDetails)
	_ = resp.Body.Close()
	if tokenDetails.Token == "" {
		t.Fatal("empty bootstrap token")
	}

	priv, pub, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	pID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubBytes, err := crypto.MarshalPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	doEnroll := func() *api.BootstrapEnrollResponse {
		t.Helper()
		ts, sig := enrollPoP(t, priv, pID.String())
		data, err := proto.Marshal(&api.BootstrapEnrollRequest{
			BootstrapToken:     tokenDetails.Token,
			PeerId:             pID.String(),
			PublicKey:          pubBytes,
			RequestedRole:      api.RoleRouter,
			Timestamp:          ts,
			ChallengeSignature: sig,
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post(baseURL+"/enroll", "application/x-protobuf", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("/enroll failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/enroll status %s: %s", resp.Status, string(body))
		}
		var out api.BootstrapEnrollResponse
		if err := proto.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshal BootstrapEnrollResponse: %v", err)
		}
		return &out
	}

	first := doEnroll()
	if first.Status != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
		t.Fatalf("expected APPROVED on first enroll, got %v", first.Status)
	}
	if len(first.BiscuitToken) == 0 {
		t.Fatal("first biscuit token is empty")
	}

	cpPubKey := ed25519.PublicKey(first.ControlPlanePublicKey)
	trustedKeys := []ed25519.PublicKey{cpPubKey}

	// The first token must actually verify right away.
	if _, _, err := identity.VerifyBiscuitAndGetKey(first.BiscuitToken, pID, trustedKeys, srv.config.BiscuitTimeout); err != nil {
		t.Fatalf("first biscuit failed to verify immediately after issuance: %v", err)
	}

	// Let it age past BiscuitTTL, exactly as an idle router's stored token
	// would after being off for longer than the control plane's TTL. Biscuit
	// Date terms round to whole seconds, so the margin has to clear that,
	// not just the nominal 1s TTL.
	time.Sleep(3 * time.Second)

	if _, _, err := identity.VerifyBiscuitAndGetKey(first.BiscuitToken, pID, trustedKeys, srv.config.BiscuitTimeout); err == nil {
		t.Fatal("first biscuit still verifies after its TTL elapsed; test setup is not exercising an expired token")
	}

	// This is the re-enrollment call a router's reEnroll() makes after a 401
	// on lease renewal: same peer ID, same bootstrap token, fresh PoP
	// signature. It must hit the "existing enrollment request" branch (not
	// mint a brand-new one from scratch) to actually exercise the bug.
	second := doEnroll()
	if second.Status != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
		t.Fatalf("expected APPROVED on re-enroll, got %v", second.Status)
	}
	if bytes.Equal(second.BiscuitToken, first.BiscuitToken) {
		t.Fatal("re-enroll after expiry returned the exact same (now-expired) biscuit token instead of minting a fresh one")
	}
	if _, _, err := identity.VerifyBiscuitAndGetKey(second.BiscuitToken, pID, trustedKeys, srv.config.BiscuitTimeout); err != nil {
		t.Fatalf("refreshed biscuit from re-enroll does not verify: %v", err)
	}

	// /enroll/status must agree: it must not resurrect the stale token either.
	statusResp, err := client.Do(signedEnrollStatusRequest(t, baseURL, priv, pID.String(), time.Now().UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	statusBody, _ := io.ReadAll(statusResp.Body)
	_ = statusResp.Body.Close()
	var polled api.BootstrapEnrollResponse
	if err := proto.Unmarshal(statusBody, &polled); err != nil {
		t.Fatalf("unmarshal polled BootstrapEnrollResponse: %v", err)
	}
	if _, _, err := identity.VerifyBiscuitAndGetKey(polled.BiscuitToken, pID, trustedKeys, srv.config.BiscuitTimeout); err != nil {
		t.Fatalf("biscuit returned by /enroll/status does not verify: %v", err)
	}

	// The enrolled node record must stay in lockstep with the refreshed
	// token: /refresh's reuse-detection compares a presented biscuit against
	// exactly this field, so a drift here would silently break that path.
	nodeRecord, err := store.GetNode(t.Context(), pID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nodeRecord.Biscuit, second.BiscuitToken) {
		t.Fatalf("enrolled node record's biscuit is out of sync with the refreshed enrollment token")
	}
}
