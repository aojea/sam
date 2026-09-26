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
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// TestEgressPolicyIsDistributedToServingNodes covers the admin-to-node path
// of an egress destination: the policy document is accepted and stored with
// its http and egress sections, GET /policies renders the serving grants,
// and GET /egress hands each node the destinations that select it, by role
// or by label, and nothing else.
func TestEgressPolicyIsDistributedToServingNodes(t *testing.T) {
	issuer, _ := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()
	const adminToken = "super-secret-admin-token"
	srv.config.AdminToken = adminToken
	srv.config.AutoApproveEnrollment = true
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	// Bootstrap tokens need the roles to exist before the policy below is
	// posted, and labels need a role that permits them.
	if err := store.SaveMeshPolicy(ctx, []*api.PolicyRole{
		{Name: "pep", AllowedLabels: []string{"*"}},
		{Name: api.RoleNode, AllowedLabels: []string{"*"}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	policy := `{
	  "roles": [
	    {"name": "pep", "allowed_services": ["egress://api.github.com"], "allowed_targets": ["*"], "allowed_labels": ["*"]},
	    {"name": "sam:role:node", "allowed_services": ["egress://mam.internal.example.com"], "allowed_targets": ["*"], "allowed_labels": ["*"],
	     "http": [{"service": "egress://mam.internal.example.com", "methods": ["GET"], "paths": ["/v2/public/*"]}]}
	  ],
	  "bindings": [{"role": "sam:role:node", "members": ["sam:system:authenticated"]}],
	  "egress": [
	    {"name": "api.github.com", "credential": "github-eu", "served_by": ["pep"]},
	    {"name": "mam.internal.example.com", "target_url": "http://mam.internal.example.com:8080", "served_by": ["site=dc1"]}
	  ]
	}`
	postPolicy := func(t *testing.T, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/policies", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /policies: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	if status, body := postPolicy(t, policy); status != http.StatusOK {
		t.Fatalf("POST /policies: %d %s", status, body)
	}

	// Stored and rendered back with both sections, so the console can post
	// what it shows.
	roles, _, err := store.GetMeshPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var nodeRole *api.PolicyRole
	for _, r := range roles {
		if r.Name == api.RoleNode {
			nodeRole = r
		}
	}
	if nodeRole == nil || len(nodeRole.Http) != 1 || nodeRole.Http[0].Service != "egress://mam.internal.example.com" || !slices.Equal(nodeRole.Http[0].Methods, []string{"GET"}) {
		t.Fatalf("http section was not stored: %v", nodeRole)
	}
	egress, err := store.GetEgressDestinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(egress) != 2 || egress[0].Name != "api.github.com" || egress[0].Credential != "github-eu" || !slices.Equal(egress[1].ServedBy, []string{"site=dc1"}) {
		t.Fatalf("egress section was not stored: %v", egress)
	}
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/admin/policy", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(rendered), `"egress"`) || !strings.Contains(string(rendered), `"http"`) {
		t.Fatalf("GET /admin/policy lacks the new sections: %s", rendered)
	}
	if status, body := postPolicy(t, string(rendered)); status != http.StatusOK {
		t.Fatalf("re-posting the rendered policy: %d %s", status, body)
	}

	// Two nodes: one holds the pep role, one is a plain node labelled
	// site=dc1. Each is selected by exactly one destination.
	enroll := func(t *testing.T, role string, labels map[string]string) []byte {
		t.Helper()
		token := createAdminBootstrapToken(t, baseURL, adminToken, role, 1)
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
		out := bootstrapEnroll(t, baseURL, token, priv, pID, pubBytes, role, labels)
		if out.Status != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
			t.Fatalf("enroll %s: status %v (%s)", role, out.Status, out.ErrorMessage)
		}
		return out.BiscuitToken
	}
	pepToken := enroll(t, "pep", nil)
	dc1Token := enroll(t, api.RoleNode, map[string]string{"site": "dc1"})

	getMesh := func(t *testing.T, path string, token []byte, out proto.Message) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, baseURL+path, nil)
		req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(token))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s %s", path, resp.Status, body)
		}
		if err := proto.Unmarshal(body, out); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
	}

	var rules api.PolicyConfigGetResponse
	getMesh(t, "/policies", pepToken, &rules)
	for _, want := range []string{
		`granted_service_exact("egress", "api.github.com") <- role("pep")`,
		`granted_service_exact("egress", "mam.internal.example.com") <- label("site", "dc1")`,
		`http_granted_service_exact("egress", "mam.internal.example.com") <- role("sam:role:node")`,
		`granted_method("egress", "mam.internal.example.com", ["GET"]) <- role("sam:role:node")`,
	} {
		if !slices.Contains(rules.DatalogRules, want) {
			t.Errorf("datalog_rules lack %q:\n%s", want, strings.Join(rules.DatalogRules, "\n"))
		}
	}
	// The response stays within the contract an older node checks.
	if len(rules.ProtoReflect().GetUnknown()) > 0 {
		t.Errorf("policy response carries unknown fields")
	}

	names := func(resp *api.EgressAssignmentsResponse) []string {
		var out []string
		for _, d := range resp.Egress {
			out = append(out, d.Name)
		}
		return out
	}
	var pepEgress, dc1Egress api.EgressAssignmentsResponse
	getMesh(t, "/egress", pepToken, &pepEgress)
	if got := names(&pepEgress); !slices.Equal(got, []string{"api.github.com"}) {
		t.Errorf("pep node assigned %v, want [api.github.com]", got)
	}
	if pepEgress.Egress[0].Credential != "github-eu" {
		t.Errorf("assignment lost its credential name: %v", pepEgress.Egress[0])
	}
	getMesh(t, "/egress", dc1Token, &dc1Egress)
	if got := names(&dc1Egress); !slices.Equal(got, []string{"mam.internal.example.com"}) {
		t.Errorf("dc1 node assigned %v, want [mam.internal.example.com]", got)
	}
	if dc1Egress.Egress[0].TargetUrl != "http://mam.internal.example.com:8080" {
		t.Errorf("assignment lost its target_url: %v", dc1Egress.Egress[0])
	}

	// Without a node credential there is nothing to select on.
	resp, err = client.Get(baseURL + "/egress")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET /egress: %s, want 401", resp.Status)
	}

	// A selector that matches no enrolled node is valid in form and is
	// reported, so a typo does not surface only as 404s at the callers.
	roles, bindings, err := store.GetMeshPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unserved, err := srv.unservedEgress(ctx, &api.PolicyConfig{Roles: roles, Bindings: bindings, Egress: []*api.EgressDestination{
		{Name: "api.github.com", ServedBy: []string{"pep"}},
		{Name: "mam.internal.example.com", ServedBy: []string{"site=dc1"}},
		{Name: "typo.example", ServedBy: []string{"site=dc-1"}},
		{Name: "nobody.example", ServedBy: []string{"contractor"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var unservedNames []string
	for _, d := range unserved {
		unservedNames = append(unservedNames, d.Name)
	}
	if want := []string{"typo.example", "nobody.example"}; !slices.Equal(unservedNames, want) {
		t.Errorf("unserved = %v, want %v", unservedNames, want)
	}

	// Validation names the mistake.
	for _, tc := range []struct{ name, body, want string }{
		{"unknown served_by", `{"roles":[{"name":"pep"}],"egress":[{"name":"x.example","served_by":["ghost"]}]}`, "neither a role"},
		{"credential is a path", `{"roles":[{"name":"pep"}],"egress":[{"name":"x.example","credential":"/etc/x","served_by":["pep"]}]}`, "not a path"},
		{"duplicate destination", `{"roles":[{"name":"pep"}],"egress":[{"name":"x.example","served_by":["pep"]},{"name":"x.example","served_by":["pep"]}]}`, "duplicate egress"},
		{"http narrows a service the role lacks", `{"roles":[{"name":"pep","allowed_services":["mcp://a"],"http":[{"service":"mcp://b","methods":["GET"]}]}]}`, "does not name one of the role's allowed_services"},
		{"http narrows nothing", `{"roles":[{"name":"pep","allowed_services":["mcp://a"],"http":[{"service":"mcp://a"}]}]}`, "narrows nothing"},
		{"a URL where a hostname belongs", `{"roles":[{"name":"pep","allowed_services":["egress://api.github.com/v3"]}]}`, "without a scheme, a port or a path"},
		{"uppercase egress grant", `{"roles":[{"name":"pep","allowed_services":["egress://API.github.com"]}]}`, "lowercase"},
	} {
		status, body := postPolicy(t, tc.body)
		if status != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("%s: got %d %q, want 400 mentioning %q", tc.name, status, body, tc.want)
		}
	}
}
