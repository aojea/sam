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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
)

// TestUngrantedServiceIsNeitherPublishedNorServed pins the publish grant end
// to end, from the policy to both verifiers. The node role grants
// mcp://granted and nothing else; a node declares that service and a second
// one, mcp://ungranted, both running. The router the node holds keeps the
// granted record and refuses the other, so no caller finds the second by
// name, and a caller that names the node by peer ID anyway is refused by its
// own node before the request leaves it. Then the policy grants the second
// service too, and nothing is restarted: the POLICY_UPDATE event brings the
// change to the router, the provider and the caller, the provider announces
// again, and the service is found and called.
func TestUngrantedServiceIsNeitherPublishedNorServed(t *testing.T) {
	m := startAttachmentMesh(t)

	grant := func(publish string) {
		t.Helper()
		policyFile := filepath.Join(m.tmpDir, "publish-policy.yaml")
		policy := fmt.Sprintf(`roles:
  - name: %s
    allowed_services: []
    allowed_targets: ["*"]
  - name: %s
    allowed_services: ["mcp://*"]
    allowed_publish: [%q]
    allowed_targets: ["*"]
bindings:
  - role: %s
    members: ["group:routers"]
  - role: %s
    members: ["user:mock-user"]
`, api.RoleRouter, api.RoleNode, publish, api.RoleRouter, api.RoleNode)
		if err := os.WriteFile(policyFile, []byte(policy), 0o644); err != nil {
			t.Fatal(err)
		}
		injectPolicyYAML(t, m.cpPort, testAdminToken, policyFile)
	}
	grant("mcp://granted")

	router := m.startRouter(t, "router")

	mcpServer := httptest.NewServer(newBoundaryMCPHandler(t))
	t.Cleanup(mcpServer.Close)

	// A node acts on a POLICY_UPDATE event after a random delay of up to a
	// tenth of its sync interval, so a fleet does not pull together; the
	// default 15 minutes would make that up to 90 seconds here.
	common := append(m.nodeArgs(), "--allow-loopback", "--control-plane-sync-interval", "5s")
	provider := launchNode(t, m.nodeBin, m.nodeEnv("provider"), filepath.Join(m.tmpDir, "provider"), append(slices.Clone(common),
		"--api-token-path", tokenPath(t, "token-provider"),
		"--config", writeNodeConfig(t, m.tmpDir, nil,
			svcDecl{Type: "mcp", Name: "granted", TargetURL: mcpServer.URL},
			svcDecl{Type: "mcp", Name: "ungranted", TargetURL: mcpServer.URL}),
	)...)
	caller := launchNode(t, m.nodeBin, m.nodeEnv("caller"), filepath.Join(m.tmpDir, "caller"), append(slices.Clone(common),
		"--api-token-path", tokenPath(t, "token-caller"),
	)...)
	provider.waitForAPI(t)
	caller.waitForAPI(t)

	// The granted service is found by name and answers.
	callAcrossMesh(t, caller, "token-caller", provider, "granted")

	// The router refused the other service's records, by name and by type,
	// and said so; the node logged the refusal it got back.
	denied := `agentmesh_router_directory_requests_total{op="provide",result="denied"}`
	deadline := time.Now().Add(10 * time.Second)
	for router.gauge(t, denied) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("router counted %v denied records, want 2 (mcp://ungranted by name and by type)\n--- provider ---\n%s", router.gauge(t, denied), provider.log())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if log := provider.log(); !strings.Contains(log, "credential does not grant serving mcp://ungranted") {
		t.Fatalf("the provider did not log the refusal\n--- provider ---\n%s", log)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	discover, _ := http.NewRequest(http.MethodGet, "http://"+caller.apiAddr+"/mesh/service/discover?type=mcp&name=ungranted", nil)
	discover.Header.Set(api.HeaderMeshAuthentication, "Bearer token-caller")
	resp, err := client.Do(discover)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), provider.peerID.String()) {
		t.Fatalf("mcp://ungranted is in the directory: %s", body)
	}

	// Named by peer ID, the caller's own node checks the provider's
	// credential and refuses before anything crosses the mesh.
	call, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/mesh/%s/mcp/ungranted", caller.apiAddr, provider.peerID),
		bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", "application/json, text/event-stream")
	call.Header.Set(api.HeaderMeshAuthentication, "Bearer token-caller")
	resp, err = client.Do(call)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "provider is not granted mcp://ungranted") {
		t.Fatalf("call to mcp://ungranted by peer ID: %d %s, want 403 naming the missing grant\n--- caller ---\n%s", resp.StatusCode, body, caller.log())
	}

	// The policy now grants every mcp service. No credential is minted
	// again: the grant travels in the policy text, the event brings it to
	// the router and both nodes, the provider announces again, and the
	// service is found and called.
	grant("mcp://*")
	deadline = time.Now().Add(10 * time.Second)
	for !strings.Contains(provider.log(), "Mesh policy changed; announcing this node's records again") {
		if time.Now().After(deadline) {
			t.Fatalf("the provider did not learn of the policy change\n--- provider ---\n%s", provider.log())
		}
		time.Sleep(200 * time.Millisecond)
	}
	callAcrossMesh(t, caller, "token-caller", provider, "ungranted")
}
