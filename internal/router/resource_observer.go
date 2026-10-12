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
	"sync"
	"time"

	"github.com/google/agentmesh/internal/p2plimits"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var inboundRefusedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agentmesh_router_inbound_connections_refused_total",
		Help: "Inbound connections the resource manager refused before the handshake, by reason",
	},
	[]string{"reason"},
)

var streamsRefusedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agentmesh_router_streams_refused_total",
		Help: "Streams the resource manager refused, by scope and protocol (or service); a member whose DHT or gossip stream is refused drops out of discovery",
	},
	[]string{"scope", "protocol"},
)

func init() {
	for _, v := range p2plimits.Reasons {
		inboundRefusedTotal.WithLabelValues(v)
	}
	for _, p := range memberProtocols {
		streamsRefusedTotal.WithLabelValues(p2plimits.StreamScopeProtocol, string(p))
	}
}

// countStreamRefusals is the reporter the resource manager is built with;
// it counts refused streams in streamsRefusedTotal by protocol.
func countStreamRefusals() *p2plimits.StreamRefusals {
	return p2plimits.CountStreamRefusals(streamsRefusedTotal, logger.Warnf)
}

// connsPerSourceIPDesc exports the cap in force, so a dashboard can read the
// refusal counter against it.
var connsPerSourceIPDesc = prometheus.NewDesc(
	"agentmesh_router_conns_per_source_ip_limit",
	"Inbound connections allowed from one source IP",
	nil, nil)

// DefaultConnsPerSourceIP is the per-source-address cap derived from a
// connection manager high watermark.
func DefaultConnsPerSourceIP(highWaterMark int) int {
	return p2plimits.DefaultConnsPerSourceIP(highWaterMark)
}

// effectiveConnsPerSourceIP is the cap a dashboard should compare against.
func effectiveConnsPerSourceIP(configured int) int {
	if configured > 0 {
		return configured
	}
	return DefaultConnsPerSourceIP(DefaultHighWaterMark)
}

// perPeerLogLimiter admits one log line per peer per interval. Entries older
// than the interval are dropped on the way, so it holds only recent peers.
type perPeerLogLimiter struct {
	mu     sync.Mutex
	lastAt map[peer.ID]time.Time
}

const perPeerLogInterval = time.Minute

func (l *perPeerLogLimiter) allow(p peer.ID, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastAt == nil {
		l.lastAt = map[peer.ID]time.Time{}
	}
	if last, ok := l.lastAt[p]; ok && now.Sub(last) < perPeerLogInterval {
		return false
	}
	for k, t := range l.lastAt {
		if now.Sub(t) >= perPeerLogInterval {
			delete(l.lastAt, k)
		}
	}
	l.lastAt[p] = now
	return true
}
