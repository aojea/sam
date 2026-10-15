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
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// oidcMember is a member enrolled with an OIDC token, driven over the
// control plane API alone: the test decides when it refreshes and with what.
type oidcMember struct {
	priv    crypto.PrivKey
	peerID  peer.ID
	sub     string
	biscuit []byte
}

// enrollOIDCMember enrolls a fresh key at POST /register with a token for sub.
func enrollOIDCMember(t *testing.T, cpPort int, mintToken func(map[string]interface{}) string, sub string) *oidcMember {
	t.Helper()
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	pID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	enrolled := registerOnControlPlane(t, cpPort, pID, priv, mintToken(map[string]interface{}{"sub": sub, "roles": []string{api.RoleNode}}))
	return &oidcMember{priv: priv, peerID: pID, sub: sub, biscuit: enrolled.BiscuitToken}
}

// refresh is one POST /refresh with the member's current credential and,
// when jwt is set, a fresh platform token that renews the session; on
// success the member holds the new credential. It returns status and body.
func (m *oidcMember) refresh(t *testing.T, cpURL, jwt string) (int, string) {
	t.Helper()
	ts := time.Now().UnixMilli()
	sig, err := m.priv.Sign(api.RefreshChallenge(m.peerID.String(), ts))
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&api.TokenRefreshRequest{ChallengeUnixMs: ts, ChallengeSignature: sig, Jwt: jwt})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, cpURL+"/refresh", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(m.biscuit))
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /refresh: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var refreshed api.TokenRefreshResponse
		if err := proto.Unmarshal(data, &refreshed); err != nil {
			t.Fatal(err)
		}
		m.biscuit = refreshed.BiscuitToken
	}
	return resp.StatusCode, string(data)
}

// bootstrapMember is a member enrolled with a bootstrap token, driven over
// the control plane API alone: the test decides when it refreshes.
type bootstrapMember struct {
	priv    crypto.PrivKey
	peerID  peer.ID
	biscuit []byte
}

// enrollBootstrapMember spends one use of token at POST /enroll.
func enrollBootstrapMember(t *testing.T, cpURL, token string) *bootstrapMember {
	t.Helper()
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	pID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.MarshalPublicKey(priv.GetPublic())
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UnixMilli()
	sig, err := priv.Sign(api.EnrollChallenge(pID.String(), ts))
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&api.BootstrapEnrollRequest{
		BootstrapToken: token, PeerId: pID.String(), PublicKey: pub, RequestedRole: api.RoleNode,
		ChallengeUnixMs: ts, ChallengeSignature: sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(cpURL+"/enroll", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /enroll: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /enroll: %d %s", resp.StatusCode, data)
	}
	var enrolled api.BootstrapEnrollResponse
	if err := proto.Unmarshal(data, &enrolled); err != nil {
		t.Fatal(err)
	}
	if enrolled.Status != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED || len(enrolled.BiscuitToken) == 0 {
		t.Fatalf("POST /enroll answered %s: %s", enrolled.Status, enrolled.ErrorMessage)
	}
	return &bootstrapMember{priv: priv, peerID: pID, biscuit: enrolled.BiscuitToken}
}

// refresh is one POST /refresh with the member's current credential; on
// success the member holds the new one. It returns the status and body.
func (m *bootstrapMember) refresh(t *testing.T, cpURL string) (int, string) {
	t.Helper()
	ts := time.Now().UnixMilli()
	sig, err := m.priv.Sign(api.RefreshChallenge(m.peerID.String(), ts))
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&api.TokenRefreshRequest{ChallengeUnixMs: ts, ChallengeSignature: sig})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, cpURL+"/refresh", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(m.biscuit))
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /refresh: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var refreshed api.TokenRefreshResponse
		if err := proto.Unmarshal(data, &refreshed); err != nil {
			t.Fatal(err)
		}
		m.biscuit = refreshed.BiscuitToken
	}
	return resp.StatusCode, string(data)
}

// listNodes is GET /user/nodes as the administrator, one page.
func listNodes(t *testing.T, cpURL, adminToken, query string) *api.NodeListResponse {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, cpURL+"/user/nodes?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET /user/nodes?%s: %v", query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /user/nodes?%s: %d %s", query, resp.StatusCode, data)
	}
	var page api.NodeListResponse
	if err := protojson.Unmarshal(data, &page); err != nil {
		t.Fatalf("GET /user/nodes?%s: %v", query, err)
	}
	return &page
}

