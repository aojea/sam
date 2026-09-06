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
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An agent that serves never says so to anyone. The node's exposed services
// are declared in the node's own configuration, where the operator names this
// gateway's ingress address as the backend; the bundle contracts which names
// the agent serves and on which sandbox port (like $PORT on a serverless
// runtime). Nothing at runtime can add a name to the mesh, and there is no
// agent-facing surface at all: the agent just binds its contracted port.
//
// Readiness is implicit. Until the agent listens, forwarding fails at the
// sandbox's reverse channel, so the node's backend probe fails and the name
// is withheld from discovery; when the agent binds the port everything
// converges, and when the sandbox goes away the probe fails again. Dynamic
// agent behaviour beyond that -- capabilities, negotiation, reconfiguration --
// belongs to the protocol served over the name (A2A, MCP), not to the mesh.

// IngressManager forwards what the mesh delivers to the ports the platform
// contracted the agent to serve.
type IngressManager struct {
	// ListenAddr is where this gateway's ingress listens, e.g.
	// "127.0.0.1:7080". It must be stable: the node's configuration names it
	// as the declared services' backend. Empty picks an ephemeral port,
	// which only tests can meaningfully consume via Addr.
	ListenAddr string

	// Serves is the bundle's contract: the agent's a2a service name and the
	// sandbox port it binds. An agent serves at most itself; tools and models
	// are operator workloads, not agent ingress.
	Serves BundleServes

	// AgentSocket is the sandbox's reverse channel: a Unix socket nano-init
	// listens on from inside the sandbox. It is how an isolated agent is
	// reached at all, because every sandbox has a network namespace of its own
	// and the gateway's 127.0.0.1 is therefore not the agent's. A pathname
	// socket crosses that boundary for the same reason the egress one does: it
	// is a filesystem object, and network namespaces do not apply to it.
	//
	// Empty means the agent shares this process's network namespace and can be
	// dialled directly, which is true of no sandboxed profile.
	AgentSocket string

	// AgentAddr resolves where the agent listens inside its sandbox. Setting it
	// overrides both of the above, which is how tests point the forwarder at a
	// server of their own.
	AgentAddr func(port int) string

	mu       sync.Mutex
	listener net.Listener
	routes   map[string]int // service name -> port inside the sandbox
}

// Start validates that the sandbox can be reached, builds the routes the
// bundle contracts, and serves the ingress. It returns the bound address,
// which is what the node's configuration must name as the services' backend.
func (m *IngressManager) Start() (string, error) {
	if err := m.reachable(); err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.listener != nil {
		return m.listener.Addr().String(), nil
	}
	m.routes = map[string]int{m.Serves.Name: m.Serves.Port}
	log.Printf("sambox: serving a2a://%s from the sandbox's port %d", m.Serves.Name, m.Serves.Port)

	addr := m.ListenAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	m.listener = listener

	server := &http.Server{Handler: m.forwarder(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = server.Serve(listener)
	}()
	return listener.Addr().String(), nil
}

// forwarder carries what the node delivers into the sandbox, stripping the
// service name the gateway added so the agent sees the path it published.
func (m *IngressManager) forwarder() http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: m.AgentTransport(),
		Rewrite: func(r *httputil.ProxyRequest) {
			name, rest := splitServicePath(r.In.URL.Path)

			m.mu.Lock()
			port, known := m.routes[name]
			m.mu.Unlock()
			if !known {
				// Nothing to route to; the proxy reports a failure rather than
				// dialling something arbitrary.
				r.Out.URL = &url.URL{Scheme: "http", Host: "ingress.invalid"}
				return
			}

			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = m.agentAddr(port)
			r.Out.Host = r.Out.URL.Host
			r.Out.URL.Path = rest
			r.Out.URL.RawPath = ""
		},
	}
	return proxy
}

// agentAddr names where the agent is, for a transport that knows how to get
// there. The port is the agent's own choice, so this must never become an
// address in this process's network namespace: see reachable.
func (m *IngressManager) agentAddr(port int) string {
	if m.AgentAddr != nil {
		return m.AgentAddr(port)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// reachable reports whether this manager can deliver into the sandbox at all.
//
// There used to be a fallback here: with no reverse channel, dial
// 127.0.0.1:<port> and hope the agent shares this network namespace. That is a
// hole rather than a degraded mode. The port is chosen by the agent, and this
// process's loopback is the pod's -- where sam-node's API, other sidecars and
// every other boundary are listening. An agent could therefore announce a
// service whose backend is the node that vouches for it, and the mesh would
// route to it.
//
// So an agent that may serve needs a channel into its sandbox, and without one
// nothing is registered.
func (m *IngressManager) reachable() error {
	if m.AgentSocket != "" || m.AgentAddr != nil {
		return nil
	}
	return fmt.Errorf("no way into the sandbox: set --agent-ingress-socket to the path " +
		"nano-init --ingress-socket serves, because delivering to an address in this " +
		"process's network namespace would reach the gateway's neighbours rather than the agent")
}

// AgentTransport reaches the sandbox over its reverse channel when there is
// one, and returns nil when the agent can be dialled directly.
//
// The address the forwarder writes is still 127.0.0.1:<port>, because that is
// what the port means where it is going. Only the dialling changes: the port is
// carried in the handshake and the connection is made by the process inside the
// sandbox, which is the one that can.
func (m *IngressManager) AgentTransport() http.RoundTripper {
	if m.AgentSocket == "" {
		return nil // the default transport dials the address directly
	}
	socket := m.AgentSocket
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("ingress target %q: %w", addr, err)
			}
			return dialSandbox(ctx, socket, port)
		},
	}
}

// dialSandbox opens one connection through the sandbox's reverse channel.
//
// The handshake is Firecracker's -- "CONNECT <port>", then "OK" -- so a microVM
// can offer the same protocol over vsock and nothing here has to know which
// kind of sandbox it is talking to.
func dialSandbox(ctx context.Context, socket, port string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("reach the sandbox's ingress socket %s: %w", socket, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ask the sandbox for port %s: %w", port, err)
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, 128)).ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read the sandbox's answer for port %s: %w", port, err)
	}
	if strings.TrimSpace(reply) != "OK" {
		_ = conn.Close()
		return nil, fmt.Errorf("the sandbox refused port %s: %s", port, strings.TrimSpace(reply))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// Close stops serving, so a detached sandbox stops being routed to: the
// node's backend probe starts failing and withholds the name from discovery.
func (m *IngressManager) Close() {
	m.mu.Lock()
	listener := m.listener
	m.listener = nil
	m.routes = nil
	m.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}
}

// splitServicePath separates the leading service name from the rest of the path.
func splitServicePath(path string) (name, rest string) {
	trimmed := path
	if len(trimmed) > 0 && trimmed[0] == '/' {
		trimmed = trimmed[1:]
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '/' {
			return trimmed[:i], trimmed[i:]
		}
	}
	return trimmed, "/"
}
