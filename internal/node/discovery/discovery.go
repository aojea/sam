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

// Package discovery provides interest-scoped service announcements over
// GossipSub. Providers announce routing keys (model IDs, tool names) on
// per-key topics only while those topics have subscribers; consumers
// subscribe only for keys they actively use. Cost therefore scales with
// interest, not mesh size. Announcements are routing hints authenticated by
// the pubsub layer's message signing — never authorization inputs.
package discovery

import (
	"time"

	"github.com/google/agentmesh/api"
	golog "github.com/ipfs/go-log/v2"
)

var logger = golog.Logger("agentmesh-discovery")

const (
	// DefaultAnnounceInterval is how often a provider publishes on topics
	// with subscribers; each tick is jittered ±20%.
	DefaultAnnounceInterval = 10 * time.Second
	// DefaultInterestTTL is how long a consumer stays subscribed to a key's
	// topic after the last Ensure call for it.
	DefaultInterestTTL = 5 * time.Minute
	// DefaultStaleAfter is the age beyond which a provider entry is no
	// longer returned by Providers.
	DefaultStaleAfter = 30 * time.Second
	// DefaultMaxProviders bounds the consumer-side provider table.
	DefaultMaxProviders = 1024
	// MaxProvidersPerSigner bounds how many entries one peer may hold in
	// the table: entries are keyed by signer|type|service, so without this
	// a single peer announcing many service names could evict everyone
	// else's entries (the global cap evicts oldest, not loudest).
	MaxProvidersPerSigner = 16
	// maxAnnounceSkew rejects announcements too old or too far in the
	// future (replay/clock defense; pubsub dedup handles exact replays).
	maxAnnounceSkew = 2 * time.Minute
)

// Load carries a provider's runtime hints. Zero values mean unknown.
type Load struct {
	ActiveRequests uint32
	LatencyEWMAMs  float64
}

// Announcement is one service's advertised state, fed by the node.
type Announcement struct {
	Type   api.ServiceType
	Name   string
	Keys   []string
	Labels map[string]string
	Load   Load
}

// SourceFunc supplies the current local announcements on each tick.
type SourceFunc func() []Announcement

// Provider is a remote service observed via gossip.
type Provider struct {
	PeerID   string
	Type     api.ServiceType
	Service  string
	Keys     []string
	Labels   map[string]string
	Load     Load
	LastSeen time.Time
}

// ServesKey reports whether the provider advertised the given routing key.
func (p Provider) ServesKey(key string) bool {
	for _, k := range p.Keys {
		if k == key {
			return true
		}
	}
	return false
}
