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
	"testing"

	"github.com/google/agentmesh/api"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	circuit "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
)

// TestHubLimitsFollowTheConnectionBudget pins the scopes that refused a
// joining fleet on the testnet to the connection budget instead of the
// host's memory: a router on a small VM must admit as many members as its
// watermarks say.
func TestHubLimitsFollowTheConnectionBudget(t *testing.T) {
	const conns, circuits = 6000, 1024
	cfg := hubLimits(conns, circuits).ToPartialLimitConfig()

	want := func(name string, got rcmgr.LimitVal, n int) {
		t.Helper()
		if int(got) != n {
			t.Errorf("%s = %d, want %d", name, got, n)
		}
	}
	want("system inbound conns", cfg.System.ConnsInbound, conns)
	want("system inbound streams", cfg.System.StreamsInbound, 8*conns)
	want("transient inbound streams", cfg.Transient.StreamsInbound, conns)
	want("identify service inbound streams", cfg.Service[identify.ServiceName].StreamsInbound, conns)
	want("identify protocol inbound streams", cfg.Protocol[identify.ID].StreamsInbound, conns)
	want("auth protocol inbound streams", cfg.Protocol[api.AuthProtocolID].StreamsInbound, conns)
	want("relay service inbound streams", cfg.Service[relay.ServiceName].StreamsInbound, conns)
	want("hop protocol inbound streams", cfg.Protocol[circuit.ProtoIDv2Hop].StreamsInbound, conns)
	// A destination many members call holds one stop stream per caller.
	want("relay per-peer outbound streams", cfg.ServicePeer[relay.ServiceName].StreamsOutbound, circuits)
	want("stop protocol per-peer outbound streams", cfg.ProtocolPeer[circuit.ProtoIDv2Stop].StreamsOutbound, circuits)

	// Below the default high watermark the budget is the watermark, so a
	// small per-address cap does not shrink the router.
	small := hubLimits(8, circuits).ToPartialLimitConfig()
	want("system inbound conns with a small per-address cap", small.System.ConnsInbound, DefaultHighWaterMark)
}
