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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// routerFixture is one agentmesh-router the test started, with the key it
// was given so its peer ID is known before it runs.
type routerFixture struct {
	cmd    *exec.Cmd
	peerID peer.ID
	port   int
}

// startLabelledRouter runs a router that declares the given labels and
// listens on a port of its own.
func startLabelledRouter(t *testing.T, routerBin, tmpDir, name string, cpPort int, routerJWT string, labels ...string) *routerFixture {
	t.Helper()
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	privData, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	peerID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keysPath := filepath.Join(tmpDir, name+".key")
	if err := os.WriteFile(keysPath, privData, 0o600); err != nil {
		t.Fatal(err)
	}
	port := getFreePort(t)
	args := []string{
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
		"--listen", fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port),
		"--keys-path", keysPath,
		"--allow-loopback",
		"--oidc-token", routerJWT,
		"--lease-renew-interval", "250ms",
	}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	cmd := exec.Command(routerBin, args...)
	logPath := filepath.Join(tmpDir, name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		if t.Failed() {
			out, _ := os.ReadFile(logPath)
			t.Logf("--- %s ---\n%s", name, out)
		}
	})
	return &routerFixture{cmd: cmd, peerID: peerID, port: port}
}

// routersConnectedTo lists the routers whose last lease reported a
// connection from the peer, by peer ID. A connection is not an attachment:
// a DHT query or a transit handshake connects a node to a router it does not
// hold as a relay.
func routersConnectedTo(t *testing.T, cpPort int, peerID peer.ID) []string {
	t.Helper()
	var out []string
	for _, lease := range fetchAdminStatus(t, cpPort, testAdminToken).GetActiveRouters() {
		if slices.Contains(lease.GetConnectedPeers(), peerID.String()) {
			out = append(out, lease.GetPeerId())
		}
	}
	slices.Sort(out)
	return out
}

// attachedRouters reads the routers the node holds as relays from its own
// debug endpoint.
func attachedRouters(t *testing.T, n *backgroundNode) []string {
	t.Helper()
	var info struct {
		AttachedRouters []string `json:"attached_routers"`
	}
	if err := json.Unmarshal([]byte(debugGetWithToken(t, n.apiAddr, n.token, "/debug/mesh-info")), &info); err != nil {
		t.Fatalf("decode mesh-info: %v", err)
	}
	slices.Sort(info.AttachedRouters)
	return info.AttachedRouters
}

// debugGetWithToken is debugGet for a node whose API token is not the
// shared test token.
func debugGetWithToken(t *testing.T, apiAddr, token, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+apiAddr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.HeaderMeshAuthentication, "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s failed: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return string(body)
}

func peerIDStrings(ids ...peer.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	slices.Sort(out)
	return out
}

