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

package controlplane

import (
	"net/http"
	"time"

	"github.com/google/agentmesh/api"
)

// HandleAdminStatus HTTP GET `/admin/status`: the whole mesh as the console
// shows it, protojson of AdminStatusResponse.
func (s *Server) HandleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	routers, err := s.store.GetActiveRouters(ctx)
	if err != nil {
		logger.Errorf("Failed to get active routers: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		logger.Errorf("Failed to list nodes: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	reqs, err := s.store.ListEnrollmentRequests(ctx)
	if err != nil {
		logger.Errorf("Failed to list enrollment requests: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	tokens, err := s.store.ListBootstrapTokens(ctx)
	if err != nil {
		logger.Errorf("Failed to list bootstrap tokens: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	users, err := s.store.ListUsers(ctx)
	if err != nil {
		logger.Errorf("Failed to list users: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	policy, err := s.loadPolicyConfig(ctx)
	if err != nil {
		logger.Errorf("Failed to list policy: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	enrolled := make([]*api.EnrolledNode, 0, len(nodes))
	for i := range nodes {
		enrolled = append(enrolled, enrolledNodeView(&nodes[i], true))
	}

	writeProtoJSON(w, &api.AdminStatusResponse{
		Users:              userViews(users),
		ActiveRouters:      routerLeaseViews(routers),
		EnrolledNodes:      enrolled,
		EnrollmentRequests: enrollmentRequestViews(reqs),
		BootstrapTokens:    bootstrapTokenViews(tokens),
		Policy:             policy,
		NodeCatalog:        s.catalogViewFor(nodes, time.Now()),
	})
}
