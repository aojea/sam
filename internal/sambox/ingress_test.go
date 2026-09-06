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

package sambox

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestIngressForwardsIntoTheSandbox: what the node delivers reaches the agent
// on its bundle-contracted port, with the service name the operator's config
// added stripped back off so the agent sees the path it serves. The agent did
// nothing to make this happen but listen.
func TestIngressForwardsIntoTheSandbox(t *testing.T) {
	paths := make(chan string, 1)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		_, _ = io.WriteString(w, "served by the agent")
	}))
	defer agent.Close()

	manager := &IngressManager{
		Serves: BundleServes{Name: "code-reviewer", Port: 8080},
		// The sandbox here is an ordinary server, so the contracted port is
		// reached at the test server's address.
		AgentAddr: func(int) string { return strings.TrimPrefix(agent.URL, "http://") },
	}
	t.Cleanup(manager.Close)

	addr, err := manager.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	resp, err := http.Get("http://" + addr + "/code-reviewer/review")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if string(body) != "served by the agent" {
		t.Errorf("body = %q", body)
	}
	if got := <-paths; got != "/review" {
		t.Errorf("the agent saw %q, want the service name stripped", got)
	}
}

// TestIngressRefusesToRouteAnUngrantedName is the property that keeps an
// agent from serving under somebody else's name: only the names the bundle
// contracts have routes, and nothing at runtime can add one.
func TestIngressRefusesToRouteAnUngrantedName(t *testing.T) {
	manager := &IngressManager{
		Serves:      BundleServes{Name: "code-reviewer", Port: 8080},
		AgentSocket: filepath.Join(t.TempDir(), "ingress.sock"),
	}
	t.Cleanup(manager.Close)

	addr, err := manager.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	resp, err := http.Get("http://" + addr + "/never-granted/anything")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Error("the gateway routed a name the bundle never granted")
	}
}

// TestIngressWithNoWayInRefusesRatherThanDiallingItsOwnNamespace is a
// vulnerability regression.
//
// There used to be a fallback: with no reverse channel, deliver to
// 127.0.0.1:<port>. That address is in the gateway's network namespace, which
// in a pod is the pod's -- sam-node's API, the other sidecars, every other
// boundary. So a bundle-contracted port would be delivered to the gateway's
// neighbours rather than the agent. Without a way into the sandbox the
// gateway must not serve at all.
func TestIngressWithNoWayInRefusesRatherThanDiallingItsOwnNamespace(t *testing.T) {
	manager := &IngressManager{
		Serves: BundleServes{Name: "code-reviewer", Port: 8080},
	}
	t.Cleanup(manager.Close)

	if _, err := manager.Start(); err == nil {
		t.Fatal("the gateway agreed to serve a sandbox it has no way into")
	}
}

// TestIngressStopsAnsweringOnClose: a detached sandbox must stop being routed
// to. With the service declared on the node, that means the ingress goes away
// and the node's backend probe withholds the name from discovery.
func TestIngressStopsAnsweringOnClose(t *testing.T) {
	manager := &IngressManager{
		Serves:      BundleServes{Name: "code-reviewer", Port: 8080},
		AgentSocket: filepath.Join(t.TempDir(), "ingress.sock"),
	}
	addr, err := manager.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	manager.Close()

	if _, err := http.Get("http://" + addr + "/code-reviewer/review"); err == nil {
		t.Error("the ingress still answers after Close")
	}
}

func TestSplitServicePath(t *testing.T) {
	tests := []struct {
		path     string
		wantName string
		wantRest string
	}{
		{"/code-reviewer/review", "code-reviewer", "/review"},
		{"/code-reviewer", "code-reviewer", "/"},
		{"/code-reviewer/", "code-reviewer", "/"},
		{"/code-reviewer/a/b", "code-reviewer", "/a/b"},
		{"/", "", "/"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			name, rest := splitServicePath(tc.path)
			if name != tc.wantName || rest != tc.wantRest {
				t.Errorf("splitServicePath(%q) = %q, %q; want %q, %q", tc.path, name, rest, tc.wantName, tc.wantRest)
			}
		})
	}
}
