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
	"syscall"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// routerFixture is one agentmesh-router the test started, with the key it
// was given so its peer ID is known before it runs.
type routerFixture struct {
	cmd         *exec.Cmd
	peerID      peer.ID
	port        int
	metricsAddr string
}

// gauge reads one metric from the router's /metrics, by its full name with
// labels, and fails when it is not there.
func (r *routerFixture) gauge(t *testing.T, name string) float64 {
	t.Helper()
	resp, err := http.Get("http://" + r.metricsAddr + "/metrics")
	if err != nil {
		t.Fatalf("scrape %s: %v", r.metricsAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, name+" ") {
			var v float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, name+" "), "%g", &v); err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("metric %s not served by %s", name, r.metricsAddr)
	return 0
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
	metricsAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	args := []string{
		"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
		"--listen", fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port),
		"--keys-path", keysPath,
		"--allow-loopback",
		"--oidc-token", routerJWT,
		"--lease-renew-interval", "250ms",
		"--metrics-addr", metricsAddr,
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
	return &routerFixture{cmd: cmd, peerID: peerID, port: port, metricsAddr: metricsAddr}
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

// meshInfo reads the node's own view: the routers it holds and every peer
// it has a connection with.
func meshInfo(t *testing.T, n *backgroundNode) (attached, connected []string) {
	t.Helper()
	var info struct {
		AttachedRouters []string `json:"attached_routers"`
		ConnectedPeers  []string `json:"connected_peers"`
	}
	if err := json.Unmarshal([]byte(debugGetWithToken(t, n.apiAddr, n.token, "/debug/mesh-info")), &info); err != nil {
		t.Fatalf("decode mesh-info: %v", err)
	}
	slices.Sort(info.AttachedRouters)
	slices.Sort(info.ConnectedPeers)
	return info.AttachedRouters, info.ConnectedPeers
}

// attachedRouters reads the routers the node holds as relays from its own
// debug endpoint.
func attachedRouters(t *testing.T, n *backgroundNode) []string {
	t.Helper()
	attached, _ := meshInfo(t, n)
	return attached
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

// attachmentMesh is a control plane with a policy that lets routers carry
// a site label, and the credentials and binaries to start routers and
// nodes against it.
type attachmentMesh struct {
	tmpDir    string
	cpPort    int
	routerBin string
	nodeBin   string
	routerJWT string
	jwtPath   string
}

func startAttachmentMesh(t *testing.T) *attachmentMesh {
	t.Helper()
	cpBin := buildBinary(t, "./cmd/agentmesh-control-plane")
	m := &attachmentMesh{
		tmpDir:    t.TempDir(),
		routerBin: buildBinary(t, "./cmd/agentmesh-router"),
		nodeBin:   buildBinary(t, "./cmd/agentmesh-node"),
	}
	// The router role must allow the site label, or enrollment refuses it.
	policyFile := filepath.Join(m.tmpDir, "policies.yaml")
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
	m.routerJWT = mintToken(map[string]interface{}{
		"sub":    "router-sites",
		"groups": []string{"routers"},
		"roles":  []string{api.RoleRouter},
	})
	nodeJWT := mintToken(map[string]interface{}{
		"sub":   "mock-user",
		"roles": []string{api.RoleNode},
	})
	m.jwtPath = filepath.Join(m.tmpDir, "node.jwt")
	if err := os.WriteFile(m.jwtPath, []byte(nodeJWT), 0o600); err != nil {
		t.Fatal(err)
	}

	m.cpPort = getFreePort(t)
	cpCmd := exec.Command(cpBin,
		"--bind-address", fmt.Sprintf("127.0.0.1:%d", m.cpPort),
		"--admin-token-path", tokenPath(t, testAdminToken),
		"--db-dsn", filepath.Join(m.tmpDir, "cp.db")+"?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)",
		"--issuer", oidcURL,
		"--insecure-skip-tls-verify",
	)
	if err := cpCmd.Start(); err != nil {
		t.Fatalf("start control plane: %v", err)
	}
	t.Cleanup(func() { _ = cpCmd.Process.Kill(); _ = cpCmd.Wait() })
	waitForControlPlane(t, m.cpPort)
	injectPolicyYAML(t, m.cpPort, testAdminToken, policyFile)
	return m
}

// startRouter starts one router and waits for its lease; routers started
// one after another federate on their first pass over /info, which a
// router makes at start and then every 30s.
func (m *attachmentMesh) startRouter(t *testing.T, name string, labels ...string) *routerFixture {
	t.Helper()
	before := len(fetchAdminStatus(t, m.cpPort, testAdminToken).GetActiveRouters())
	r := startLabelledRouter(t, m.routerBin, m.tmpDir, name, m.cpPort, m.routerJWT, labels...)
	waitForActiveRouters(t, m.cpPort, before+1, 10*time.Second)
	return r
}

// nodeArgs is the run command every node of the mesh starts with.
func (m *attachmentMesh) nodeArgs() []string {
	return []string{
		"run", "--control-plane", fmt.Sprintf("http://127.0.0.1:%d", m.cpPort),
		"--jwt-path", m.jwtPath,
		"--discovery-interval", "200ms",
		"--autorelay-boot-delay", "0s",
		"--autorelay-min-interval", "200ms",
		"--autorelay-backoff", "200ms",
	}
}

func (m *attachmentMesh) nodeEnv(name string) []string {
	home := filepath.Join(m.tmpDir, name)
	return append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
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
	m := startAttachmentMesh(t)
	tmpDir, cpPort, nodeBin := m.tmpDir, m.cpPort, m.nodeBin

	routerA := m.startRouter(t, "router-a", "site=a")
	routerB := m.startRouter(t, "router-b", "site=b")
	waitForEqual(t, "routers connected to router B", func() []string { return routersConnectedTo(t, cpPort, routerB.peerID) },
		peerIDStrings(routerA.peerID), 10*time.Second)

	// A real MCP server behind node A; the node probes it before it
	// advertises the service.
	mcpServer := httptest.NewServer(newBoundaryMCPHandler(t))
	t.Cleanup(mcpServer.Close)

	nodeEnv := m.nodeEnv
	common := append(m.nodeArgs(),
		"--monitor-bootstrap", "500ms",
		"--monitor-interval", "500ms",
	)
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

// TestNodeMovesWhenItsRouterStops pins what a router's stop does to a
// member: the router tells its members it is draining before it closes,
// and a node that held it attaches to another router at once. The monitor
// is kept out of the way so the move can only come from the go-away; a
// node that only noticed the connection drop would redial the stopped
// router and, with the monitor off, never top up.
func TestNodeMovesWhenItsRouterStops(t *testing.T) {
	m := startAttachmentMesh(t)
	routers := map[string]*routerFixture{}
	for _, name := range []string{"router-a", "router-b", "router-c"} {
		r := m.startRouter(t, name)
		routers[r.peerID.String()] = r
	}

	node := launchNode(t, m.nodeBin, m.nodeEnv("node"), filepath.Join(m.tmpDir, "node"), append(m.nodeArgs(),
		"--api-token-path", tokenPath(t, "token"),
		"--allow-loopback",
		"--routers", "2",
		"--monitor-bootstrap", "1h",
		"--monitor-interval", "1h",
	)...)
	node.waitForAPI(t)

	var before []string
	deadline := time.Now().Add(10 * time.Second)
	for len(before) != 2 && time.Now().Before(deadline) {
		before = attachedRouters(t, node)
		time.Sleep(100 * time.Millisecond)
	}
	if len(before) != 2 {
		t.Fatalf("node holds %v, want 2 of the 3 routers", before)
	}
	stopping, kept := routers[before[0]], before[1]
	var spare string
	for id := range routers {
		if id != before[0] && id != before[1] {
			spare = id
		}
	}

	// SIGTERM, as a pod gets: the router withdraws its lease, sends its
	// members away and closes.
	stoppedAt := time.Now()
	if err := stopping.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForEqual(t, "routers attached by the node after its router stopped", func() []string { return attachedRouters(t, node) },
		peerIDStrings(mustPeerID(t, kept), mustPeerID(t, spare)), 15*time.Second)
	moved := time.Since(stoppedAt)
	if err := stopping.cmd.Wait(); err != nil && !strings.Contains(err.Error(), "exit status") {
		t.Logf("router exit: %v", err)
	}
	log := node.log()
	if !strings.Contains(log, "sent this node away (DRAINING)") {
		t.Fatalf("the node did not log the go-away; moved in %s\n--- node ---\n%s", moved, log)
	}
	t.Logf("node moved to the spare router %s after its router stopped", moved)
}

func mustPeerID(t *testing.T, s string) peer.ID {
	t.Helper()
	id, err := peer.Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestIdleNodesHoldOnlyTheirRouters pins what a member costs the mesh when
// it is not calling anyone: a session with the routers it chose and nothing
// else. Three routers, three nodes each pinned to its own router, no
// service and no call; after the discovery interval has ticked many times,
// no node is connected to another node, and each router has authenticated
// its own node and the two other routers, nobody else. A node that sought
// out other members on its own would be connected to them, through their
// routers, and would have authenticated at every router on the way, which
// is the load of a member on every router rather than on two. Connections
// a node's DHT client holds to the other routers are allowed: they carry
// no session and are the first a router closes when it is full.
func TestIdleNodesHoldOnlyTheirRouters(t *testing.T) {
	m := startAttachmentMesh(t)
	sites := []string{"a", "b", "c"}
	routers := map[string]*routerFixture{}
	for _, s := range sites {
		routers[s] = m.startRouter(t, "router-"+s, "site="+s)
	}
	// Federation on the first pass: every router finds the ones before it.
	waitForEqual(t, "routers connected to router c", func() []string { return routersConnectedTo(t, m.cpPort, routers["c"].peerID) },
		peerIDStrings(routers["a"].peerID, routers["b"].peerID), 10*time.Second)

	nodes := map[string]*backgroundNode{}
	for _, s := range sites {
		nodes[s] = launchNode(t, m.nodeBin, m.nodeEnv("node-"+s), filepath.Join(m.tmpDir, "node-"+s), append(m.nodeArgs(),
			"--api-token-path", tokenPath(t, "token-"+s),
			"--allow-loopback",
			"--router-selector", "site="+s,
			"--monitor-bootstrap", "500ms",
			"--monitor-interval", "500ms",
		)...)
	}
	for _, s := range sites {
		nodes[s].waitForAPI(t)
		waitForEqual(t, "routers attached by node "+s, func() []string { return attachedRouters(t, nodes[s]) }, peerIDStrings(routers[s].peerID), 10*time.Second)
	}

	// Twenty discovery intervals (200ms) with nothing to do.
	time.Sleep(4 * time.Second)

	for _, s := range sites {
		attached, connected := meshInfo(t, nodes[s])
		if !slices.Equal(attached, peerIDStrings(routers[s].peerID)) {
			t.Errorf("node %s holds %v, want its router %s", s, attached, routers[s].peerID)
		}
		for _, p := range connected {
			for _, other := range sites {
				if other != s && p == nodes[other].peerID.String() {
					t.Errorf("idle node %s is connected to node %s", s, other)
				}
			}
		}
		// Its own node and the two peer routers.
		if got := routers[s].gauge(t, "agentmesh_router_authenticated_peers"); got != 3 {
			t.Errorf("router %s authenticated %v peers, want 3 (its node and two routers)", s, got)
		}
		if t.Failed() {
			t.Logf("node %s connected peers: %v", s, connected)
		}
	}
	if t.Failed() {
		for _, s := range sites {
			out := nodes[s].log()
			if i := strings.LastIndex(out, "[AuthN] Successfully authenticated"); i > 0 {
				t.Logf("--- node %s, last handshake line ---\n%s", s, out[i:min(len(out), i+300)])
			}
		}
	}
}
