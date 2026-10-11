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

// Package p2plimits builds the libp2p resource manager a mesh component
// runs with: limits sized from its connection budget, a per-source-address
// connection cap in place of libp2p's default of 8, and a wrapper that
// counts and logs every inbound connection the manager refuses. libp2p
// refuses a connection before any byte of TLS and, for its per-IP and
// rate-limit paths, without a trace event; a dialer sees the connection
// close or a QUIC CONNECTION_REFUSED, and nothing else records it.
package p2plimits

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/network"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	xrate "github.com/libp2p/go-libp2p/x/rate"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/prometheus/client_golang/prometheus"
)

// Why the resource manager refused an inbound connection; the label values
// of a refusal counter.
const (
	RefusedPerIP     = "per_ip_limit"
	RefusedRateLimit = "rate_limit"
	RefusedScope     = "scope_limit"
	RefusedOther     = "other"
)

// Reasons lists every refusal label, for a counter to pre-register.
var Reasons = []string{RefusedPerIP, RefusedRateLimit, RefusedScope, RefusedOther}

// ConnsPerSourceIPShare is the fraction of the inbound connection budget one
// source address may hold when no cap is configured.
const ConnsPerSourceIPShare = 4

// DefaultConnsPerSourceIP is the per-source-address cap derived from a
// connection manager high watermark. Members share addresses (pod SNAT,
// Cloud NAT, offices, a load generator), and the handshake, not the
// address, is the trust boundary.
func DefaultConnsPerSourceIP(highWaterMark int) int {
	return max(highWaterMark/ConnsPerSourceIPShare, 1)
}

// libp2pDefaultConnsPerIP is the cap the resource manager applies when none
// is configured; the per-subnet rate is scaled from it.
const libp2pDefaultConnsPerIP = 8

// NewResourceManager is libp2p's resource manager with the given limits, a
// per-source-IP inbound connection cap of perIP and the per-subnet
// connection rate limit scaled to it. Loopback is exempt from both, as in
// libp2p's default, so a local mesh never meets them.
func NewResourceManager(limits rcmgr.ConcreteLimitConfig, perIP int) (network.ResourceManager, error) {
	scale := float64(perIP) / libp2pDefaultConnsPerIP
	connRateLimiter := &xrate.Limiter{
		NetworkPrefixLimits: []xrate.PrefixLimit{
			{Prefix: netip.MustParsePrefix("127.0.0.0/8"), Limit: xrate.Limit{}},
			{Prefix: netip.MustParsePrefix("::1/128"), Limit: xrate.Limit{}},
		},
		SubnetRateLimiter: xrate.SubnetLimiter{
			IPv4SubnetLimits: []xrate.SubnetLimit{
				{PrefixLength: 32, Limit: xrate.Limit{RPS: 0.2 * scale, Burst: 2 * perIP}},
			},
			IPv6SubnetLimits: []xrate.SubnetLimit{
				{PrefixLength: 56, Limit: xrate.Limit{RPS: 0.2 * scale, Burst: 2 * perIP}},
			},
			GracePeriod: time.Minute,
		},
	}
	return rcmgr.NewResourceManager(
		rcmgr.NewFixedLimiter(limits),
		rcmgr.WithLimitPerSubnet(
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 32, ConnCount: perIP}},
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 56, ConnCount: perIP}},
		),
		rcmgr.WithConnRateLimiters(connRateLimiter),
	)
}

// Streams is a stream budget of n each way.
func Streams(n int) rcmgr.ResourceLimits {
	return rcmgr.ResourceLimits{
		Streams:         rcmgr.LimitVal(2 * n),
		StreamsInbound:  rcmgr.LimitVal(n),
		StreamsOutbound: rcmgr.LimitVal(n),
	}
}

// RefusalLogInterval bounds how often one source IP's refusals are logged;
// the counter carries the rate.
const RefusalLogInterval = time.Minute

// Observed counts and logs what the wrapped manager refuses. It keeps the
// wrapped manager's connection limit visible to libp2p's connmgr sanity
// check, and leaves every scope it returns untouched.
type Observed struct {
	network.ResourceManager
	refused   *prometheus.CounterVec
	warnf     func(format string, args ...any)
	mu        sync.Mutex
	lastLogAt map[string]time.Time
	now       func() time.Time
}

// Observe wraps inner. refused is counted by reason (see Reasons) and warnf
// written once per source address per RefusalLogInterval.
func Observe(inner network.ResourceManager, refused *prometheus.CounterVec, warnf func(format string, args ...any)) *Observed {
	for _, v := range Reasons {
		refused.WithLabelValues(v)
	}
	return &Observed{ResourceManager: inner, refused: refused, warnf: warnf, lastLogAt: map[string]time.Time{}, now: time.Now}
}

// SetClock replaces the clock the log limiter reads; for tests.
func (m *Observed) SetClock(now func() time.Time) { m.now = now }

// SourcesLogged is how many source addresses the log limiter remembers.
func (m *Observed) SourcesLogged() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lastLogAt)
}

// LastLoggedAt is when source was last logged, and whether it is remembered.
func (m *Observed) LastLoggedAt(source string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastLogAt[source]
	return t, ok
}

func (m *Observed) OpenConnection(dir network.Direction, usefd bool, endpoint multiaddr.Multiaddr) (network.ConnManagementScope, error) {
	scope, err := m.ResourceManager.OpenConnection(dir, usefd, endpoint)
	if err != nil && dir == network.DirInbound {
		m.refusedConn(endpoint, err)
	}
	return scope, err
}

// GetConnLimit is what libp2p compares the connection manager's high water
// mark against at host construction.
func (m *Observed) GetConnLimit() int {
	if l, ok := m.ResourceManager.(connmgr.GetConnLimiter); ok {
		return l.GetConnLimit()
	}
	return 0
}

var _ connmgr.GetConnLimiter = (*Observed)(nil)

func (m *Observed) refusedConn(endpoint multiaddr.Multiaddr, err error) {
	reason := RefusalReason(err)
	m.refused.WithLabelValues(reason).Inc()

	source := "?"
	if ip, ipErr := manet.ToIP(endpoint); ipErr == nil {
		source = ip.String()
	}
	m.mu.Lock()
	last, seen := m.lastLogAt[source]
	now := m.now()
	shouldLog := !seen || now.Sub(last) >= RefusalLogInterval
	if shouldLog {
		m.lastLogAt[source] = now
		for k, t := range m.lastLogAt {
			if now.Sub(t) >= RefusalLogInterval {
				delete(m.lastLogAt, k)
			}
		}
	}
	m.mu.Unlock()
	if shouldLog {
		m.warnf("[Conn] Refused inbound connection from %s (%s): %v; the dialer sees the connection close before TLS", source, reason, err)
	}
}

// RefusalReason maps the resource manager's errors to the counter's labels.
// The per-IP and rate-limit paths return plain strings, not sentinel errors.
func RefusalReason(err error) string {
	if errors.Is(err, network.ErrResourceLimitExceeded) {
		return RefusedScope
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connections per ip limit"):
		return RefusedPerIP
	case strings.Contains(msg, "rate limit exceeded"):
		return RefusedRateLimit
	}
	return RefusedOther
}