// controlPlaneMetric reads one series from the control plane's /metrics.
func controlPlaneMetric(t *testing.T, cpURL, series string) (float64, bool) {
	t.Helper()
	resp, err := http.Get(cpURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, series+" ") {
			var v float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, series+" "), "%g", &v); err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v, true
		}
	}
	return 0, false
}

// TestLapsedMembersAreCollected is the testnet's nodes table in seconds. A
// probe CronJob enrolls a fresh identity every run and never comes back for
// it; such rows stayed forever, the table grew without bound, the metrics
// scrape read every row every 30 seconds and the control plane stalled on
// it. One member keeps its session alive by refreshing with a fresh token;
// sixteen enroll and leave.
func TestLapsedMembersAreCollected(t *testing.T) {
	tmpDir := t.TempDir()
	oidcURL, mintToken := startCustomMockOIDC(t)
	policyFile := filepath.Join(tmpDir, "policies.yaml")
	policy := fmt.Sprintf("roles: []\nbindings:\n  - role: %s\n    members: [%q]\n", api.RoleNode, api.SystemAuthenticated)
	if err := os.WriteFile(policyFile, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sessions of two seconds, kept for one more after they lapse; the GC
	// sweeps at a tenth of the retention, no less often than every second.
	cpPort, _ := startControlPlane(t, tmpDir, oidcURL, policyFile,
		"--oidc-session-ttl", "2s",
		"--node-retention", "1s",
	)
	cpURL := fmt.Sprintf("http://127.0.0.1:%d", cpPort)

	// Under the enrollment limiter's burst (controlplane.EnrollBurst), which
	// is a guard this test leaves in force.
	const leaving = 16
	stayer := enrollOIDCMember(t, cpPort, mintToken, "stayer")
	leavers := make([]*oidcMember, leaving)
	for i := range leavers {
		leavers[i] = enrollOIDCMember(t, cpPort, mintToken, fmt.Sprintf("leaver-%02d", i))
	}
	enrolledAt := time.Now()

	// The one that stays refreshes with a fresh token well within its
	// session; each such refresh renews the session.
	stop := make(chan struct{})
	stopRefreshing := sync.OnceFunc(func() { close(stop) })
	t.Cleanup(stopRefreshing)
	refreshed := make(chan error, 1)
	go func() {
		defer close(refreshed)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ts := time.Now().UnixMilli()
				sig, err := stayer.priv.Sign(api.RefreshChallenge(stayer.peerID.String(), ts))
				if err != nil {
					refreshed <- err
					return
				}
				jwt := mintToken(map[string]interface{}{"sub": stayer.sub, "roles": []string{api.RoleNode}})
				body, _ := proto.Marshal(&api.TokenRefreshRequest{ChallengeUnixMs: ts, ChallengeSignature: sig, Jwt: jwt})
				req, _ := http.NewRequest(http.MethodPost, cpURL+"/refresh", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(stayer.biscuit))
				req.Header.Set("Content-Type", "application/x-protobuf")
				resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
				if err != nil {
					refreshed <- err
					return
				}
				data, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					refreshed <- fmt.Errorf("refresh of the member that stays: %d %s", resp.StatusCode, data)
					return
				}
				var out api.TokenRefreshResponse
				if err := proto.Unmarshal(data, &out); err != nil {
					refreshed <- err
					return
				}
				stayer.biscuit = out.BiscuitToken
			}
		}
	}()

	// Every member is admitted; the status lists them all.
	if got := len(fetchAdminStatus(t, cpPort, testAdminToken).GetEnrolledNodes()); got != leaving+1 {
		t.Fatalf("admin status lists %d nodes right after enrollment, want %d", got, leaving+1)
	}

	// The leavers' sessions lapse. A lapsed member is refused at /refresh
	// and told what to do; it is out of the status and in the expired list,
	// which pages; and the metrics count it by state without listing it.
	time.Sleep(time.Until(enrolledAt.Add(2500 * time.Millisecond)))
	if status, body := leavers[0].refresh(t, cpURL, ""); status != http.StatusUnauthorized || !strings.Contains(body, "Session expired") {
		t.Fatalf("refresh of a lapsed member: %d %s, want 401 naming the lapsed session", status, strings.TrimSpace(body))
	}
	if nodes := fetchAdminStatus(t, cpPort, testAdminToken).GetEnrolledNodes(); len(nodes) != 1 || nodes[0].GetPeerId() != stayer.peerID.String() {
		t.Fatalf("admin status after the sessions lapsed lists %d nodes, want only the one that refreshes", len(nodes))
	}
	var expired []string
	pageToken := ""
	for pages := 0; ; pages++ {
		page := listNodes(t, cpURL, testAdminToken, "state=expired&page_size=5&page_token="+pageToken)
		for _, n := range page.GetNodes() {
			expired = append(expired, n.GetPeerId())
		}
		if page.GetNextPageToken() == "" {
			if pages != 3 {
				t.Errorf("%d expired nodes came in %d pages of 5, want 4", len(expired), pages+1)
			}
			break
		}
		pageToken = page.GetNextPageToken()
	}
	if len(expired) != leaving {
		t.Fatalf("expired list holds %d nodes, want the %d that left: %v", len(expired), leaving, expired)
	}
	for _, id := range expired {
		if id == stayer.peerID.String() {
			t.Fatalf("the member that refreshes is in the expired list")
		}
	}
	if v, ok := controlPlaneMetric(t, cpURL, `agentmesh_control_plane_enrolled_nodes{role="mesh:role:node",state="expired"}`); !ok || v != leaving {
		t.Errorf("enrolled_nodes{state=expired} = %v (present %v), want %d", v, ok, leaving)
	}
	if v, ok := controlPlaneMetric(t, cpURL, `agentmesh_control_plane_enrolled_nodes{role="mesh:role:node",state="admitted"}`); !ok || v != 1 {
		t.Errorf("enrolled_nodes{state=admitted} = %v (present %v), want 1", v, ok)
	}

	// The retention passes and the GC deletes the lapsed rows; the member
	// that refreshes is untouched.
	deadline := time.Now().Add(6 * time.Second)
	for {
		left := listNodes(t, cpURL, testAdminToken, "state=expired&page_size=200").GetNodes()
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d lapsed rows still in the table %s after they lapsed; want them deleted after the retention", len(left), time.Since(enrolledAt))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v, ok := controlPlaneMetric(t, cpURL, `agentmesh_control_plane_nodes_deleted_total{reason="session"}`); !ok || v != leaving {
		t.Errorf("nodes_deleted_total{reason=session} = %v (present %v), want %d", v, ok, leaving)
	}
	stopRefreshing()
	if err := <-refreshed; err != nil {
		t.Fatalf("the member that kept refreshing was refused: %v", err)
	}
	if status, body := stayer.refresh(t, cpURL, ""); status != http.StatusOK {
		t.Fatalf("the member that kept refreshing is refused after the sweep: %d %s", status, body)
	}
	if nodes := fetchAdminStatus(t, cpPort, testAdminToken).GetEnrolledNodes(); len(nodes) != 1 {
		t.Fatalf("admin status after the sweep lists %d nodes, want 1", len(nodes))
	}
}

