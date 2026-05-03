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

package registry

// NodeCard holds the self-described capabilities of a SAM node.
// It is published to the DHT so peers can discover each other's skills,
// models and tools without a centralised registry.
type NodeCard struct {
	PeerID    string            `json:"peer_id"`
	Name      string            `json:"name,omitempty"`
	Skills    []string          `json:"skills,omitempty"`
	MCPTools  []string          `json:"mcp_tools,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Signature []byte            `json:"signature,omitempty"`
	Timestamp int64             `json:"timestamp"`
}
