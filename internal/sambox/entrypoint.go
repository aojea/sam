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
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/google/sam/api"
)

// The gateway consumes the node; the agent consumes the mesh through the
// gateway. Those are different surfaces and this file is the boundary between
// them.
//
// A sam-node's sidecar API is local and operator-facing: it can register
// services under the node's identity, drive the raw /sam/<peer>/... egress
// proxy at any peer and service the operator chooses, and read node internals.
// Reaching its Unix socket is itself the credential — withAuth treats arriving
// there as proof of authorization, on the grounds that it is the same bar as
// reading the token file. Piping an agent's bytes to that socket would
// therefore hand every sandbox the node's full local authority, so the
// entrypoint terminates HTTP and forwards only what an agent is supposed to
// have.

// agentMayReach is the entire surface an agent gets on the node. Inference and
// tools, and nothing else.
//
// Discovery is not on the list even though agents need it: it is already
// available through MCP as find_remote_tools and discover_remote_services, so
// exposing /sam/service/discover as well would widen the surface without adding
// a capability. Serving is not on the list at all — what an agent serves is
// declared by the platform in its bundle and by the operator in the node's
// configuration, and the agent's only part is binding its contracted port.
func agentMayReach(path string) bool {
	switch path {
	case "/v1/models", "/v1/chat/completions", "/v1/completions":
		return true
	}
	return path == "/mcp" || strings.HasPrefix(path, "/mcp/")
}

// dialMeshEntrypoint returns a connection serving the agent-facing surface.
func (d *AgentDialer) dialMeshEntrypoint() (net.Conn, error) {
	if d.SidecarSocket == "" {
		return nil, ErrHostUnreachable
	}
	return serveOnPipe(d.entrypointHandler()), nil
}

func (d *AgentDialer) entrypointHandler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = sidecarHost
			r.Out.Host = sidecarHost
			d.assertAgent(r)
		},
		Transport: d.sidecarTransport(),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !agentMayReach(r.URL.Path) {
			http.Error(w, "the mesh entrypoint serves /v1 and /mcp only", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// assertAgent replaces every identity-bearing header with what the gateway
// knows, so an agent cannot claim to be anything by setting them itself.
//
// X-Sam-Biscuit is the mesh datapath credential and X-Sam-Authentication is the
// node's local gate; both are the node's business, not the agent's. X-Sam-Agent
// is the one the gateway does set, and it is always overwritten rather than
// merged: an agent's own value must never survive.
//
// Authorization is deliberately untouched. There it means the destination
// service's credential, which is the agent's to send.
func (d *AgentDialer) assertAgent(r *httputil.ProxyRequest) {
	r.Out.Header.Del(api.HeaderSamBiscuit)
	r.Out.Header.Del(api.HeaderSamAuthentication)

	r.Out.Header.Del(api.HeaderSamAgent)
	if d.AgentID != "" {
		r.Out.Header.Set(api.HeaderSamAgent, d.AgentID)
	}
}
