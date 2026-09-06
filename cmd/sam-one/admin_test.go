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

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/storage"
	"google.golang.org/protobuf/proto"
)

func newFakeAdminAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/bootstrap-tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm-tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "abcdef123456",
				"token":      "sam-bt-fresh",
				"role":       req["role"],
				"expires_at": "2026-12-31T00:00:00Z",
			})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]storage.BootstrapToken{{
				ID:          "abcdef123456",
				Role:        api.RoleNode,
				MaxUsages:   3,
				UsagesCount: 1,
				Description: "seeded",
				ExpiresAt:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
			}})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/admin/revoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm-tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req api.TokenRevokeRequest
		if err := proto.Unmarshal(body, &req); err != nil || req.PeerId != "12D3KooTestPeer" {
			http.Error(w, "wrong peer", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAdminClient(t *testing.T) {
	ts := newFakeAdminAPI(t)
	c := &adminClient{client: ts.Client(), server: ts.URL, token: "adm-tok"}

	created, err := c.createToken(api.RoleNode, 24, 1, "note")
	if err != nil {
		t.Fatalf("createToken failed: %v", err)
	}
	if created.Token != "sam-bt-fresh" || created.Role != api.RoleNode {
		t.Errorf("unexpected created token: %+v", created)
	}

	list, err := c.listTokens()
	if err != nil {
		t.Fatalf("listTokens failed: %v", err)
	}
	if len(list) != 1 || list[0].Description != "seeded" || list[0].UsagesCount != 1 {
		t.Errorf("unexpected token list: %+v", list)
	}

	if err := c.banPeer("12D3KooTestPeer"); err != nil {
		t.Fatalf("banPeer failed: %v", err)
	}

	bad := &adminClient{client: ts.Client(), server: ts.URL, token: "wrong"}
	if _, err := bad.listTokens(); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error with wrong token, got %v", err)
	}
}

func TestResolveAdminToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte("sam_adm_file\n"), 0o600); err != nil {
		t.Fatalf("failed to write token file: %v", err)
	}

	t.Setenv("SAM_ADMIN_TOKEN", "")
	if got, err := resolveAdminToken("", dir); err != nil || got != "sam_adm_file" {
		t.Errorf("data-dir fallback = %q, %v; want sam_adm_file", got, err)
	}

	t.Setenv("SAM_ADMIN_TOKEN", "sam_adm_env")
	if got, err := resolveAdminToken("", dir); err != nil || got != "sam_adm_env" {
		t.Errorf("env precedence = %q, %v; want sam_adm_env", got, err)
	}
	if got, err := resolveAdminToken("sam_adm_flag", dir); err != nil || got != "sam_adm_flag" {
		t.Errorf("flag precedence = %q, %v; want sam_adm_flag", got, err)
	}

	t.Setenv("SAM_ADMIN_TOKEN", "")
	if _, err := resolveAdminToken("", t.TempDir()); err == nil {
		t.Error("expected an error when no admin token source exists")
	}
}
