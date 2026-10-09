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
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// nodeStateCollector exports the node's own view of its mesh membership: the
// same figures GET /debug/mesh-info and /debug/token-info answer on demand,
// as gauges a scraper can keep. A node that runs unattended, such as a
// canary, becomes a continuous probe of the mesh this way.
type nodeStateCollector struct {
	n *AgentMeshNode

	readyDesc         *prometheus.Desc
	meshConnectedDesc *prometheus.Desc
	connectedDesc     *prometheus.Desc
	authenticatedDesc *prometheus.Desc
	dhtDesc           *prometheus.Desc
	biscuitExpiryDesc *prometheus.Desc
	servicesDesc      *prometheus.Desc
}

func newNodeStateCollector(n *AgentMeshNode) *nodeStateCollector {
	return &nodeStateCollector{
		n: n,
		readyDesc: prometheus.NewDesc(
			"agentmesh_node_ready",
			"1 once the libp2p host, DHT and identity store exist",
			nil, nil),
		meshConnectedDesc: prometheus.NewDesc(
			"agentmesh_node_mesh_connected",
			"1 while the node holds an authenticated connection to a router",
			nil, nil),
		connectedDesc: prometheus.NewDesc(
			"agentmesh_node_connected_peers",
			"Peers with an open libp2p connection, authenticated or not",
			nil, nil),
		authenticatedDesc: prometheus.NewDesc(
			"agentmesh_node_authenticated_peers",
			"Peers that have completed the mesh authentication handshake with this node",
			nil, nil),
		dhtDesc: prometheus.NewDesc(
			"agentmesh_node_dht_routing_table_size",
			"Peers in the Kademlia routing table",
			nil, nil),
		biscuitExpiryDesc: prometheus.NewDesc(
			"agentmesh_node_biscuit_expiry_timestamp_seconds",
			"Unix time the node's mesh credential expires",
			nil, nil),
		servicesDesc: prometheus.NewDesc(
			"agentmesh_node_services_registered",
			"Local services this node offers to the mesh, by type",
			[]string{"type"}, nil),
	}
}

func (c *nodeStateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.readyDesc, c.meshConnectedDesc, c.connectedDesc, c.authenticatedDesc,
		c.dhtDesc, c.biscuitExpiryDesc, c.servicesDesc,
	} {
		ch <- d
	}
}

func (c *nodeStateCollector) Collect(ch chan<- prometheus.Metric) {
	n := c.n
	if n.debugReady() != nil {
		ch <- prometheus.MustNewConstMetric(c.readyDesc, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.readyDesc, prometheus.GaugeValue, 1)

	connected := 0.0
	if n.IsConnected() {
		connected = 1
	}
	ch <- prometheus.MustNewConstMetric(c.meshConnectedDesc, prometheus.GaugeValue, connected)
	ch <- prometheus.MustNewConstMetric(c.connectedDesc, prometheus.GaugeValue, float64(len(n.Host.Network().Peers())))

	authenticated := 0
	n.authPeers.Range(func(_, _ any) bool {
		authenticated++
		return true
	})
	ch <- prometheus.MustNewConstMetric(c.authenticatedDesc, prometheus.GaugeValue, float64(authenticated))
	ch <- prometheus.MustNewConstMetric(c.dhtDesc, prometheus.GaugeValue, float64(n.DHT.RoutingTable().Size()))

	if exp, err := n.Store.LoadIdentityExpiration(); err == nil && exp > 0 {
		ch <- prometheus.MustNewConstMetric(c.biscuitExpiryDesc, prometheus.GaugeValue, float64(exp))
	}

	if n.services != nil {
		byType := map[string]int{}
		for _, svc := range n.services.List(api.ServiceType_SERVICE_TYPE_UNSPECIFIED) {
			byType[serviceTypeLabel(svc.Type)]++
		}
		for t, count := range byType {
			ch <- prometheus.MustNewConstMetric(c.servicesDesc, prometheus.GaugeValue, float64(count), t)
		}
	}
}

// serviceTypeLabel turns SERVICE_TYPE_MCP into "mcp".
func serviceTypeLabel(t api.ServiceType) string {
	return strings.ToLower(strings.TrimPrefix(t.String(), "SERVICE_TYPE_"))
}

// metricsHandler serves the process-wide registry, which carries the request
// and inference counters and the Go runtime, alongside this node's own state.
// The state collector is per node so two nodes in one process (tests,
// agentmesh-one) never fight over a registration.
func (n *AgentMeshNode) metricsHandler() http.Handler {
	n.metricsOnce.Do(func() {
		n.metricsRegistry = prometheus.NewRegistry()
		n.metricsRegistry.MustRegister(newNodeStateCollector(n))
	})
	return promhttp.HandlerFor(
		prometheus.Gatherers{prometheus.DefaultGatherer, n.metricsRegistry},
		promhttp.HandlerOpts{},
	)
}

// StartMetricsServer serves /metrics, /healthz and /readyz on addr with no
// authentication, for a scraper and a kubelet that hold no sidecar token.
// The sidecar's own /metrics stays token-gated: this listener exists so a
// socket-only node, which has no TCP port at all, can still be observed. It
// is off unless an operator names the address, since nothing on it is gated.
func StartMetricsServer(node *AgentMeshNode, addr string) (*http.Server, error) {
	if addr == "" {
		return nil, errors.New("no metrics address configured")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", node.metricsHandler())
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if node.debugReady() != nil || !node.IsConnected() {
			http.Error(w, "not connected to the mesh", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		// Informational once Serve has the listener; lets callers find the port.
		Addr:              listener.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()
	logger.Infof("Metrics listening on http://%s", listener.Addr())
	return server, nil
}