// waitForEqual polls get until it returns want.
func waitForEqual(t *testing.T, what string, get func() []string, want []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := get()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestNodesOnDifferentRoutersReachEachOther pins the mesh's shape when a
// node attaches to a subset of the routers: two routers labelled by site,
// node A selecting site a and node B selecting site b, so neither router
// holds both. B discovers A's service through the one DHT and calls it over
// a circuit through A's router, which admits B on the handshake B runs
// there before asking for the circuit. Attachment is read from each node;
// the control plane's view of a router's connections also counts DHT and
// transit connections, so it is read for federation and transit only.
func TestNodesOnDifferentRoutersReachEachOther(t *testing.T) {
	cpBin := buildBinary(t, "./cmd/agentmesh-control-plane")
	routerBin := buildBinary(t, "./cmd/agentmesh-router")
	nodeBin := buildBinary(t, "./cmd/agentmesh-node")

	tmpDir := t.TempDir()
	// The router role must allow the site label, or enrollment refuses it.
	policyFile := filepath.Join(tmpDir, "policies.yaml")
	policy := fmt.Sprintf(`roles:
  - name: %s
    allowed_services: []
    allowed_targets: ["*"]
    allowed_labels: ["site=*"]
  - name: %s
    allowed_services: ["mcp://*"]
    allowed_targets: ["*"]
bindings:
  - role: %s
    members: ["group:routers"]
  - role: %s
    members: ["user:mock-user"]
`, api.RoleRouter, api.RoleNode, api.RoleRouter, api.RoleNode)
	if err := os.WriteFile(policyFile, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}

	oidcURL, mintToken := startCustomMockOIDC(t)
	routerJWT := mintToken(map[string]interface{}{
		"sub":    "router-sites",
		"groups": []string{"routers"},
		"roles":  []string{api.RoleRouter},
	})
	nodeJWT := mintToken(map[string]interface{}{
		"sub":   "mock-user",
		"roles": []string{api.RoleNode},
	})
	jwtPath := filepath.Join(tmpDir, "node.jwt")
	if err := os.WriteFile(jwtPath, []byte(nodeJWT), 0o600); err != nil {
		t.Fatal(err)
	}

	cpPort := getFreePort(t)
	cpCmd := exec.Command(cpBin,
		"--bind-address", fmt.Sprintf("127.0.0.1:%d", cpPort),
		"--admin-token-path", tokenPath(t, testAdminToken),
		"--db-dsn", filepath.Join(tmpDir, "cp.db")+"?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)",
		"--issuer", oidcURL,
		"--insecure-skip-tls-verify",
	)
	if err := cpCmd.Start(); err != nil {
		t.Fatalf("start control plane: %v", err)
	}
	t.Cleanup(func() { _ = cpCmd.Process.Kill(); _ = cpCmd.Wait() })
	waitForControlPlane(t, cpPort)
	injectPolicyYAML(t, cpPort, testAdminToken, policyFile)

	// A router federates on its first pass over /info and then every 30s,
	// so router B starts once router A holds a lease and finds it at once.
	routerA := startLabelledRouter(t, routerBin, tmpDir, "router-a", cpPort, routerJWT, "site=a")
	waitForActiveRouters(t, cpPort, 1, 10*time.Second)
	routerB := startLabelledRouter(t, routerBin, tmpDir, "router-b", cpPort, routerJWT, "site=b")
	waitForActiveRouters(t, cpPort, 2, 10*time.Second)
	waitForEqual(t, "routers connected to router B", func() []string { return routersConnectedTo(t, cpPort, routerB.peerID) },
		peerIDStrings(routerA.peerID), 10*time.Second)

	// A real MCP server behind node A; the node probes it before it
	// advertises the service.
	mcpServer := httptest.NewServer(newBoundaryMCPHandler(t))
	t.Cleanup(mcpServer.Close)

	nodeEnv := func(name string) []string {
		home := filepath.Join(tmpDir, name)
		return append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	}
	common := []string{
		"run", "--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
		"--jwt-path", jwtPath,
		"--discovery-interval", "200ms",
		"--monitor-bootstrap", "500ms",
		"--monitor-interval", "500ms",
		"--autorelay-boot-delay", "0s",
		"--autorelay-min-interval", "200ms",
		"--autorelay-backoff", "200ms",
	}
	// Neither node publishes its loopback address (no --allow-loopback), so
	// the only path to either is a circuit through its router.
	nodeA := launchNode(t, nodeBin, nodeEnv("node-a"), filepath.Join(tmpDir, "node-a"), append(slices.Clone(common),
		"--api-token-path", tokenPath(t, "token-a"),
		"--router-selector", "site=a",
		"--config", writeNodeConfig(t, tmpDir, nil, svcDecl{Type: "mcp", Name: "site-tool", TargetURL: mcpServer.URL}),
	)...)
	nodeB := launchNode(t, nodeBin, nodeEnv("node-b"), filepath.Join(tmpDir, "node-b"), append(slices.Clone(common),
		"--api-token-path", tokenPath(t, "token-b"),
		"--router-selector", "site=b",
	)...)
	nodeA.waitForAPI(t)
	nodeB.waitForAPI(t)

	// Each node holds the router its selector names and no other.
	waitForEqual(t, "routers attached by node A", func() []string { return attachedRouters(t, nodeA) }, peerIDStrings(routerA.peerID), 10*time.Second)
	waitForEqual(t, "routers attached by node B", func() []string { return attachedRouters(t, nodeB) }, peerIDStrings(routerB.peerID), 10*time.Second)

	// B finds A's service: the DHT spans both routers.
	client := &http.Client{Timeout: 5 * time.Second}
	discover := func() string {
		req, _ := http.NewRequest(http.MethodGet, "http://"+nodeB.apiAddr+"/mesh/service/discover?type=mcp&name=site-tool", nil)
		req.Header.Set(api.HeaderMeshAuthentication, "Bearer token-b")
		resp, err := client.Do(req)
		if err != nil {
			return ""
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(discover(), "site-tool") {
		if time.Now().After(deadline) {
			t.Fatalf("node B never discovered node A's service.\n--- node A ---\n%s\n--- node B ---\n%s", nodeA.log(), nodeB.log())
		}
		time.Sleep(300 * time.Millisecond)
	}

	// B calls A: the circuit goes through router A, where B is a stranger
	// until it runs the handshake there.
	callURL := fmt.Sprintf("http://%s/mesh/%s/mcp/site-tool", nodeB.apiAddr, nodeA.peerID)
	var lastStatus int
	var lastBody string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, callURL, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(api.HeaderMeshAuthentication, "Bearer token-b")
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastStatus, lastBody = resp.StatusCode, string(body)
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if lastStatus != http.StatusOK {
		t.Fatalf("call from B to A across routers: %d %s\n--- node A ---\n%s\n--- node B ---\n%s", lastStatus, lastBody, nodeA.log(), nodeB.log())
	}

	// The call left B connected to router A as well, without attaching to
	// it; A still holds only its own router.
	waitForEqual(t, "routers connected to node B", func() []string { return routersConnectedTo(t, cpPort, nodeB.peerID) },
		peerIDStrings(routerA.peerID, routerB.peerID), 10*time.Second)
	if got := attachedRouters(t, nodeB); !slices.Equal(got, peerIDStrings(routerB.peerID)) {
		t.Fatalf("routers attached by node B after the call: got %v, want %v", got, peerIDStrings(routerB.peerID))
	}
	if got := attachedRouters(t, nodeA); !slices.Equal(got, peerIDStrings(routerA.peerID)) {
		t.Fatalf("routers attached by node A after the call: got %v, want %v", got, peerIDStrings(routerA.peerID))
	}
}
