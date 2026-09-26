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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/sam/api"
)

const sdkCanaryTemplate = ".github/k8s/sam-sdk-canary-template.yaml"

// canaryVerdict is one line the canary script prints per call.
type canaryVerdict struct {
	Canary    string `json:"canary"`
	OK        bool   `json:"ok"`
	AgentPeer string `json:"agent_peer"`
	CallS     int    `json:"call_s"`
	Error     string `json:"error"`
}

// TestSDKCanaryScript runs the testnet's SDK canary script, taken from
// .github/k8s/sam-sdk-canary-template.yaml, the way its pod runs it: an
// agent example in one language writes its output to the directory the pod
// shares, and the script calls it with the other language's A2A example,
// both enrolled with the same projected token, the caller resuming its
// identity from SAM_STATE_DIR on the second call. The script reports each
// call as one JSON line and keeps the readiness file while the last call
// succeeded; a failed call is reported by the line that names the error.
func TestSDKCanaryScript(t *testing.T) {
	script := canaryScript(t)

	t.Run("a failed call is reported by the line naming the error", func(t *testing.T) {
		for _, tc := range []struct{ name, output, want string }{
			{"node", nodeCrash, "NoValidAddressesError: The dial request has no valid addresses for peer: 12D3KooWfake"},
			{"python", pythonTraceback, "ConnectionError: cannot reach 12D3KooWfake:"},
		} {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "agent.log"), []byte("accepting a2a://agent as 12D3KooWfake\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			call := filepath.Join(dir, "call.sh")
			if err := os.WriteFile(call, []byte("#!/bin/sh\ncat <<'EOF' >&2\n"+tc.output+"EOF\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			verdicts := runCanaryScript(t, script, dir, tc.name+"-fails", call, nil)
			v := <-verdicts
			if v.OK || v.Error != tc.want || v.AgentPeer != "12D3KooWfake" {
				t.Fatalf("%s failure reported as %+v, want error %q", tc.name, v, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "reachable")); err == nil {
				t.Fatalf("%s: the readiness file is present after a failed call", tc.name)
			}
		}
	})

	root := repoRoot(t)
	var launchers []sdkExampleLauncher
	for _, l := range sdkExampleLaunchers {
		_, skip := l.cmd(context.Background(), root, "a2a-agent")
		if skip == "" {
			skip = a2aSDKMissing(root, l.name)
		}
		if skip != "" {
			t.Logf("%s SDK skipped: %s", l.name, skip)
			continue
		}
		launchers = append(launchers, l)
	}
	if len(launchers) == 0 {
		t.Skip("no SDK toolchain with the A2A SDK available; see sdk/README.md")
	}
	mesh := startSDKMesh(t)
	for i, agent := range launchers {
		caller := launchers[(i+1)%len(launchers)]
		t.Run(caller.name+"-calls-"+agent.name+"-agent", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			jwtPath := filepath.Join(dir, "sam-token")
			jwt := mesh.mintToken(map[string]interface{}{"sub": "mock-user", "roles": []string{api.RoleNode}})
			if err := os.WriteFile(jwtPath, []byte(jwt+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := []string{
				"SAM_CONTROL_PLANE_URL=" + mesh.baseURL,
				"SAM_INSECURE_CONTROL_PLANE=true",
				"SAM_JWT_PATH=" + jwtPath,
				"PYTHONUNBUFFERED=1",
			}

			// The agent's output is the log the caller reads the peer ID from.
			logFile, err := os.Create(filepath.Join(dir, "agent.log"))
			if err != nil {
				t.Fatal(err)
			}
			agentCmd, _ := agent.cmd(context.Background(), mesh.root, "a2a-agent")
			agentCmd.Env = append(os.Environ(), append(env, "SAM_STATE_DIR="+filepath.Join(dir, "agent"))...)
			agentCmd.Dir = mesh.root
			agentCmd.Stdout, agentCmd.Stderr = logFile, logFile
			if err := agentCmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = agentCmd.Process.Kill()
				_ = agentCmd.Wait()
				_ = logFile.Close()
				if t.Failed() {
					log, _ := os.ReadFile(filepath.Join(dir, "agent.log"))
					t.Logf("%s agent log:\n%s", agent.name, log)
				}
			})

			callCmd, _ := caller.cmd(context.Background(), mesh.root, "a2a-call")
			verdicts := runCanaryScript(t, script, dir, caller.name+"-calls-"+agent.name+"-agent", strings.Join(callCmd.Args, " "),
				append(env, "SAM_STATE_DIR="+filepath.Join(dir, "caller"), "INTERVAL=1"))
			for i := 0; i < 2; i++ {
				v := <-verdicts
				if !v.OK || v.Error != "" || v.AgentPeer == "" {
					t.Fatalf("call %d: %+v", i+1, v)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "reachable")); err != nil {
				t.Fatalf("the readiness file is missing after two successful calls: %v", err)
			}
		})
	}
}

// canaryScript returns the path of canary.sh as the template's ConfigMap carries it.
func canaryScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), sdkCanaryTemplate))
	if err != nil {
		t.Fatal(err)
	}
	var script []string
	inScript := false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.TrimSpace(line) == "canary.sh: |":
			inScript = true
		case inScript && (line == "" || strings.HasPrefix(line, "    ")):
			script = append(script, strings.TrimPrefix(line, "    "))
		case inScript:
			inScript = false
		}
	}
	if len(script) == 0 {
		t.Fatalf("no canary.sh in %s", sdkCanaryTemplate)
	}
	path := filepath.Join(t.TempDir(), "canary.sh")
	if err := os.WriteFile(path, []byte(strings.Join(script, "\n")), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runCanaryScript starts the canary script as the pod does, `sh canary.sh`
// with CANARY and CALL in the environment, and streams its verdict lines.
// The script loops forever; it is killed with its children at cleanup.
func runCanaryScript(t *testing.T, script, dir, canary, call string, env []string) <-chan canaryVerdict {
	t.Helper()
	cmd := exec.Command("sh", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append(env, "SAM_CANARY_DIR="+dir, "CANARY="+canary, "CALL="+call)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	verdicts := make(chan canaryVerdict)
	go func() {
		defer close(verdicts)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var v canaryVerdict
			if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
				t.Errorf("canary line %q is not JSON: %v", scanner.Text(), err)
				return
			}
			verdicts <- v
		}
	}()
	out := make(chan canaryVerdict)
	go func() {
		defer close(out)
		for {
			select {
			case v, ok := <-verdicts:
				if !ok {
					return
				}
				out <- v
			case <-time.After(60 * time.Second):
				t.Errorf("%s: no verdict within 60s", canary)
				return
			}
		}
	}()
	return out
}

const nodeCrash = `file:///app/node_modules/libp2p/dist/src/connection-manager/dial-queue.js:398
            throw new NoValidAddressesError(` + "`" + `The dial request has no valid addresses for peer: ${peerId}` + "`" + `);
                  ^

NoValidAddressesError: The dial request has no valid addresses for peer: 12D3KooWfake
    at DialQueue.calculateMultiaddrs (file:///app/node_modules/libp2p/dist/src/connection-manager/dial-queue.js:398:19)
    at async Job.run (file:///app/node_modules/@libp2p/utils/dist/src/queue/job.js:62:28)

Node.js v22.23.3
`

const pythonTraceback = `Traceback (most recent call last):
  File "/app/examples/a2a_call.py", line 61, in <module>
    trio.run(main)
  File "/app/src/agent_mesh/session.py", line 233, in connect
    raise ConnectionError(f"cannot reach {target}:\n  " + "\n  ".join(failures))
ConnectionError: cannot reach 12D3KooWfake:
  via router 12D3KooWrouter: NO_RESERVATION
`
