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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callMCPAllowError(t *testing.T, mcpAddr string, apiToken string, toolName string, params map[string]any) (string, error) {
	t.Helper()
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "0.1.0",
	}, nil)

	transport := http.DefaultTransport
	clientTransport := &mcp.StreamableClientTransport{
		Endpoint: "http://" + mcpAddr + "/mcp",
		HTTPClient: &http.Client{
			Transport: &authRoundTripper{
				token: apiToken,
				rt:    transport,
			},
		},
	}

	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return "", fmt.Errorf("failed to connect: %v", err)
	}
	defer func() {
		_ = session.Close()
	}()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: params,
	})
	if err != nil {
		return "", err
	}

	if res.IsError {
		var errText []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				errText = append(errText, tc.Text)
			}
		}
		return "", fmt.Errorf("MCP error: %s", strings.Join(errText, " "))
	}

	if len(res.Content) > 0 {
		if textContent, ok := res.Content[0].(*mcp.TextContent); ok {
			return textContent.Text, nil
		}
	}
	return "", nil
}

func TestPolicyPermutations(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")

	tmpDir := t.TempDir()

	oidcURL, mintToken := startCustomMockOIDC(t)

	// 1. Control plane policies
	controlPlanePolicyFile := filepath.Join(tmpDir, "policies.yaml")
	controlPlanePolicyYAML := `roles:
  - name: role-user
    allowed_services: ["mcp://test-user"]
    allowed_targets: ["user:bob-subject"]
  - name: role-email
    allowed_services: ["mcp://test-email"]
    allowed_targets: ["email:nodeB@example.com"]
  - name: role-group
    allowed_services: ["mcp://test-group"]
    allowed_targets: ["group:compute"]
  - name: role-node
    allowed_services: ["mcp://test-node"]
    allowed_targets: ["group:backend"]
  - name: role-direct
    allowed_services: ["mcp://test-role"]
    allowed_targets: ["group:compute"]
  # Same shape as role-direct but with no idp_role binding: an issuer's
  # roles claim naming it must grant nothing.
  - name: role-unbound
    allowed_services: ["mcp://test-unbound"]
    allowed_targets: ["group:compute"]
  - name: admin
    allowed_services: ["*"]
    allowed_targets: ["*:*"]

bindings:
  - role: role-user
    members: ["user:bob-subject"]
  - role: role-email
    members: ["email:bob@example.com"]
  - role: role-group
    members: ["group:eng-team"]
  - role: role-node
    members: ["user:node-user"]
  - role: role-direct
    members: ["idp_role:role-direct"]
  - role: admin
    members: ["user:admin-user"]
  - role: admin
    members: ["user:nodeB"]
  # This suite permutes service/target grants, not enrollment; every identity
  # (including the deliberately unknown one) must still get a seat.
  - role: mesh:role:node
    members: ["mesh:system:authenticated"]
`
	if err := os.WriteFile(controlPlanePolicyFile, []byte(controlPlanePolicyYAML), 0644); err != nil {
		t.Fatal(err)
	}

	httpPortCP, cleanupCP := startControlPlaneAndRouter(t, tmpDir, oidcURL, mintToken, controlPlanePolicyFile)
	defer cleanupCP()

	// 2. Node B (Target) Config
	nodeBPolicyFile := filepath.Join(tmpDir, "nodeB_config.yaml")
	nodeBPolicyYAML := `version: "v1alpha1"
services:
  - type: "mcp"
    name: "test-user"
    command: ["echo", "test-user"]
  - type: "mcp"
    name: "test-email"
    command: ["echo", "test-email"]
  - type: "mcp"
    name: "test-group"
    command: ["echo", "test-group"]
  - type: "mcp"
    name: "test-role"
    command: ["echo", "test-role"]
  - type: "mcp"
    name: "test-node"
    command: ["echo", "test-node"]
`
	if err := os.WriteFile(nodeBPolicyFile, []byte(nodeBPolicyYAML), 0644); err != nil {
		t.Fatal(err)
	}

	homeB := filepath.Join(tmpDir, "nodeB")
	apiTokenB := "tokenB"

	nodeB := launchNode(t, nodeBin, os.Environ(), homeB, "run",
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", httpPortCP),
		"--data-dir", homeB,
		"--api-token-path", tokenPath(t, apiTokenB),
		"--jwt", mintToken(map[string]interface{}{
			"sub":    "bob-subject",
			"roles":  []string{api.RoleNode},
			"email":  "nodeB@example.com",
			"groups": []string{"compute", "backend"},
		}),
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--allow-loopback",
		"--config", nodeBPolicyFile,
	)
	nodeB.waitForAPI(t)
	addrB := nodeB.p2pAddr

	// 3. Test Permutations
	tests := []struct {
		name                  string
		jwtClaims             map[string]interface{}
		targetSvc             string
		expectAllow           bool
		expectControlPlaneErr bool
	}{
		{
			name:        "Fact sub: user(bob-subject)",
			jwtClaims:   map[string]interface{}{"sub": "bob-subject"},
			targetSvc:   "mcp://test-user",
			expectAllow: true,
		},
		{
			name:        "Fact email: email(bob@example.com)",
			jwtClaims:   map[string]interface{}{"sub": "some-id", "email": "bob@example.com"},
			targetSvc:   "mcp://test-email",
			expectAllow: true,
		},
		{
			name:        "Fact groups: group(eng-team)",
			jwtClaims:   map[string]interface{}{"sub": "some-id", "groups": []string{"eng-team"}},
			targetSvc:   "mcp://test-group",
			expectAllow: true,
		},
		{
			name:        "Fact idp_role bound: idp_role(role-direct) -> role(role-direct)",
			jwtClaims:   map[string]interface{}{"sub": "some-id", "roles": []string{"role-direct"}},
			targetSvc:   "mcp://test-role",
			expectAllow: true,
		},
		{
			// The issuer's roles claim used to be minted as role() itself, so
			// naming any mesh role in it granted that role with no binding.
			name:        "Fact idp_role unbound: roles claim does not mint a mesh role",
			jwtClaims:   map[string]interface{}{"sub": "some-id", "roles": []string{"role-unbound"}},
			targetSvc:   "mcp://test-unbound",
			expectAllow: false,
		},
		{
			name:        "Fact node: node(peerID)",
			jwtClaims:   map[string]interface{}{"sub": "node-user"},
			targetSvc:   "mcp://test-node",
			expectAllow: true,
		},
		{
			name:        "Unknown User / No Roles -> control plane error",
			jwtClaims:   map[string]interface{}{"sub": "unknown"},
			targetSvc:   "mcp://test-user",
			expectAllow: false,
		},
	}

	t.Run("cases", func(t *testing.T) {
		for i, tt := range tests {
			i, tt := i, tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				homeA := filepath.Join(tmpDir, fmt.Sprintf("nodeA_%d", i))
				apiTokenA := "tokenA"

				// The seat comes from the mesh:system:authenticated binding above,
				// never from a roles claim naming mesh:role:node.
				jwtA := mintToken(tt.jwtClaims)

				nodeA := launchNode(t, nodeBin, os.Environ(), homeA, "run",
					"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", httpPortCP),
					"--data-dir", homeA,
					"--api-token-path", tokenPath(t, apiTokenA),
					"--jwt", jwtA,
					"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
					"--allow-loopback",
				)
				defer nodeA.kill()
				actualApiAddrA := nodeA.waitForAPI(t)

				parts := strings.Split(addrB, "/p2p/")
				if len(parts) != 2 {
					t.Fatalf("unexpected addrB format: %s", addrB)
				}
				peerIDB := parts[1]
				connectPeerWithToken(t, actualApiAddrA, apiTokenA, addrB)

				// Use the local callMCPAllowError targeting Node A to hit Node B
				resp, callErr := callMCPAllowError(t, actualApiAddrA, apiTokenA, "call_remote_tool", map[string]any{
					"peer_id":   peerIDB,
					"tool_name": tt.targetSvc + "/test_tool",
					"arguments": map[string]any{},
				})

				// call_remote_tool might return a JSON error inside resp, or callErr might be non-nil.
				failed := false
				if callErr != nil {
					if strings.Contains(callErr.Error(), "EOF") {
						// EOF means the 'echo' backend started and exited, which means AuthZ succeeded!
						failed = false
					} else {
						failed = true
					}
				} else if strings.Contains(resp, "Authorization failed") || strings.Contains(resp, "failed to connect") || strings.Contains(resp, "token lacks") || strings.Contains(resp, "denied") {
					failed = true
				}

				if tt.expectAllow {
					if failed {
						t.Errorf("expected success, got error: %v / %s", callErr, resp)
					}
				} else {
					if !failed {
						t.Errorf("expected failure, got success: %s", resp)
					}
				}
			})
		}
	})
}
