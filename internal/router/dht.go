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
	"fmt"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/records"

	"github.com/google/agentmesh/api"
)

// dhtOptions configures the router's DHT: a server for the members and the
// other routers. The record TTL is the mesh's (syncDirectory): three of the
// members' reprovide intervals, so a node that is gone drops out of
// discovery within it and two missed announcements do not unlist one that
// is not. The library default (48h) would keep every past identity listed.
func dhtOptions(directory api.DirectorySettings) []dht.Option {
	return []dht.Option{
		dht.Mode(dht.ModeServer),
		dht.ProtocolPrefix(api.DHTProtocolPrefix),
		dht.ProviderManagerOpts(records.ProviderAddrTTL(directory.RecordTTL), records.ProvideValidity(directory.RecordTTL)),
	}
}

// syncDirectory reads the mesh's directory parameters from /info before the
// DHT is built, since its record TTL is fixed at construction. The control
// plane answered the keys sync a moment ago, so a failure here is one to
// stop on rather than to run a TTL of the router's own.
func (r *Router) syncDirectory() error {
	info, err := r.controlPlane(10 * time.Second).FetchInfo(r.ctx)
	if err != nil {
		return fmt.Errorf("directory parameters: %w", err)
	}
	r.directory = api.DirectorySettingsFrom(info.GetDirectory())
	logger.Infof("[DHT] Directory parameters from the control plane: replication %d, reprovide every %s, records kept %s", r.directory.Replication, r.directory.Reprovide, r.directory.RecordTTL)
	return nil
}
