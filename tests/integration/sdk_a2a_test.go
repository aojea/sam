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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/sam/api"
)

// TestNativeSDKA2A runs the examples written with the official A2A SDKs
// (sdk/js/examples/a2a-agent.ts, a2a-call.ts; sdk/python/examples/a2a_agent.py,
// a2a_call.py) against a real mesh: an agent per language, each an A2A
// server the mesh SDK accepts requests for, and a caller per language that
// reaches the other language's agent by peer ID with the A2A SDK's client
// over the mesh SDK's transport. The caller fetches the agent card, sends a
// message and gets an answer that names the caller the agent's server saw
// as the verified peer. A sam-node's egress proxy fetches the same card.
// A language whose toolchain or A2A SDK is missing is skipped.
func TestNativeSDKA2A(t *testing.T) {
	mesh := startSDKMesh(t)

	var launchers []sdkExampleLauncher
	var cmds []*exec.Cmd
	for _, l := range sdkExampleLaunchers {
		cmd, skip := l.cmd(context.Background(), mesh.root, "a2a-agent")
		if skip == "" {
			skip = a2aSDKMissing(mesh.root, l.name)
		}
		if skip != "" {
			t.Logf("%s SDK skipped: %s", l.name, skip)
			continue
		}
		launchers = append(launchers, l)
		cmds = append(cmds, cmd)
	}
	if len(launchers) == 0 {
		t.Skip("no SDK toolchain with the A2A SDK available; see sdk/README.md")
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
		t.Run(l.name+"-calls-"+target.name, func(t *testing.T) {
			t.Parallel()
			tokenPath := filepath.Join(t.TempDir(), "join-token")
			if err := os.WriteFile(tokenPath, []byte(mintBootstrapToken(t, mesh.baseURL, mesh.adminToken)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := []string{"SAM_CONTROL_PLANE_URL=" + mesh.baseURL, "SAM_STATE_DIR=" + filepath.Join(t.TempDir(), "state"), "SAM_BOOTSTRAP_TOKEN_PATH=" + tokenPath}

			out := runExample(t, mesh.root, l, env, "a2a-call", target.peerID, "hello from "+l.name)
			caller := expectLine(t, out, "on the mesh as ")
			expectLine(t, out, "agent: Echo agent, ")
			// The agent's server saw the verified caller, never a name the
			// caller chose.
			if want := caller + " said: hello from " + l.name; !strings.Contains(out, want) {
				t.Fatalf("%s calling the %s agent: want %q in\n%s", l.name, target.name, want, out)
			}
		})
	}

	// A sam-node reaches the same agent card through its egress proxy, and
	// rewrites the card's URL so an A2A client behind the node can use it.
	for _, a := range agents {
		status, body := egressGet(t, mesh.nodeAPI, "node-token", "/sam/"+a.peerID+"/a2a/agent/.well-known/agent-card.json")
		if status != 200 || !strings.Contains(body, `"Echo agent"`) {
			t.Fatalf("agent card of the %s agent through the node: %d %s", a.name, status, body)
		}
		if !strings.Contains(body, "/sam/"+a.peerID+"/a2a/agent") {
			t.Fatalf("agent card of the %s agent through the node does not name the mesh path: %s", a.name, body)
		}
	}
}

// a2aSDKMissing reports why a language's A2A example cannot run: the A2A
// SDK is a dependency of the examples, not of the packages.
func a2aSDKMissing(root, lang string) string {
	switch lang {
	case "js":
		if _, err := os.Stat(filepath.Join(root, "sdk", "js", "node_modules", "@a2a-js", "sdk")); err != nil {
			return "@a2a-js/sdk is not installed (cd sdk/js && npm ci)"
		}
	case "python":
		python := filepath.Join(root, "sdk", "python", ".venv", "bin", "python")
		if _, err := os.Stat(python); err != nil {
			python = "python3"
		}
		if err := exec.Command(python, "-c", "import a2a.client, uvicorn").Run(); err != nil {
			return "a2a-sdk is not importable (pip install -e 'sdk/python[examples]')"
		}
	}
	return ""
}
