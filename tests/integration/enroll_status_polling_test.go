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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/controlplane"
	"github.com/google/agentmesh/internal/identity"
	"github.com/google/agentmesh/internal/node"
	"github.com/google/agentmesh/internal/storage"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestEnrollStatusPollingCollectsBiscuit(t *testing.T) {
	oidcURL, _ := startCustomMockOIDC(t)

	store, err := storage.NewSQLStore("sqlite", filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const adminToken = "test-admin-token"
	opts := controlplane.Options{
		ListenAddr:            "127.0.0.1:0",
		AdminToken:            adminToken,
		OIDCIssuer:            oidcURL,
		AllowedAudiences:      []string{"agentmesh-audience"},
		AutoApproveEnrollment: false,
	}
	srv, err := controlplane.NewServer(opts, store)
	if err != nil {
		t.Fatalf("failed to create control plane: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start control plane: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	baseURL := "http://" + srv.Addr()

	ctx := context.Background()
	cpPriv, _, err := store.GetCurrentKey(ctx)
	if err != nil {
		t.Fatalf("failed to load control plane signing key: %v", err)
	}

	routerHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create mock router host: %v", err)
	}
	t.Cleanup(func() { _ = routerHost.Close() })

	routerBiscuit, err := identity.MintBootstrapBiscuitToken(cpPriv, routerHost.ID(), api.RoleRouter, time.Now().Add(time.Hour), nil, nil)
	if err != nil {
		t.Fatalf("failed to mint router biscuit: %v", err)
	}
	routerHost.SetStreamHandler(api.AuthProtocolID, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		reader := msgio.NewVarintReaderSize(s, 1024*64)
		msg, err := reader.ReadMsg()
		if err != nil {
			return
		}
		defer reader.ReleaseMsg(msg)
		writer := msgio.NewVarintWriter(s)
		resp := &api.AuthResponse{Success: true, Biscuit: routerBiscuit}
		respBytes, _ := proto.Marshal(resp)
		_ = writer.WriteMsg(respBytes)
	})

	routerAddr := routerHost.Addrs()[0].String() + "/p2p/" + routerHost.ID().String()
	if err := store.UpsertRouterLease(ctx, &storage.RouterLease{
		PeerID:      routerHost.ID().String(),
		Addresses:   []string{routerAddr},
		LastRenewal: time.Now(),
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("failed to register mock router: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	adminReqBody := []byte(`{
		"role": "mesh:role:node",
		"ttl_hours": 2,
		"max_usages": 1,
		"description": "enroll status polling"
	}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/admin/bootstrap-tokens", bytes.NewReader(adminReqBody))
	if err != nil {
		t.Fatalf("failed to build bootstrap token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to create bootstrap token: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected status creating token: %s body %s", resp.Status, body)
	}
	var tokenDetails struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenDetails); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("failed to decode token response: %v", err)
	}
	_ = resp.Body.Close()
	if tokenDetails.Token == "" {
		t.Fatal("empty bootstrap token")
	}

	nodeStore, err := node.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create node store: %v", err)
	}
	t.Cleanup(func() { _ = nodeStore.Close() })

	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("failed to generate node key: %v", err)
	}
	samNode, err := node.NewAgentMeshNode(node.Options{
		PrivKey:       priv,
		Store:         nodeStore,
		ListenAddrs:   []string{"/ip4/127.0.0.1/tcp/0"},
		AllowLoopback: true,
		RequiredRole:  api.RoleNode,
	})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	enrollCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := samNode.Start(enrollCtx); err != nil {
		t.Fatalf("failed to start node: %v", err)
	}
	t.Cleanup(func() {
		if samNode.Host != nil {
			_ = samNode.Host.Close()
		}
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- samNode.EnrollBootstrap(enrollCtx, baseURL, tokenDetails.Token)
	}()

	var reqID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		listReq, err := http.NewRequest(http.MethodGet, baseURL+"/admin/enrollments", nil)
		if err != nil {
			t.Fatalf("failed to build enrollments request: %v", err)
		}
		listReq.Header.Set("Authorization", "Bearer "+adminToken)
		listResp, err := client.Do(listReq)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if listResp.StatusCode != http.StatusOK {
			_ = listResp.Body.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		listBody, readErr := io.ReadAll(listResp.Body)
		_ = listResp.Body.Close()
		enrollList := &api.EnrollmentRequestListResponse{}
		if readErr != nil || protojson.Unmarshal(listBody, enrollList) != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if len(enrollList.GetRequests()) == 1 {
			reqID = enrollList.GetRequests()[0].GetId()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reqID == "" {
		t.Fatal("pending enrollment did not appear")
	}

	approveReq, err := http.NewRequest(http.MethodPost, baseURL+"/admin/enrollments/"+reqID+"/approve", nil)
	if err != nil {
		t.Fatalf("failed to build approve request: %v", err)
	}
	approveReq.Header.Set("Authorization", "Bearer "+adminToken)
	approveResp, err := client.Do(approveReq)
	if err != nil {
		t.Fatalf("failed to approve enrollment: %v", err)
	}
	if approveResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(approveResp.Body)
		_ = approveResp.Body.Close()
		t.Fatalf("failed to approve enrollment: %s body %s", approveResp.Status, body)
	}
	_ = approveResp.Body.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("EnrollBootstrap failed: %v", err)
		}
	case <-enrollCtx.Done():
		t.Fatal("timed out waiting for EnrollBootstrap")
	}

	if len(samNode.GetIdentity()) == 0 {
		t.Fatal("expected node to hold a biscuit after approved enrollment polling")
	}
}
