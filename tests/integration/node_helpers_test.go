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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/internal/node"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A node under test is observed through what it does, never through what it
// prints: the log is kept for the failure message only. Its identity and
// addresses are known before it starts, because the test chooses the ports
// and the node's key lives in its store, so nothing has to be read back out.

// backgroundNode is a `agentmesh-node run` started by a test.
type backgroundNode struct {
	cmd     *exec.Cmd
	apiAddr string  // host:port the sidecar API listens on
	token   string  // API token, "" for a node running the enrollment sidecar
	dataDir string  // the node's store
	peerID  peer.ID // derived from the key in dataDir before the node started
	p2pAddr string  // a TCP address the node listens on, with /p2p/<id>
	logPath string
	exited  chan error
}

// launchNode starts nodeBin with args plus a --bind-addr and a --listen the
// test chose. Both are appended so they win over (bind) or add to (listen)
// what args carry. The node's key is generated in its store first, so its
// peer ID is known up front. Output goes to logDir/node.log.
func launchNode(t *testing.T, nodeBin string, env []string, logDir string, args ...string) *backgroundNode {
	t.Helper()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	logPath := filepath.Join(logDir, "node.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create node log: %v", err)
	}

	dataDir := nodeDataDir(env, args)
	peerID := ensureNodeKey(t, dataDir)
	apiAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	p2pPort := getFreePort(t)

	fullArgs := append(append([]string{}, args...),
		"--bind-addr", apiAddr,
		"--listen", fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", p2pPort),
	)
	cmd := exec.Command(nodeBin, fullArgs...)
	cmd.Dir = repoRoot(t)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start agentmesh-node: %v", err)
	}
	n := &backgroundNode{
		cmd:     cmd,
		apiAddr: apiAddr,
		token:   nodeToken(t, env, args),
		dataDir: dataDir,
		peerID:  peerID,
		p2pAddr: fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", p2pPort, peerID),
		logPath: logPath,
		exited:  make(chan error, 1),
	}
	go func() {
		n.exited <- cmd.Wait()
		_ = logFile.Close()
	}()
	t.Cleanup(n.kill)
	return n
}

// nodeDataDir resolves the store the node will open, the way the binary
// does: --data-dir, else the user config dir under XDG_CONFIG_HOME or HOME.
func nodeDataDir(env []string, args []string) string {
	for i, a := range args {
		if a == "--data-dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	var home, xdg string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "XDG_CONFIG_HOME="); ok {
			xdg = v
		}
		if v, ok := strings.CutPrefix(e, "HOME="); ok {
			home = v
		}
	}
	if xdg != "" {
		return filepath.Join(xdg, "agentmesh")
	}
	return filepath.Join(home, ".config", "agentmesh")
}

// ensureNodeKey creates the node's key in dataDir if there is none, exactly
// as the node itself would, and returns the peer ID it implies. The store is
// closed again before the node starts: bbolt holds an exclusive lock.
func ensureNodeKey(t *testing.T, dataDir string) peer.ID {
	t.Helper()
	store, err := node.NewStore(dataDir)
	if err != nil {
		t.Fatalf("open node store %s: %v", dataDir, err)
	}
	priv := node.GetOrGenerateKey(store)
	if err := store.Close(); err != nil {
		t.Fatalf("close node store: %v", err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("peer ID from node key: %v", err)
	}
	return id
}

func nodeToken(t *testing.T, env []string, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "--api-token-path" && i+1 < len(args) {
			data, err := os.ReadFile(args[i+1])
			if err != nil {
				t.Fatalf("read api token: %v", err)
			}
			return strings.TrimSpace(string(data))
		}
	}
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "AGENTMESH_API_TOKEN="); ok {
			return v
		}
	}
	return ""
}

// startBackgroundNode is launchNode for a node that enrolls with --jwt
// against routerAddr, with homeDir as its home and the shared test token.
func startBackgroundNode(t *testing.T, nodeBin string, routerAddr string, homeDir string, args ...string) *backgroundNode {
	t.Helper()
	env := append(os.Environ(),
		"HOME="+homeDir,
		"XDG_CONFIG_HOME="+filepath.Join(homeDir, ".config"),
		"AGENTMESH_API_TOKEN=test-token", // per-test overrides use --api-token-path, which wins
	)
	allArgs := append([]string{"run", "--control-plane", routerAddr, "--jwt", "test-jwt", "--allow-loopback"}, args...)
	return launchNode(t, nodeBin, env, homeDir, allArgs...)
}

func (n *backgroundNode) kill() {
	if n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
	}
	<-n.exited
	// Re-arm so a second kill (test cleanup after an explicit one) does not block.
	n.exited <- nil
}

// log is the node's output, for failure messages only.
func (n *backgroundNode) log() string {
	data, err := os.ReadFile(n.logPath)
	if err != nil {
		return fmt.Sprintf("(no log: %v)", err)
	}
	return string(data)
}

// waitForAPI returns the API address once /healthz answers, which the node
// only does after Start succeeded (enrolled, router authenticated), and fails
// if the node exits first or does not come up in time.
func (n *backgroundNode) waitForAPI(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case err := <-n.exited:
			n.exited <- err
			t.Fatalf("agentmesh-node exited (%v) before serving its API.\n--- node.log ---\n%s", err, n.log())
		default:
		}
		resp, err := client.Get("http://" + n.apiAddr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return n.apiAddr
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agentmesh-node did not serve its API at %s in time.\n--- node.log ---\n%s", n.apiAddr, n.log())
	return ""
}

// exitsWithin fails unless the node exits within d; it returns the exit error.
func (n *backgroundNode) exitsWithin(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-n.exited:
		n.exited <- err
		return err
	case <-time.After(d):
		t.Fatalf("agentmesh-node still running after %s.\n--- node.log ---\n%s", d, n.log())
		return nil
	}
}

// staysUp fails if the node exits within d.
func (n *backgroundNode) staysUp(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case err := <-n.exited:
		n.exited <- err
		t.Fatalf("agentmesh-node exited (%v) within %s.\n--- node.log ---\n%s", err, d, n.log())
	case <-time.After(d):
	}
}

// waitForEnrollmentSidecar returns once the node serves the enrollment-only
// MCP surface: a session opened without a token that offers
// get_login_instructions. A node holding an identity refuses token-less
// sessions, so this also tells which of the two sidecars came up.
func (n *backgroundNode) waitForEnrollmentSidecar(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		select {
		case err := <-n.exited:
			n.exited <- err
			t.Fatalf("agentmesh-node exited (%v) before serving its enrollment sidecar.\n--- node.log ---\n%s", err, n.log())
		default:
		}
		tools, err := listMCPTools(n.apiAddr)
		if err == nil {
			for _, name := range tools {
				if name == "get_login_instructions" {
					return
				}
			}
			err = fmt.Errorf("tools %v do not include get_login_instructions", tools)
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agentmesh-node at %s did not serve the enrollment sidecar in time: %v\n--- node.log ---\n%s", n.apiAddr, last, n.log())
}

// listMCPTools opens a token-less MCP session against apiAddr and returns the
// names of the tools it offers.
func listMCPTools(apiAddr string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + apiAddr + "/mcp"}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// waitForAPI polls addr's /healthz for a node the test started itself.
func waitForAPI(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for API at %s", addr)
}
