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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForMCPAddrLocal(t *testing.T, logPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second) // Increased timeout
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		lines := strings.Split(string(data), "\n")
		t.Logf("Read %d bytes, %d lines", len(data), len(lines))
		for _, line := range lines {
			if strings.Contains(line, "Starting MCP server on TCP address ") {
				t.Logf("Found line: %s", line)
				parts := strings.Split(line, "Starting MCP server on TCP address ")
				t.Logf("Parts len: %d", len(parts))
				if len(parts) > 1 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Dump logs on failure
	data, _ := os.ReadFile(logPath)
	t.Logf("Node logs:\n%s", string(data))

	t.Fatalf("timeout waiting for MCP addr in log: %s", logPath)
	return ""
}

func TestRegistryIntegration(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	_, hubAddr := startMockLibp2pHub(t)

	homeA := t.TempDir()
	homeB := t.TempDir()

	// Start Node A (Client)
	t.Log("Starting Node A...")
	_ = startBackgroundNode(t, nodeBin, hubAddr, homeA, "--listen", "/ip4/127.0.0.1/udp/0/quic-v1", "--listen", "/ip4/127.0.0.1/tcp/0", "--discovery-interval", "100ms")
	mcpAddrA := waitForMCPAddrLocal(t, filepath.Join(homeA, "node.log"))

	// Start Node B (Provider)
	t.Log("Starting Node B...")
	_ = startBackgroundNode(t, nodeBin, hubAddr, homeB, "--listen", "/ip4/127.0.0.1/udp/0/quic-v1", "--listen", "/ip4/127.0.0.1/tcp/0", "--discovery-interval", "100ms")
	mcpAddrB := waitForMCPAddrLocal(t, filepath.Join(homeB, "node.log"))
	addrB := waitForPeerInfoInLog(t, filepath.Join(homeB, "node.log"))

	// Force Node A to connect to Node B
	callMCP(t, mcpAddrA, "connect_peer", map[string]any{"peer_addr": addrB})

	// Wait for connection
	time.Sleep(1 * time.Second)

	// Node B registers a capability via /sam/register
	t.Log("Node B registering capability...")
	nodeBURL := "http://" + mcpAddrB + "/sam/register"
	reqBody := `{"skills":["math"]}`
	resp, err := http.Post(nodeBURL, "application/json", bytes.NewBufferString(reqBody))
	if err != nil {
		t.Fatalf("Failed to register capability: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status OK, got %d. Body: %s", resp.StatusCode, string(body))
	}

	// Wait for DHT propagation
	time.Sleep(2 * time.Second)

	// Node A searches for the capability
	t.Log("Node A searching for capability...")
	searchResult := callMCP(t, mcpAddrA, "search_nodes", map[string]any{"capability": "math"})
	t.Logf("Search result: %s", searchResult)

	// Verify search result contains Node B's peer ID
	parts := strings.Split(addrB, "/")
	nodeBPeerID := parts[len(parts)-1]
	if !strings.Contains(searchResult, nodeBPeerID) {
		t.Errorf("Expected search result to contain %s, got %s", nodeBPeerID, searchResult)
	}

	// Node A inspects Node B
	t.Log("Node A inspecting Node B...")
	inspectResult := callMCP(t, mcpAddrA, "inspect_node", map[string]any{"peer_id": nodeBPeerID})
	t.Logf("Inspect result: %s", inspectResult)

	if !strings.Contains(inspectResult, "math") {
		t.Errorf("Expected inspect result to contain 'math', got %s", inspectResult)
	}
}
