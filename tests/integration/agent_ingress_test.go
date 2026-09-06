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
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/sam/api"
	"github.com/google/sam/internal/sambox"
)

// TestAgentIngressCUJ has a sandboxed agent serve the mesh, which is the
// direction the boundary was not built for at first.
//
// An agent serves exactly one thing: itself, over A2A. The bundle contracts
// the name and the sandbox port; the node's configuration declares the
// a2a:// service with the gateway's ingress as its backend; capabilities live
// on the agent's own card. The agent here is a stock a2a-go server that never
// learns the mesh exists, and the consumer is a stock a2a-go client that
// bootstraps from the card the mesh regenerates.
func TestAgentIngressCUJ(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	_, hubAddr := startMockRouter(t)

	homeA := t.TempDir()
	homeB := t.TempDir()
	apiToken := "test-token"

	sockDir, err := os.MkdirTemp("", "ingress")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	nodeSocket := filepath.Join(sockDir, "node.sock")
	agentSocket := filepath.Join(sockDir, "agent.sock")

	// The agent: a stock A2A server inside its sandbox, bound to its
	// contracted port. Its card names an address only it can dial, which the
	// mesh must replace on the consumer side.
	echo := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("echo from the sandbox")), nil)
		}
	})
	agentCard := &a2a.AgentCard{
		Name:        "code-reviewer",
		Description: "sandboxed a2a agent",
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			{URL: "http://127.0.0.1:8080", ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: "1.0"},
		},
	}
	agentMux := http.NewServeMux()
	agentMux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(agentCard))
	agentMux.Handle("/", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(echo)))
	agent := httptest.NewServer(agentMux)
	defer agent.Close()
	agentHost := strings.TrimPrefix(agent.URL, "http://")

	// The gateway: routes the bundle-contracted name to the contracted port,
	// from startup. Nothing announces anything.
	ingress := &sambox.IngressManager{
		Serves:    sambox.BundleServes{Name: "code-reviewer", Port: 8080},
		AgentAddr: func(int) string { return agentHost },
	}
	t.Cleanup(ingress.Close)
	ingressAddr, err := ingress.Start()
	if err != nil {
		t.Fatalf("ingress.Start: %v", err)
	}

	// Node B declares the sandbox's a2a service up front, backed by the
	// gateway. Advertisement is gated on the card probe passing through it.
	cfgPath := filepath.Join(homeB, "node-config.yaml")
	cfg := "version: \"v1alpha1\"\nservices:\n" +
		"  - type: \"a2a\"\n    name: \"code-reviewer\"\n" +
		"    description: \"served by an agent behind the gateway\"\n" +
		"    target_url: \"http://" + ingressAddr + "/code-reviewer\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing node config: %v", err)
	}

	_ = startBackgroundNode(t, nodeBin, hubAddr, homeA,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
	)
	// Node B hosts the agent, and is the one that advertises its service.
	_ = startBackgroundNode(t, nodeBin, hubAddr, homeB,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
		"--socket-path", nodeSocket,
		"--config", cfgPath,
	)

	apiAddrA := waitForMCPAddr(t, filepath.Join(homeA, "node.log"))
	apiAddrB := waitForMCPAddr(t, filepath.Join(homeB, "node.log"))
	waitForAPI(t, apiAddrA)
	waitForAPI(t, apiAddrB)

	addrB := waitForPeerInfoInLog(t, filepath.Join(homeB, "node.log"))
	peerB := extractPeerID(addrB)
	connectPeer(t, apiAddrA, addrB)
	waitForDHTPeers(t, apiAddrB)

	startBoundaryServing(t, agentSocket, nodeSocket, "reviewer-7.prod.acme.example")
	client := boundaryClient(t, agentSocket)

	t.Run("the agent has no serving surface at all", func(t *testing.T) {
		resp, err := client.Post("http://"+api.MeshEntrypointHost+"/ingress",
			"application/json", strings.NewReader(`{"name":"payments","type":"mcp","port":9000}`))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %s, want 403", resp.Status)
		}
	})

	t.Run("a stock a2a client reaches the agent through the mesh", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Bootstrap from the card the mesh regenerates at the consumer's
		// edge: the agent's own 127.0.0.1 URL must have been replaced.
		meshBase := "http://" + apiAddrA + "/sam/" + peerB + "/a2a/code-reviewer"
		resolver := agentcard.NewResolver(meshHTTPClient(apiToken, nil))
		var card *a2a.AgentCard
		deadline := time.Now().Add(30 * time.Second)
		for {
			card, err = resolver.Resolve(ctx, meshBase)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout resolving the agent card through the mesh: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != meshBase {
			t.Errorf("card interfaces not regenerated for the mesh: %+v", card.SupportedInterfaces)
		}

		a2aClient, err := a2aclient.NewFromCard(ctx, card,
			a2aclient.WithJSONRPCTransport(meshHTTPClient(apiToken, nil)))
		if err != nil {
			t.Fatalf("stock client rejected the regenerated card: %v", err)
		}
		req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}
		var result a2a.SendMessageResult
		deadline = time.Now().Add(30 * time.Second)
		for {
			result, err = a2aClient.SendMessage(ctx, req)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("message/send through the sandbox failed: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		reply, ok := result.(*a2a.Message)
		if !ok {
			t.Fatalf("send result = %T, want *a2a.Message", result)
		}
		if len(reply.Parts) == 0 || reply.Parts[0].Text() != "echo from the sandbox" {
			t.Fatalf("unexpected reply: %+v", reply)
		}
	})
}

// startBoundaryServing runs a boundary for an agent that also serves.
func startBoundaryServing(t *testing.T, agentSocket, nodeSocket, agentID string) {
	t.Helper()

	egress, err := sambox.NewEgressPolicy(nil)
	if err != nil {
		t.Fatalf("NewEgressPolicy: %v", err)
	}
	listener, err := sambox.ListenSandboxSocket(agentSocket)
	if err != nil {
		t.Fatalf("ListenSandboxSocket: %v", err)
	}

	server := &sambox.ConnectServer{
		Dialer: &sambox.AgentDialer{
			Router:        &sambox.Router{Egress: egress},
			SidecarSocket: nodeSocket,
			AgentID:       agentID,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(ctx, listener); err != nil {
			t.Errorf("boundary: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}
