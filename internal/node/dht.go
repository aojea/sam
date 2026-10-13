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
//
// The table is not refreshed. kad-dht treats a table of ten peers or fewer
// as starved and, every two minutes, pings every entry and runs a lookup
// for the member's own ID against all of them; with the routers as the only
// servers that is a connection to every router the member is not attached
// to, closed by the router at its auth deadline and opened again two
// minutes later, for nothing the member needs: the servers are the routers
// the control plane lists, learned on attach and from closer-peer answers,
// and a router that leaves the catalog leaves the table (setRouterCatalog).
// Bootstrap still fills the table once at start.
//
// The record TTL and the replication factor are the mesh's
// (ControlPlaneInfoResponse.directory). The TTL is the same figure the
// routers keep a record for, so a record the member's own store holds and
// one a router holds expire alike. The replication factor r is kademlia's
// k on the member: a record is stored on the r routers closest to its key,
// a lookup asks those r, r at a time. The routers keep every router in
// their own tables (router.dhtOptions) and answer FIND_NODE with the
// closest routers to the key in order, so a member learns the r closest
// from one answer of a router it is attached to and reaches them in one
// hop. A member therefore holds sessions with its K attached routers plus
// the r closest to each key it publishes or looks up, whatever the number
// of routers; with the library's k of 20 that would be every router of a
// mesh with fewer than 20.
func dhtOptions(directory api.DirectorySettings) []dht.Option {
	return []dht.Option{
		dht.Mode(dht.ModeClient),
		dht.DisableAutoRefresh(),
		dht.ProtocolPrefix(api.DHTProtocolPrefix),
		dht.BucketSize(directory.Replication),
		dht.Concurrency(directory.Replication),
		dht.Resiliency(directory.Replication),
		dht.ProviderManagerOpts(records.ProviderAddrTTL(directory.RecordTTL), records.ProvideValidity(directory.RecordTTL)),
	}
}
