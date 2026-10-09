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

package discovery

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/agentmesh/api"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Discovery is the node-facing handle: it announces local services
// (provider role) and maintains an interest-scoped view of remote
// providers (consumer role) over a shared set of gossip topics.
type Discovery struct {
	ps   *pubsub.PubSub
	self peer.ID
	tm   *topicManager

	interval     time.Duration
	interestTTL  time.Duration
	staleAfter   time.Duration
	maxProviders int

	runCtx context.Context

	// announceTopics is only touched by the announce loop goroutine.
	announceTopics map[string]*heldTopic

	viewMu    sync.Mutex
	interests map[string]*interest
	providers map[string]Provider // keyed peer|type|service
}

type heldTopic struct {
	topic   *pubsub.Topic
	release func()
}

type interest struct {
	sub     *pubsub.Subscription
	release func()
	expires time.Time
}

// Option tweaks Discovery timing/bounds (used by tests).
type Option func(*Discovery)

func WithIntervals(announce, interestTTL, staleAfter time.Duration) Option {
	return func(d *Discovery) {
		d.interval = announce
		d.interestTTL = interestTTL
		d.staleAfter = staleAfter
	}
}

func WithMaxProviders(n int) Option {
	return func(d *Discovery) { d.maxProviders = n }
}

func New(ps *pubsub.PubSub, self peer.ID, opts ...Option) *Discovery {
	d := &Discovery{
		ps:             ps,
		self:           self,
		tm:             newTopicManager(ps),
		interval:       DefaultAnnounceInterval,
		interestTTL:    DefaultInterestTTL,
		staleAfter:     DefaultStaleAfter,
		maxProviders:   DefaultMaxProviders,
		announceTopics: map[string]*heldTopic{},
		interests:      map[string]*interest{},
		providers:      map[string]Provider{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Start runs the announce and interest-janitor loops until ctx is done.
// src may be nil for consume-only nodes.
func (d *Discovery) Start(ctx context.Context, src SourceFunc) {
	d.runCtx = ctx
	go d.janitorLoop(ctx)
	if src != nil {
		go d.announceLoop(ctx, src)
	}
}

func (d *Discovery) announceLoop(ctx context.Context, src SourceFunc) {
	for {
		// ±20% jitter to avoid mesh-wide synchronization.
		jittered := d.interval + time.Duration((rand.Float64()-0.5)*0.4*float64(d.interval))
		select {
		case <-ctx.Done():
			for _, held := range d.announceTopics {
				held.release()
			}
			d.announceTopics = map[string]*heldTopic{}
			return
		case <-time.After(jittered):
			d.announceOnce(ctx, src())
		}
	}
}

// announceOnce reconciles the joined topic set with the desired one and
// publishes on every desired topic that currently has subscribers.
func (d *Discovery) announceOnce(ctx context.Context, anns []Announcement) {
	desired := map[string][][]byte{}

	for _, ann := range anns {
		msg := &api.ServiceAnnounce{
			PeerId:         d.self.String(),
			Type:           ann.Type,
			ServiceName:    ann.Name,
			Keys:           ann.Keys,
			Labels:         ann.Labels,
			ActiveRequests: ann.Load.ActiveRequests,
			LatencyEwmaMs:  ann.Load.LatencyEWMAMs,
			AnnounceTime:   timestamppb.Now(),
		}
		if err := api.ValidateServiceAnnounce(msg); err != nil {
			logger.Warnf("[Discovery] skipping invalid announcement for %q: %v", ann.Name, err)
			continue
		}
		data, err := proto.Marshal(msg)
		if err != nil {
			continue
		}
		for _, key := range ann.Keys {
			name, err := api.DiscoveryTopic(ann.Type, key)
			if err != nil {
				continue
			}
			desired[name] = append(desired[name], data)
		}
	}

	// Release topics no longer served.
	for name, held := range d.announceTopics {
		if _, ok := desired[name]; !ok {
			held.release()
			delete(d.announceTopics, name)
		}
	}

	for name, msgs := range desired {
		held, ok := d.announceTopics[name]
		if !ok {
			topic, release, err := d.tm.acquire(name)
			if err != nil {
				logger.Warnf("[Discovery] join %s: %v", name, err)
				continue
			}
			held = &heldTopic{topic: topic, release: release}
			d.announceTopics[name] = held
		}
		// Publish-when-subscribed: silence costs nothing on an idle mesh.
		if len(held.topic.ListPeers()) == 0 {
			continue
		}
		for _, data := range msgs {
			if err := held.topic.Publish(ctx, data); err != nil {
				logger.Debugf("[Discovery] publish %s: %v", name, err)
			}
		}
	}
}
