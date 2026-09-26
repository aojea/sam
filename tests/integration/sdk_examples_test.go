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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/sam/api"
)

// sdkExampleLauncher starts one of the example programs the SDK READMEs and
// the Native SDKs guide embed (sdk/js/examples, sdk/python/examples), so
// the documented programs are the ones exercised here.
type sdkExampleLauncher struct {
	name string
	cmd  func(ctx context.Context, root, example string, args ...string) (*exec.Cmd, string)
}

var sdkExampleLaunchers = []sdkExampleLauncher{
	{
		name: "js",
		cmd: func(ctx context.Context, root, example string, args ...string) (*exec.Cmd, string) {
			entry := filepath.Join(root, "sdk", "js", "build", "examples", example+".js")
			if _, err := exec.LookPath("node"); err != nil {
				return nil, "node is not installed"
			}
			if _, err := os.Stat(entry); err != nil {
				return nil, "sdk/js examples are not built (cd sdk/js && npm ci && npm run build && npm run examples)"
			}
			return exec.CommandContext(ctx, "node", append([]string{entry}, args...)...), ""
		},
	},
	{
		name: "python",
		cmd: func(ctx context.Context, root, example string, args ...string) (*exec.Cmd, string) {
			python := filepath.Join(root, "sdk", "python", ".venv", "bin", "python")
			if _, err := os.Stat(python); err != nil {
				var lookErr error
				if python, lookErr = exec.LookPath("python3"); lookErr != nil {
					return nil, "python3 is not installed"
				}
			}
			if err := exec.Command(python, "-c", "import agent_mesh.session").Run(); err != nil {
				return nil, "agent_mesh is not importable with libp2p (pip install -e sdk/python)"
			}
			// Python names its files with underscores: a2a-agent is a2a_agent.py.
			entry := filepath.Join(root, "sdk", "python", "examples", strings.ReplaceAll(example, "-", "_")+".py")
			return exec.CommandContext(ctx, python, append([]string{entry}, args...)...), ""
		},
	},
}

// sdkExampleAgent is a running agent example.
type sdkExampleAgent struct {
	name   string
	peerID string
}

var acceptingLine = regexp.MustCompile(`^accepting (\S+) as (\S+)$`)

