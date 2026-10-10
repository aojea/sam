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
	"net/netip"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	circuit "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
	xrate "github.com/libp2p/go-libp2p/x/rate"
)

// hubLimits returns the libp2p resource limits for a router, sized from the
// connection budget rather than from host memory. libp2p's defaults are for
// a leaf peer that talks to a few dozen others and scale with the memory of
// the machine: a router on a 1 GB VM got 1024 inbound streams in all, 64 for
// identify and 64 relayed connections per peer, and refused the handshakes
// of a joining fleet (libp2p_rcmgr_blocked_resources counts them). A router
// is a hub: every member holds a connection and, on it, a few long-lived
// streams (gossip, its relay reservation) and a burst of short ones when it
// joins (identify, the auth handshake, DHT), and a service many members
// call holds one relayed connection per caller.
//
// conns is the inbound connection budget per source address and circuits
// the relayed connections one peer may hold; both come from Options.
func hubLimits(conns, circuits int) rcmgr.ConcreteLimitConfig {
	limits := rcmgr.DefaultLimits
	libp2p.SetDefaultServiceLimits(&limits)
	scaled := limits.AutoScale()

	sysBase := max(conns, DefaultHighWaterMark)
	// Streams per member on the scopes that see every member.
	const streamsPerConn = 8
	streams := func(n int) rcmgr.ResourceLimits {
		return rcmgr.ResourceLimits{
			Streams:         rcmgr.LimitVal(2 * n),
			StreamsInbound:  rcmgr.LimitVal(n),
			StreamsOutbound: rcmgr.LimitVal(n),
		}
	}
	system := streams(streamsPerConn * sysBase)
	system.Conns = rcmgr.LimitVal(2 * sysBase)
	system.ConnsInbound = rcmgr.LimitVal(sysBase)
	system.FD = rcmgr.LimitVal(2 * sysBase)
	transient := streams(sysBase)
	transient.Conns = rcmgr.LimitVal(sysBase)
	transient.ConnsInbound = rcmgr.LimitVal(sysBase)
	transient.FD = rcmgr.LimitVal(sysBase)

	// A relayed connection is one hop stream from the caller and one stop
	// stream to the destination, both held while it lasts; the memory the
	// relay reserves per connection is a few KiB on each.
	relayStreams := streams(sysBase)
	relayStreams.Memory = rcmgr.LimitVal64(int64(sysBase) * 64 << 10)
	perPeerCircuits := streams(circuits)

	overrides := rcmgr.PartialLimitConfig{
		System:    system,
		Transient: transient,
		Service: map[string]rcmgr.ResourceLimits{
			identify.ServiceName: streams(sysBase),
			relay.ServiceName:    relayStreams,
		},
		ServicePeer: map[string]rcmgr.ResourceLimits{
			relay.ServiceName: perPeerCircuits,
		},
		Protocol: map[protocol.ID]rcmgr.ResourceLimits{
			identify.ID:           streams(sysBase),
			identify.IDPush:       streams(sysBase),
			api.AuthProtocolID:    streams(sysBase),
			circuit.ProtoIDv2Hop:  relayStreams,
			circuit.ProtoIDv2Stop: relayStreams,
		},
		ProtocolPeer: map[protocol.ID]rcmgr.ResourceLimits{
			circuit.ProtoIDv2Hop:  perPeerCircuits,
			circuit.ProtoIDv2Stop: perPeerCircuits,
		},
	}
	return overrides.Build(scaled)
}

// perIPConnResourceManager is libp2p's resource manager with hubLimits, the
// per-source-IP inbound connection cap and the per-subnet connection rate
// limit scaled to it; see Options.ConnsPerSourceIP. All of these matter
// behind a proxy or NAT: every peer shares a few source IPs, so libp2p's
// per-IP cap (8), its per-IP rate (0.2 conns/s, burst 16) and the small
// transient scope each take down the whole listener under normal reconnect
// churn.
func perIPConnResourceManager(limit, circuits int) (network.ResourceManager, error) {
	// Same shape as the rcmgr default limiter (loopback exempt, no global
	// cap), with the per-subnet budget scaled by limit relative to the
	// default per-IP cap of 8.
	scale := float64(limit) / 8
	connRateLimiter := &xrate.Limiter{
		NetworkPrefixLimits: []xrate.PrefixLimit{
			{Prefix: netip.MustParsePrefix("127.0.0.0/8"), Limit: xrate.Limit{}},
			{Prefix: netip.MustParsePrefix("::1/128"), Limit: xrate.Limit{}},
		},
		SubnetRateLimiter: xrate.SubnetLimiter{
			IPv4SubnetLimits: []xrate.SubnetLimit{
				{PrefixLength: 32, Limit: xrate.Limit{RPS: 0.2 * scale, Burst: 2 * limit}},
			},
			IPv6SubnetLimits: []xrate.SubnetLimit{
				{PrefixLength: 56, Limit: xrate.Limit{RPS: 0.2 * scale, Burst: 2 * limit}},
			},
			GracePeriod: time.Minute,
		},
	}

	return rcmgr.NewResourceManager(
		rcmgr.NewFixedLimiter(hubLimits(limit, circuits)),
		rcmgr.WithLimitPerSubnet(
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 32, ConnCount: limit}},
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 56, ConnCount: limit}},
		),
		rcmgr.WithConnRateLimiters(connRateLimiter),
	)
}
