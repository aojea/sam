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
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/internal/bench"
	"github.com/google/agentmesh/internal/standalone"
)

// TestMemberBurstJoinsConcurrently pins the join path under concurrency, the
// part of a new member's journey nothing else in the suite exercises: ten
// nodes enroll from one bootstrap token at the same instant, each
// authenticates to the router and calls a service another node advertises,
// and all ten complete. It is the integration tier of
// tests/scale/member-journey.sh, which takes the same journey at hundreds of
// members against a testnet; this one guards the enrollment race and the
// tool, not the latency.
func TestMemberBurstJoinsConcurrently(t *testing.T) {
	const members = 10
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("standalone.New: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("standalone start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	controlPlane := "http://" + srv.Addr()
	_, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	waitForActiveRouters(t, port, 1, 10*time.Second)
	joinToken := tokenPath(t, srv.JoinToken())

	// The service the members call: a real MCP server, which the provider's
	// advertisement probe needs before it publishes the record the members
	// will look up.
	backend := httptest.NewServer(newBoundaryMCPHandler(t))
	defer backend.Close()

	nodeBin := buildBinary(t, "./cmd/agentmesh-node")
	providerDir := t.TempDir()
	provider := launchNode(t, nodeBin, os.Environ(), providerDir, "run",
		"--control-plane", controlPlane,
		"--data-dir", providerDir,
		"--bootstrap-token-path", joinToken,
		"--api-token-path", tokenPath(t, "test-token"),
		"--allow-loopback",
		"--config", writeNodeConfig(t, providerDir, nil, svcDecl{Type: "mcp", Name: "everything", TargetURL: backend.URL}),
	)
	provider.waitForAPI(t)

	// A short path: a member's socket lives under its directory, and a Unix
	// socket path has a hard limit.
	fleet, err := os.MkdirTemp("", "burst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fleet) })

	report, err := bench.RunJoin(ctx, bench.JoinOptions{
		NodeBin:            nodeBin,
		Count:              members,
		Dir:                filepath.Join(fleet, "members"),
		ControlPlane:       controlPlane,
		BootstrapTokenPath: joinToken,
		Service:            "everything",
		ReadyTimeout:       30 * time.Second,
		CallTimeout:        30 * time.Second,
		MetricsBasePort:    reservePortBlock(t, members),
		NodeArgs:           []string{"--allow-loopback"},
		Log:                testLogWriter{t},
	})
	if err != nil {
		t.Fatalf("RunJoin: %v", err)
	}

	if report.Completed != members || report.Failed != 0 {
		for _, m := range report.Members {
			if m.Error == "" {
				continue
			}
			t.Errorf("member %d: %s\n%s", m.Index, m.Error, logTail(filepath.Join(m.Dir, "node.log")))
		}
		t.Fatalf("completed %d of %d, failed %d: %v\nprovider log:\n%s", report.Completed, members, report.Failed, report.Errors, logTail(provider.logPath))
	}
	if report.ReadyMs.Count != members || report.JourneyMs.Count != members {
		t.Errorf("ready/journey samples = %d/%d, want %d each", report.ReadyMs.Count, report.JourneyMs.Count, members)
	}
	for _, m := range report.Members {
		if m.Provider != provider.peerID.String() {
			t.Errorf("member %d reached %q, want the provider %s", m.Index, m.Provider, provider.peerID)
		}
	}

	// Every member is a distinct enrollment the control plane holds: one
	// token, consumed once per member, with no row lost to the race.
	status := fetchAdminStatus(t, port, srv.AdminToken())
	enrolled := 0
	for _, n := range status.GetEnrolledNodes() {
		if n.GetPeerId() != provider.peerID.String() && !strings.EqualFold(n.GetPeerId(), srv.PeerID()) {
			enrolled++
		}
	}
	if enrolled < members {
		t.Errorf("control plane holds %d member enrollments, want %d", enrolled, members)
	}
	t.Logf("%d members: ready p50 %.0f ms p95 %.0f ms; journey p50 %.0f ms p95 %.0f ms max %.0f ms; %d call attempts",
		members, report.ReadyMs.P50, report.ReadyMs.P95, report.JourneyMs.P50, report.JourneyMs.P95, report.JourneyMs.Max, report.CallAttempts)
}

// reservePortBlock finds n consecutive free loopback ports in the reserved
// range and returns the first.
func reservePortBlock(t *testing.T, n int) int {
	t.Helper()
	for range 50 {
		base := getFreePort(t)
		free := true
		for i := 1; i < n; i++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				free = false
				break
			}
			_ = l.Close()
		}
		if free {
			return base
		}
	}
	t.Fatalf("no run of %d free ports", n)
	return 0
}

// logTail returns the last part of a log file, for a failure message.
func logTail(path string) string {
	data, err := os.ReadFile(path) // #nosec G304 -- a log the test wrote
	if err != nil {
		return err.Error()
	}
	if len(data) > 3000 {
		data = data[len(data)-3000:]
	}
	return string(data)
}

// testLogWriter routes a tool's progress lines into the test log.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
