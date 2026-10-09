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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
)

func TestIntegrationStdioDatapath(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	_, routerAddr := startMockRouter(t)

	homeA := t.TempDir()
	homeB := t.TempDir()

	apiToken := "test-token"

	// The stdio service is declared in node A's configuration; there is no
	// runtime registration.
	serviceName := "stdio-tool"
	cfgA := writeNodeConfig(t, homeA, nil, svcDecl{Type: "mcp", Name: serviceName, Command: []string{"cat"}})

	// Start Node A
	t.Log("Starting Node A...")
	nodeA := startBackgroundNode(t, nodeBin, routerAddr, homeA,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
		"--api-token-path", tokenPath(t, apiToken),
		"--config", cfgA,
	)

	// Start Node B
	t.Log("Starting Node B...")
	nodeB := startBackgroundNode(t, nodeBin, routerAddr, homeB,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
		"--api-token-path", tokenPath(t, apiToken),
	)

	nodeA.waitForAPI(t)
	actualApiAddrB := nodeB.waitForAPI(t)

	addrA := nodeA.p2pAddr
	peerIDA := getPeerIDFromAddr(addrA)

	// Connect Node B to Node A
	connectPeer(t, actualApiAddrB, addrA)

	// Wait for propagation (optional, but safe)
	time.Sleep(1 * time.Second)

	// Node B calls Node A's service via its local egress proxy
	// URL format: http://localhost:<port>/sam/{peer_id}/{service_type}/{service_name}/{upstream_path}
	postURL := fmt.Sprintf("http://%s/sam/%s/mcp/%s/", actualApiAddrB, peerIDA, serviceName)

	client := &http.Client{}

	// Test Streamable HTTP (http-first mode)
	// Send message via POST and expect the response in the HTTP response body
	testMessage := `{"jsonrpc":"2.0","method":"ping","id":1}`

	var postResp *http.Response
	var err error
	for i := 0; i < 3; i++ {
		postReq, _ := http.NewRequest("POST", postURL, bytes.NewBufferString(testMessage))
		postReq.Header.Set(api.HeaderSamAuthentication, "Bearer "+apiToken)
		postReq.Header.Set("Content-Type", "application/json")
		postReq.Header.Set("Accept", "application/json")

		postResp, err = client.Do(postReq)
		if err == nil {
			if postResp.StatusCode == http.StatusOK {
				break
			}
			_ = postResp.Body.Close()
		}
		t.Logf("POST Attempt %d failed: %v, status: %v", i+1, err, postResp)
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = postResp.Body.Close() }()

	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected POST status OK, got %d", postResp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(postResp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// The bridge rewrites the JSON-RPC id on the way to the backend and
	// restores it on the way back, so the echo is the same message
	// re-serialized, not the same bytes.
	var want, got map[string]any
	if err := json.Unmarshal([]byte(testMessage), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bodyBytes, &got); err != nil {
		t.Fatalf("POST response is not JSON: %v (%q)", err, bodyBytes)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("Expected to receive %v in POST response, got %v", want, got)
	}
}

func TestIntegrationHTTPDatapath(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	_, routerAddr := startMockRouter(t)

	homeA := t.TempDir()
	homeB := t.TempDir()

	apiToken := "test-token"

	// Start a dummy HTTP server on Node A's host (simulating local service).
	// It captures the headers it receives so we can assert what actually
	// crosses the mesh: verified caller identity in, biscuit stripped. The
	// backend exists before the node: services are declared in the node's
	// configuration, never registered at runtime.
	expectedBody := `{"status":"success"}`
	var (
		hdrMu      sync.Mutex
		gotPeerID  string
		gotBiscuit string
	)
	dummyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrMu.Lock()
		gotPeerID = r.Header.Get(api.HeaderPeerID)
		gotBiscuit = r.Header.Get(api.HeaderSamBiscuit)
		hdrMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expectedBody))
	}))
	defer dummyServer.Close()

	serviceName := "http-tool"
	cfgA := writeNodeConfig(t, homeA, nil, svcDecl{Type: "mcp", Name: serviceName, TargetURL: dummyServer.URL})

	// Start Node A
	t.Log("Starting Node A...")
	nodeA := startBackgroundNode(t, nodeBin, routerAddr, homeA,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
		"--api-token-path", tokenPath(t, apiToken),
		"--config", cfgA,
	)

	// Start Node B
	t.Log("Starting Node B...")
	nodeB := startBackgroundNode(t, nodeBin, routerAddr, homeB,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
		"--api-token-path", tokenPath(t, apiToken),
	)

	nodeA.waitForAPI(t)
	actualApiAddrB := nodeB.waitForAPI(t)

	addrA := nodeA.p2pAddr
	peerIDA := getPeerIDFromAddr(addrA)
	addrB := nodeB.p2pAddr
	peerIDB := getPeerIDFromAddr(addrB)

	// Connect Node B to Node A
	connectPeer(t, actualApiAddrB, addrA)

	// Wait for propagation
	time.Sleep(1 * time.Second)

	// Node B calls Node A's service via its local egress proxy
	url := fmt.Sprintf("http://%s/sam/%s/mcp/%s/testpath", actualApiAddrB, peerIDA, serviceName)

	client := &http.Client{}
	var httpResp *http.Response
	var err error
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set(api.HeaderSamAuthentication, "Bearer "+apiToken)
		// Spoof attempt: the backend must see the verified peer id, not this.
		req.Header.Set(api.HeaderPeerID, "spoofed-peer")
		req.Close = true // Force close the connection so the libp2p stream terminates and flushed accounting logs
		httpResp, err = client.Do(req)
		if err == nil && httpResp.StatusCode == http.StatusOK {
			break
		}
		t.Logf("HTTP Connect Attempt %d failed: %v, status: %v", i+1, err, httpResp)
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status OK, got %d", httpResp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()

	if string(bodyBytes) != expectedBody {
		t.Fatalf("Expected body %s, got %s", expectedBody, string(bodyBytes))
	}

	hdrMu.Lock()
	if gotPeerID != peerIDB.String() {
		t.Errorf("backend %s: got %q, want verified caller %q (spoofed inbound value must be overwritten)", api.HeaderPeerID, gotPeerID, peerIDB)
	}
	if gotBiscuit != "" {
		t.Errorf("%s leaked to backend: %q", api.HeaderSamBiscuit, gotBiscuit)
	}
	hdrMu.Unlock()

	// Verify Audit Traceability and Stream Accounting logs were emitted
	assertLogInFile(t, filepath.Join(homeA, "node.log"), "Audit Traceability")
	assertLogInFile(t, filepath.Join(homeA, "node.log"), "Stream Accounting")
	assertLogInFile(t, filepath.Join(homeA, "node.log"), "bytes_read")
	assertLogInFile(t, filepath.Join(homeA, "node.log"), "bytes_written")
}

func assertLogInFile(t *testing.T, logFile, expectedMsg string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(logFile)
		if err == nil && strings.Contains(string(b), expectedMsg) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(logFile)
	logEnd := string(b)
	if len(b) > 1000 {
		logEnd = string(b[len(b)-1000:])
	}
	t.Errorf("Expected log file %s to contain %q, but it didn't.\nLog end:\n%s", logFile, expectedMsg, logEnd)
}
