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

package node

import (
	"context"

	"github.com/libp2p/go-libp2p/core/peer"
)

// authDirectoryRouter runs the credential handshake with a router this node
// just connected to without being attached to it. The DHT client opens such
// connections for a lookup or an announcement, to the routers kademlia puts
// closest to the key; left anonymous, the router closes them at its auth
// deadline a minute later and the next lookup opens them again. Handshaken,
// they are sessions: the router keeps them, counts them, and the node's
// lookups reuse them. The routers a node holds this way are its K attached
// ones plus those its keys land on, which is how a mesh with more routers
// spreads them; a node's own connection manager trims the idle ones first.
// Attached routers handshake on attach, and a connection that arrives while
// that handshake is in flight waits for it (authRouter).
func (n *AgentMeshNode) authDirectoryRouter(ctx context.Context, router peer.ID) {
	if n.isAttached(router) || n.isAuthenticatedAndConnected(router) || !n.isCatalogRouter(router) {
		return
	}
	go func() {
		biscuitBytes, err := n.loadIdentityForAuth()
		if err != nil {
			logger.Debugf("[AuthN] Cannot authenticate to directory router %s without an identity: %v", router, err)
			return
		}
		if err := n.authRouter(ctx, peer.AddrInfo{ID: router}, biscuitBytes); err != nil {
			directoryHandshakesTotal.WithLabelValues("failed").Inc()
			logger.Warnf("[AuthN] Directory router %s did not admit this node; the router will close the connection at its auth deadline: %v", router, err)
			return
		}
		directoryHandshakesTotal.WithLabelValues("ok").Inc()
		logger.Infof("[AuthN] Authenticated to directory router %s", router)
	}()
}

// isCatalogRouter reports whether p is a router the control plane lists.
func (n *AgentMeshNode) isCatalogRouter(p peer.ID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.routerCatalog {
		if c.ID == p {
			return true
		}
	}
	return false
}

// directoryRouters are the routers this node holds an authenticated session
// with for its DHT alone: connected and admitted, not attached.
func (n *AgentMeshNode) directoryRouters() []peer.ID {
	attached := map[peer.ID]bool{}
	n.mu.Lock()
	for r := range n.attached {
		attached[r] = true
	}
	n.mu.Unlock()
	var out []peer.ID
	for _, r := range n.authenticatedRouterIDs() {
		if !attached[r] {
			out = append(out, r)
		}
	}
	return out
}

// DirectoryRouters is how many routers this node holds for its DHT alone.
func (n *AgentMeshNode) DirectoryRouters() int { return len(n.directoryRouters()) }