// TestNativeSDKExamples runs the programs the SDK READMEs and the Native
// SDKs guide embed, unchanged, against a real mesh, configured as the
// testnet canaries are (.github/k8s/sam-sdk-canary-template.yaml). Each
// SDK's agent example enrolls with an OIDC token through SAM_JWT_PATH, the
// way a Kubernetes workload does, and accepts A2A requests for a2a://agent,
// publishing nothing. Each SDK's call example enrolls with a bootstrap
// token, reaches the sam-node's mcp://calc by name, and the other language's
// agent (its own when the other toolchain is missing) by peer ID; the SDK
// finds the path through the router by itself. The first run spends the
// token, the later runs resume from the state directory without one.
func TestNativeSDKExamples(t *testing.T) {
	mesh := startSDKMesh(t)

	var launchers []sdkExampleLauncher
	var cmds []*exec.Cmd
	for _, l := range sdkExampleLaunchers {
		cmd, skip := l.cmd(context.Background(), mesh.root, "agent")
		if skip != "" {
			t.Logf("%s SDK skipped: %s", l.name, skip)
			continue
		}
		launchers = append(launchers, l)
		cmds = append(cmds, cmd)
	}
	if len(launchers) == 0 {
		t.Skip("no SDK toolchain available; see sdk/README.md")
	}
	agents := make([]*sdkExampleAgent, len(launchers))
	errs := make([]error, len(launchers))
	var wg sync.WaitGroup
	for i := range launchers {
		jwtPath := filepath.Join(t.TempDir(), "sam-token")
		jwt := mesh.mintToken(map[string]interface{}{"sub": "mock-user", "roles": []string{api.RoleNode}})
		if err := os.WriteFile(jwtPath, []byte(jwt+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmds[i].Env = append(os.Environ(),
			"SAM_CONTROL_PLANE_URL="+mesh.baseURL,
			"SAM_JWT_PATH="+jwtPath,
			"SAM_STATE_DIR="+filepath.Join(t.TempDir(), "state"),
		)
		cmds[i].Dir = mesh.root
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agents[i], errs[i] = startExampleAgent(t, launchers[i].name, cmds[i])
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range agents {
		waitForPeerOnRouter(t, mesh.cpPort, mesh.adminToken, a.peerID, 5*time.Second)
	}

	// The agent a caller of one language targets: the other language's, as
	// the testnet probes do, or its own when it is the only one present.
	peerAgent := func(name string) *sdkExampleAgent {
		for _, a := range agents {
			if a.name != name {
				return a
			}
		}
		return agents[0]
	}

	for _, l := range launchers {
		l := l
		target := peerAgent(l.name)
		t.Run(l.name+"-calls", func(t *testing.T) {
			t.Parallel()
			stateDir := filepath.Join(t.TempDir(), "state")
			tokenPath := filepath.Join(t.TempDir(), "join-token")
			if err := os.WriteFile(tokenPath, []byte(mintBootstrapToken(t, mesh.baseURL, mesh.adminToken)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			withToken := []string{"SAM_CONTROL_PLANE_URL=" + mesh.baseURL, "SAM_STATE_DIR=" + stateDir, "SAM_BOOTSTRAP_TOKEN_PATH=" + tokenPath}
			withoutToken := withToken[:2]

			// The first run spends the token: the sam-node's calc over MCP,
			// found by name.
			out := runExample(t, mesh, l, withToken, "call", "mcp://calc", "add", `{"a": 1, "b": 2}`)
			caller := expectLine(t, out, "on the mesh as ")
			expectLine(t, out, "mcp://calc is served by "+mesh.samNode.peerID.String())
			expectLine(t, out, "tools: add")
			expectLine(t, out, "fake-result:add")

			// The state directory now holds the identity and credential; the
			// token file is gone and not needed. The agent, by peer ID: no
			// lookup, the path goes through the router. The agent sees the
			// verified caller, never the biscuit.
			if err := os.Remove(tokenPath); err != nil {
				t.Fatal(err)
			}
			out = runExample(t, mesh, l, withoutToken, "call", target.peerID, "a2a://agent", "/card")
			if got := expectLine(t, out, "on the mesh as "); got != caller {
				t.Fatalf("second run joined as %s, want the identity of the first run %s", got, caller)
			}
			if got := expectLine(t, out, "a2a://agent is served by "); got != target.peerID {
				t.Fatalf("a2a://agent is served by %s, want the %s agent example %s", got, target.name, target.peerID)
			}
			card := expectLine(t, out, "200 ")
			if !strings.Contains(card, `"path": "/card"`) && !strings.Contains(card, `"path":"/card"`) {
				t.Fatalf("a2a card %q does not echo the path", card)
			}
			if !strings.Contains(card, caller) {
				t.Fatalf("a2a card %q does not name the verified caller %s", card, caller)
			}

			// The state directory is the same layout in every implementation:
			// the other SDK's call example resumes this identity from it, and
			// so does a sam-node after `state import`. Each proves it by
			// reaching the agent as the same peer.
			for _, other := range launchers {
				if other.name == l.name {
					continue
				}
				out := runExample(t, mesh, other, withoutToken, "call", target.peerID, "a2a://agent", "/card")
				if got := expectLine(t, out, "on the mesh as "); got != caller {
					t.Fatalf("%s resumed %s's state directory as %s, want %s", other.name, l.name, got, caller)
				}
				if card := expectLine(t, out, "200 "); !strings.Contains(card, caller) {
					t.Fatalf("a2a card %q through %s does not name the verified caller %s", card, other.name, caller)
				}
			}
			importedNode := importStateIntoNode(t, mesh, stateDir)
			if importedNode.peerID.String() != caller {
				t.Fatalf("sam-node imported %s's state directory as %s, want %s", l.name, importedNode.peerID, caller)
			}
			importedAPI := importedNode.waitForAPI(t)
			waitForPeerOnRouter(t, mesh.cpPort, mesh.adminToken, caller, 10*time.Second)
			// The node's A2A egress path reaches the agent by peer ID, as the
			// testnet's node probe does.
			status, body := egressGet(t, importedAPI, "imported-token", "/sam/"+target.peerID+"/a2a/agent/card")
			if status != 200 || !strings.Contains(body, `"caller"`) || !strings.Contains(body, caller) {
				t.Fatalf("a2a card through the node: %d %s", status, body)
			}
		})
	}
}

// importStateIntoNode runs `sam-node state import` on a fresh data directory
// and starts a node from it, so the node runs as the member the directory
// holds. The import refuses a token: the identity is already enrolled.
func importStateIntoNode(t *testing.T, mesh *sdkMesh, stateDir string) *backgroundNode {
	t.Helper()
	nodeBin := buildBinary(t, "./cmd/sam-node")
	nodeHome := filepath.Join(t.TempDir(), "imported")
	dataDir := filepath.Join(nodeHome, "data")
	importCmd := exec.Command(nodeBin, "state", "import", stateDir, "--data-dir", dataDir)
	importCmd.Dir = mesh.root
	if out, err := importCmd.CombinedOutput(); err != nil {
		t.Fatalf("sam-node state import: %v\n%s", err, out)
	}
	return launchNode(t, nodeBin,
		append(os.Environ(), "HOME="+nodeHome, "XDG_CONFIG_HOME="+filepath.Join(nodeHome, ".config")),
		nodeHome, "run",
		"--data-dir", dataDir,
		"--control-plane", mesh.baseURL,
		"--allow-loopback",
		"--api-token-path", tokenPath(t, "imported-token"),
		"--discovery-interval", "100ms",
		"--log-level", "debug",
	)
}

// startExampleAgent starts a configured agent example and waits for its
// "accepting ... as <peer>" line. Safe to call from several goroutines at
// once, so it reports failures instead of ending the test.
func startExampleAgent(t *testing.T, name string, cmd *exec.Cmd) (*sdkExampleAgent, error) {
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s agent example: %w", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("%s agent example stderr:\n%s", name, stderr.String())
		}
	})

	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if m := acceptingLine.FindStringSubmatch(scanner.Text()); m != nil {
				lines <- scanner.Text()
				return
			}
		}
		close(lines)
	}()
	select {
	case line, ok := <-lines:
		if !ok {
			return nil, fmt.Errorf("%s agent example exited before accepting\nstderr:\n%s", name, stderr.String())
		}
		m := acceptingLine.FindStringSubmatch(line)
		if m[1] != "a2a://agent" {
			return nil, fmt.Errorf("%s agent example accepts %q, want a2a://agent", name, m[1])
		}
		return &sdkExampleAgent{name: name, peerID: m[2]}, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("%s agent example did not report accepting within 30s\nstderr:\n%s", name, stderr.String())
	}
}

// runExample runs an example that exits on its own to completion and returns its stdout.
func runExample(t *testing.T, mesh *sdkMesh, l sdkExampleLauncher, env []string, example string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, skip := l.cmd(ctx, mesh.root, example, args...)
	if skip != "" {
		t.Fatalf("%s: %s", l.name, skip)
	}
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = mesh.root
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s %v failed: %v\nstdout:\n%s\nstderr:\n%s", l.name, example, args, err, stdout, stderr.String())
	}
	return string(stdout)
}

// expectLine returns the rest of the first stdout line starting with prefix.
func expectLine(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("no line starting with %q in output:\n%s", prefix, out)
	return ""
}
