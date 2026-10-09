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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/agentmesh/api"
)

func TestLocalPolicyCanGrantPermissions(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpDir := t.TempDir()

	oidcURL, mintToken := startCustomMockOIDC(t)

	// Control plane policy that grants NOTHING.
	controlPlanePolicyFile := filepath.Join(tmpDir, "policies.yaml")
	controlPlanePolicyYAML := `roles:
  - name: none
    allowed_services: []
    allowed_targets: ["*"]
bindings:
  - role: none
    members: ["user:unprivileged-user"]
  - role: none
    members: ["user:nodeB-user"]
  - role: mesh:role:node
    members: ["user:unprivileged-user", "user:nodeB-user"]
`
	if err := os.WriteFile(controlPlanePolicyFile, []byte(controlPlanePolicyYAML), 0644); err != nil {
		t.Fatal(err)
	}

	httpPortCP, cleanupCP := startControlPlaneAndRouter(t, tmpDir, oidcURL, mintToken, controlPlanePolicyFile)
	defer cleanupCP()

	// Node B Config with a permissive local policy
	nodeBPolicyFile := filepath.Join(tmpDir, "nodeB_config.yaml")
	nodeBPolicyYAML := `version: "v1alpha1"
services:
  - type: "mcp"
    name: "test-tool"
    command: ["echo", "test-tool"]
attenuation:
  policies:
    - 'allow if true;'
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
			"sub":   "nodeB-user",
			"roles": []string{api.RoleNode},
		}),
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--allow-loopback",
		"--config", nodeBPolicyFile,
	)
	nodeB.waitForAPI(t)
	addrB := nodeB.p2pAddr

	parts := strings.Split(addrB, "/p2p/")
	if len(parts) != 2 {
		t.Fatalf("unexpected addrB format: %s", addrB)
	}
	peerIDB := parts[1]

	// Node A Config (unprivileged user)
	homeA := filepath.Join(tmpDir, "nodeA")
	apiTokenA := "tokenA"

	nodeA := launchNode(t, nodeBin, os.Environ(), homeA, "run",
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", httpPortCP),
		"--data-dir", homeA,
		"--api-token-path", tokenPath(t, apiTokenA),
		"--jwt", mintToken(map[string]interface{}{
			"sub":   "unprivileged-user",
			"roles": []string{api.RoleNode},
		}),
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--allow-loopback",
	)
	actualApiAddrA := nodeA.waitForAPI(t)

	// Make request from Node A to Node B
	// Even though Node A has no control plane permissions, Node B's local policy "allow if true;" should permit it.
	resp, callErr := callMCPAllowError(t, actualApiAddrA, apiTokenA, "call_remote_tool", map[string]any{
		"peer_id":   peerIDB,
		"tool_name": "mcp://test-tool/test_tool",
		"arguments": map[string]any{},
	})

	failed := false
	if callErr != nil {
		if strings.Contains(callErr.Error(), "EOF") {
			failed = false
		} else {
			failed = true
		}
	} else if strings.Contains(resp, "Authorization failed") || strings.Contains(resp, "failed to connect") || strings.Contains(resp, "token lacks") || strings.Contains(resp, "denied") {
		failed = true
	}

	if failed {
		t.Errorf("expected success due to local permissive policy, got error: %v / %s", callErr, resp)
	}
}

func TestLocalPolicyCannotBypassControlPlaneTargetConstraint(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpDir := t.TempDir()

	oidcURL, mintToken := startCustomMockOIDC(t)

	// Control plane policy that grants service access but RESTRICTS target to "group:admin-only".
	controlPlanePolicyFile := filepath.Join(tmpDir, "policies.yaml")
	controlPlanePolicyYAML := `roles:
  - name: restricted-role
    allowed_services: ["*"]
    allowed_targets: ["group:admin-only"]
bindings:
  - role: restricted-role
    members: ["user:client-user"]
  - role: restricted-role
    members: ["user:nodeB-user"]
  - role: mesh:role:node
    members: ["user:client-user", "user:nodeB-user"]
`
	if err := os.WriteFile(controlPlanePolicyFile, []byte(controlPlanePolicyYAML), 0644); err != nil {
		t.Fatal(err)
	}

	httpPortCP, cleanupCP := startControlPlaneAndRouter(t, tmpDir, oidcURL, mintToken, controlPlanePolicyFile)
	defer cleanupCP()

	// Node B Config with a permissive local policy ("allow if true;")
	// Node B will NOT claim "group:admin-only", so it will fail the control plane's target check.
	nodeBPolicyFile := filepath.Join(tmpDir, "nodeB_config.yaml")
	nodeBPolicyYAML := `version: "v1alpha1"
services:
  - type: "mcp"
    name: "test-tool"
    command: ["echo", "test-tool"]
attenuation:
  policies:
    - 'allow if true;'
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
			"sub":   "nodeB-user",
			"roles": []string{api.RoleNode},
		}),
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--allow-loopback",
		"--config", nodeBPolicyFile,
	)
	nodeB.waitForAPI(t)
	addrB := nodeB.p2pAddr

	parts := strings.Split(addrB, "/p2p/")
	if len(parts) != 2 {
		t.Fatalf("unexpected addrB format: %s", addrB)
	}
	peerIDB := parts[1]

	// Node A Config (Client)
	homeA := filepath.Join(tmpDir, "nodeA")
	apiTokenA := "tokenA"

	nodeA := launchNode(t, nodeBin, os.Environ(), homeA, "run",
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", httpPortCP),
		"--data-dir", homeA,
		"--api-token-path", tokenPath(t, apiTokenA),
		"--jwt", mintToken(map[string]interface{}{
			"sub":   "client-user",
			"roles": []string{api.RoleNode},
		}),
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--allow-loopback",
	)
	actualApiAddrA := nodeA.waitForAPI(t)

	// Node A attempts to call Node B.
	// Node A's token allows calling "*" but restricts the target to "group:admin-only".
	// Node B doesn't have the "group:admin-only" identity, so the target check will fail.
	// Node B's local policy "allow if true;" cannot bypass this failed check.
	resp, callErr := callMCPAllowError(t, actualApiAddrA, apiTokenA, "call_remote_tool", map[string]any{
		"peer_id":   peerIDB,
		"tool_name": "mcp://test-tool/test_tool",
		"arguments": map[string]any{},
	})

	failed := false
	if callErr != nil {
		if strings.Contains(callErr.Error(), "EOF") {
			failed = false
		} else {
			failed = true
		}
	} else if strings.Contains(resp, "Authorization failed") || strings.Contains(resp, "failed to connect") || strings.Contains(resp, "token lacks") || strings.Contains(resp, "denied") || strings.Contains(resp, "biscuit") {
		failed = true
	}

	if !failed {
		t.Errorf("expected failure due to control plane target constraint check, got success. Resp: %s", resp)
	}
}
