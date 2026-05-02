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

package router

import (
	"io"
	"net/http"
	"strings"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// SetupProxyRoutes attaches the Tier 2 dynamic paths to the local HTTP multiplexer
func SetupProxyRoutes(mux *http.ServeMux, h host.Host) {

	// A. Proxy for Remote MCP Execution
	// Example: GET /nodes/Qm123.../mcp/sse
	mux.HandleFunc("/nodes/{peer_id}/mcp/", func(w http.ResponseWriter, r *http.Request) {
		peerIDStr := r.PathValue("peer_id")
		targetPeer, err := peer.Decode(peerIDStr)
		if err != nil {
			http.Error(w, "Invalid peer ID", http.StatusBadRequest)
			return
		}

		// Strip the local prefix to get the actual path the remote node expects
		// e.g., "/nodes/Qm123/mcp/sse" -> "/sse"
		remotePath := strings.TrimPrefix(r.URL.Path, "/nodes/"+peerIDStr+"/mcp")

		// Open a secure libp2p stream using the MCP protocol ID
		stream, err := h.NewStream(r.Context(), targetPeer, api.MCPProtocolID)
		if err != nil {
			http.Error(w, "Failed to connect to mesh peer", http.StatusBadGateway)
			return
		}
		defer func() {
			_ = stream.Close()
		}()

		// Stream the HTTP request down the libp2p pipe
		r.URL.Path = remotePath
		
		// Note: This is a simple proxy implementation.
		// In a full implementation, you might want to handle headers and body correctly.
		// For now, we write the request to the stream.
		if err := r.Write(stream); err != nil {
			http.Error(w, "Failed to write request to stream", http.StatusInternalServerError)
			return
		}

		// Read the HTTP response from the libp2p stream and write it back to the client
		_, err = io.Copy(w, stream)
		if err != nil {
			// If we already started writing the response, we can't change the status code.
			// But we should log it or handle it.
			return
		}
	})

	// B. Proxy for Remote LLM Inference (OpenAI Compatible)
	// Example: POST /nodes/Qm123.../llm/v1/chat/completions
	mux.HandleFunc("/nodes/{peer_id}/llm/", func(w http.ResponseWriter, r *http.Request) {
		// Identical proxy logic, but potentially using a different libp2p protocol ID 
		// e.g., /sam/inference/1.0.0
		http.Error(w, "LLM proxy not implemented", http.StatusNotImplemented)
	})

	// C. Proxy for A2A Communication
	// Example: GET /nodes/{peer_id}/a2a/stream
	mux.HandleFunc("/nodes/{peer_id}/a2a/stream", func(w http.ResponseWriter, r *http.Request) {
		// Upgrades to WebSockets/SSE and binds to a libp2p A2A stream
		http.Error(w, "A2A stream proxy not implemented", http.StatusNotImplemented)
	})
}
