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
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/records"

	"github.com/google/agentmesh/api"
)

// dhtOptions configures the member's DHT. A member is a DHT client whatever
// its reachability: the routers are the directory, and a member that served
// it would sit in every other member's routing table and take their lookups
// on a 400-connection budget, answer whoever connects, and be trusted for
// unsigned provider records. A public member is still dialled directly; the
// role in the directory and the way it is reached are separate things.
func dhtOptions(cfg Options) []dht.Option {
	providerTTL := cfg.DHTProviderAddrTTL
	if providerTTL <= 0 {
		providerTTL = DefaultDHTProviderAddrTTL
	}
	opts := []dht.Option{
		dht.Mode(dht.ModeClient),
		dht.ProtocolPrefix(api.DHTProtocolPrefix),
		dht.ProviderManagerOpts(records.ProviderAddrTTL(providerTTL), records.ProvideValidity(providerTTL)),
	}
	if cfg.DHTMaxRecordAge > 0 {
		opts = append(opts, dht.MaxRecordAge(cfg.DHTMaxRecordAge))
	}
	return opts
}
