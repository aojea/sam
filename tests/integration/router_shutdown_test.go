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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
)

// TestRouterGracefulShutdownWithdrawsLease covers a router stopping on
// SIGTERM: its last lease carries --shutdown-lease-ttl, so the control plane
// stops handing it to joiners once that time has passed instead of when the
// lease expires. A router with the ttl off sends nothing and stays listed,
// as does a router that keeps running.
func TestRouterGracefulShutdownWithdrawsLease(t *testing.T) {
	cpBin := buildBinary(t, "./cmd/agentmesh-control-plane")
	routerBin := buildBinary(t, "./cmd/agentmesh-router")

	tmpDir := t.TempDir()
	policyFile := filepath.Join(tmpDir, "policies.yaml")
	writePolicyWithRouter(t, policyFile, "bindings: []\nroles: []\n")

	oidcURL, mintToken := startCustomMockOIDC(t)
	routerJWT := mintToken(map[string]interface{}{
		"sub":    "router-shutdown-test",
		"groups": []string{"routers"},
		"roles":  []string{api.RoleRouter},
	})

	cpPort := getFreePort(t)
	cpCmd := exec.Command(cpBin,
		"--bind-address", fmt.Sprintf("127.0.0.1:%d", cpPort),
		"--admin-token-path", tokenPath(t, testAdminToken),
		"--db-dsn", filepath.Join(tmpDir, "cp.db"),
		"--issuer", oidcURL,
		"--insecure-skip-tls-verify",
	)
	if err := cpCmd.Start(); err != nil {
		t.Fatalf("failed to start control plane: %v", err)
	}
	defer func() { _ = cpCmd.Process.Kill(); _ = cpCmd.Wait() }()
	waitForControlPlane(t, cpPort)
	injectPolicyYAML(t, cpPort, testAdminToken, policyFile)

	// startRouter returns the router's process and peer ID, read from the
	// control plane's listing once the router holds a lease.
	startRouter := func(name, shutdownTTL string, listed []string) (*exec.Cmd, string) {
		t.Helper()
		cmd := exec.Command(routerBin,
			"--control-plane", fmt.Sprintf("http://127.0.0.1:%d", cpPort),
			"--listen", fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", getFreePort(t)),
			"--keys-path", filepath.Join(tmpDir, name+".db"),
			"--allow-loopback",
			"--oidc-token", routerJWT,
			// Long enough that only a withdrawal, never a renewal, changes
			// the active set during the test.
			"--lease-renew-interval", "1m",
			"--shutdown-lease-ttl", shutdownTTL,
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("failed to start %s: %v", name, err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		for _, id := range waitForActiveRouters(t, cpPort, len(listed)+1, 10*time.Second) {
			if !slices.Contains(listed, id) {
				return cmd, id
			}
		}
		t.Fatalf("%s holds no lease of its own", name)
		return nil, ""
	}
	withdrawing, withdrawingID := startRouter("router-withdrawing", "1s", nil)
	silent, silentID := startRouter("router-silent", "0", []string{withdrawingID})
	_, stayingID := startRouter("router-staying", "1s", []string{withdrawingID, silentID})

	// The control plane's lease lasts minutes; the withdrawing router's last
	// lease of one second is what takes it out within the poll below, while
	// the silent router's lease is left as it was.
	for _, cmd := range []*exec.Cmd{withdrawing, silent} {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
	}

	want := []string{silentID, stayingID}
	slices.Sort(want)
	deadline := time.Now().Add(5 * time.Second)
	for {
		routers := fetchActiveRouters(t, cpPort)
		slices.Sort(routers)
		if slices.Equal(routers, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active routers after graceful shutdown: got %v, want %v", routers, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