// TestSilentEphemeralMembersAreForgotten is the ladder's fleet and the
// probe's identities in seconds. A bootstrap enrollment has no session, so
// a member that leaves used to stay enrolled forever; it is now forgotten
// once it has not refreshed for the retention, and a member an ephemeral
// token enrolled, which the ladder and the probes mint, within the
// ephemeral retention. A regular member that is silent for less than the
// retention, and one that refreshes, stay.
func TestSilentEphemeralMembersAreForgotten(t *testing.T) {
	tmpDir := t.TempDir()
	oidcURL, _ := startCustomMockOIDC(t)
	policyFile := filepath.Join(tmpDir, "policies.yaml")
	if err := os.WriteFile(policyFile, []byte("roles: []\nbindings: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ephemeral members are kept for a second after their last refresh,
	// regular ones for an hour; the GC sweeps every second for the shorter
	// of the two to matter.
	cpPort, _ := startControlPlane(t, tmpDir, oidcURL, policyFile,
		"--auto-approve-enrollment",
		"--ephemeral-node-retention", "1s",
		"--node-retention", "10s",
	)
	cpURL := fmt.Sprintf("http://127.0.0.1:%d", cpPort)

	const fleet = 12
	ephemeralToken := mintBootstrapTokenWithUses(t, cpURL, testAdminToken, fleet, true)
	regularToken := mintBootstrapTokenWithUses(t, cpURL, testAdminToken, 2, false)
	silent := enrollBootstrapMember(t, cpURL, regularToken)
	refreshing := enrollBootstrapMember(t, cpURL, regularToken)
	members := make([]*bootstrapMember, fleet)
	for i := range members {
		members[i] = enrollBootstrapMember(t, cpURL, ephemeralToken)
	}
	enrolledAt := time.Now()
	if got := len(fetchAdminStatus(t, cpPort, testAdminToken).GetEnrolledNodes()); got != fleet+2 {
		t.Fatalf("admin status lists %d nodes right after enrollment, want %d", got, fleet+2)
	}
	for _, n := range fetchAdminStatus(t, cpPort, testAdminToken).GetEnrolledNodes() {
		if n.GetEphemeral() != (n.GetPeerId() != silent.peerID.String() && n.GetPeerId() != refreshing.peerID.String()) {
			t.Fatalf("node %s ephemeral=%v, want the flag on the fleet's members only", n.GetPeerId(), n.GetEphemeral())
		}
	}

	// The fleet falls silent and is forgotten; the regular members stay,
	// the one that refreshes with its row's last refresh moving.
	if status, body := refreshing.refresh(t, cpURL); status != http.StatusOK {
		t.Fatalf("refresh: %d %s", status, body)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		nodes := fetchAdminStatus(t, cpPort, testAdminToken).GetEnrolledNodes()
		if len(nodes) == 2 {
			var left []string
			for _, n := range nodes {
				left = append(left, n.GetPeerId())
			}
			slices.Sort(left)
			want := []string{silent.peerID.String(), refreshing.peerID.String()}
			slices.Sort(want)
			if !slices.Equal(left, want) {
				t.Fatalf("after the sweep the status lists %v, want the two regular members %v", left, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d nodes still listed %s after the fleet enrolled; want the fleet forgotten after a second of silence", len(nodes), time.Since(enrolledAt))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v, ok := controlPlaneMetric(t, cpURL, `agentmesh_control_plane_nodes_deleted_total{reason="ephemeral"}`); !ok || v != fleet {
		t.Errorf("nodes_deleted_total{reason=ephemeral} = %v (present %v), want %d", v, ok, fleet)
	}
	if v, ok := controlPlaneMetric(t, cpURL, `agentmesh_control_plane_nodes_deleted_total{reason="silent"}`); !ok || v != 0 {
		t.Errorf("nodes_deleted_total{reason=silent} = %v (present %v), want 0: the regular members are within their retention", v, ok)
	}

	// A forgotten member is a stranger: its credential still verifies, its
	// row is gone, so it is told to enroll again. The regular ones refresh.
	if status, body := members[0].refresh(t, cpURL); status != http.StatusUnauthorized {
		t.Fatalf("refresh of a forgotten member: %d %s, want 401", status, strings.TrimSpace(body))
	}
	for _, m := range []*bootstrapMember{silent, refreshing} {
		if status, body := m.refresh(t, cpURL); status != http.StatusOK {
			t.Fatalf("refresh of a regular member after the sweep: %d %s", status, body)
		}
	}
}

// mintBootstrapTokenWithUses mints a node bootstrap token good for uses
// enrollments, ephemeral or not.
func mintBootstrapTokenWithUses(t *testing.T, cpURL, adminToken string, uses int, ephemeral bool) string {
	t.Helper()
	body, err := protojson.Marshal(&api.BootstrapTokenCreateRequest{Role: api.RoleNode, TtlHours: 1, MaxUsages: int32(uses), Description: "lifecycle", Ephemeral: ephemeral})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, cpURL+"/admin/bootstrap-tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /admin/bootstrap-tokens: %d %s", resp.StatusCode, data)
	}
	var created api.BootstrapTokenCreateResponse
	if err := protojson.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	return created.GetToken()
}
