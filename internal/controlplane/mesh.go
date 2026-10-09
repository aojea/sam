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

package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/storage"
)

// ServiceAnnouncement represents service discovery details retrieved from the mesh.
type ServiceAnnouncement struct {
	ServiceName string
	ServiceType string
	PeerID      string
	Addresses   []string
}

// NodeStatus represents mesh status information for a peer.
type NodeStatus struct {
	PeerID      string
	IsReachable bool
	Addresses   []string
	LastSeen    time.Time
}

// MeshAdapter defines the generic interface for Control Plane operations interacting with the Agent Mesh.
type MeshAdapter interface {
	// PublishEvent constructs, signs, and broadcasts a Control Plane MeshEvent (POLICY_UPDATE, BANNED, KEY_ROTATION).
	PublishEvent(ctx context.Context, eventType api.MeshEvent_Type, peerID string, payload []byte) error

	// DiscoverServices queries active mesh nodes/DHT for services matching a type or pattern.
	DiscoverServices(ctx context.Context, serviceType string) ([]*ServiceAnnouncement, error)

	// GetNodeStatus retrieves node reachability and status from the mesh.
	GetNodeStatus(ctx context.Context, peerID string) (*NodeStatus, error)

	// Close gracefully releases any P2P host resources, streams, and PubSub topics.
	Close() error
}

// NopMeshAdapter provides a no-op implementation used when P2P mesh integration is disabled or in unit tests.
type NopMeshAdapter struct{}

func NewNopMeshAdapter() *NopMeshAdapter {
	return &NopMeshAdapter{}
}

func (n *NopMeshAdapter) PublishEvent(ctx context.Context, eventType api.MeshEvent_Type, peerID string, payload []byte) error {
	logger.Debugf("[NopMeshAdapter] PublishEvent skipped (P2P disabled): type=%v, peerID=%s", eventType, peerID)
	return nil
}

func (n *NopMeshAdapter) DiscoverServices(ctx context.Context, serviceType string) ([]*ServiceAnnouncement, error) {
	return nil, nil
}

func (n *NopMeshAdapter) GetNodeStatus(ctx context.Context, peerID string) (*NodeStatus, error) {
	return nil, nil
}

func (n *NopMeshAdapter) Close() error {
	return nil
}

// P2PMeshAdapter publishes control plane events on the mesh's gossip topic.
//
// It is the control plane's whole presence on the mesh, and it is one-way:
// events go out so a ban, a key rotation or a policy change reaches routers
// and nodes the moment it happens, and nothing is read back. Every consumer
// still pulls /keys, /info and /policies on its own schedule, so a missed
// event is a delay, never a divergence. Where the adapter runs on a host of
// its own (NewMeshPublisher) that host has no listen address, no DHT, no
// relay and no stream handlers: it can dial routers and nothing can dial it.
type P2PMeshAdapter struct {
	host  host.Host
	topic *pubsub.Topic
	store storage.Store
	mu    sync.Mutex
	// close tears down what NewMeshPublisher built; nil when the host and
	// topic belong to someone else (agentmesh-one's embedded router).
	close func() error
}

// NewP2PMeshAdapter publishes on an existing host's topic. The caller owns
// both and closes them; Close on the adapter is a no-op.
func NewP2PMeshAdapter(h host.Host, topic *pubsub.Topic, store storage.Store) (*P2PMeshAdapter, error) {
	if h == nil || topic == nil || store == nil {
		return nil, fmt.Errorf("host, topic, and store cannot be nil")
	}
	if topic.String() != api.GossipEvents {
		return nil, fmt.Errorf("topic %q is not the mesh events topic %q", topic.String(), api.GossipEvents)
	}
	return &P2PMeshAdapter{host: h, topic: topic, store: store}, nil
}

// RouterDialTimeout bounds each attempt to reach a leased router.
const RouterDialTimeout = 10 * time.Second

// DefaultMeshReconnectInterval is how often the publisher re-reads the lease
// table; a router that just enrolled waits at most this long for events. It
// is one query and at most a few dials per tick, so it is kept short.
const DefaultMeshReconnectInterval = 30 * time.Second

// NewMeshPublisher builds the control plane's own publish-only peer and
// keeps it connected to every router holding a lease, re-checking the lease
// table every reconnect. The lease table is all the control plane needs to
// know about the mesh's shape, and it already has it. Close stops the loop
// and the host.
func NewMeshPublisher(ctx context.Context, store storage.Store, reconnect time.Duration) (*P2PMeshAdapter, error) {
	if store == nil {
		return nil, fmt.Errorf("store cannot be nil")
	}
	if reconnect <= 0 {
		return nil, fmt.Errorf("reconnect interval must be positive, got %s", reconnect)
	}
	h, err := libp2p.New(libp2p.NoListenAddrs, libp2p.DisableRelay())
	if err != nil {
		return nil, fmt.Errorf("mesh publisher host: %w", err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	// StrictSign is the default; pinned because routers and nodes key their
	// per-author rate limit on the signed sender.
	ps, err := pubsub.NewGossipSub(loopCtx, h, pubsub.WithMessageSignaturePolicy(pubsub.StrictSign))
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, fmt.Errorf("mesh publisher gossipsub: %w", err)
	}
	topic, err := ps.Join(api.GossipEvents)
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, fmt.Errorf("join %s: %w", api.GossipEvents, err)
	}
	p := &P2PMeshAdapter{host: h, topic: topic, store: store}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.keepRoutersConnected(loopCtx, reconnect)
	}()
	p.close = func() error {
		cancel()
		wg.Wait()
		_ = topic.Close()
		return h.Close()
	}
	logger.Infof("[Mesh] Publishing control plane events as %s", h.ID())
	return p, nil
}

// keepRoutersConnected dials, now and every interval, each leased router the
// host is not connected to. A publish only reaches peers that have announced
// the topic, so connections are kept warm ahead of the events rather than
// made when one is due.
func (p *P2PMeshAdapter) keepRoutersConnected(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		p.connectRouters(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *P2PMeshAdapter) connectRouters(ctx context.Context) {
	routers, err := p.store.GetActiveRouters(ctx)
	if err != nil {
		logger.Warnf("[Mesh] Cannot list routers to publish to: %v", err)
		return
	}
	for _, r := range routers {
		info, err := routerAddrInfo(r)
		if err != nil {
			logger.Warnf("[Mesh] Skipping router lease %q: %v", r.PeerID, err)
			continue
		}
		if p.host.Network().Connectedness(info.ID) == network.Connected {
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, RouterDialTimeout)
		err = p.host.Connect(dialCtx, info)
		cancel()
		if err != nil {
			logger.Warnf("[Mesh] Router %s unreachable for event publishing: %v", info.ID, err)
			continue
		}
		logger.Infof("[Mesh] Connected to router %s", info.ID)
	}
}

// routerAddrInfo is the dial target for a lease. Routers announce their
// addresses with a trailing /p2p/<id>; the dialer wants the id once and the
// addresses bare, and an address naming a different peer is not this
// router's.
func routerAddrInfo(r storage.RouterLease) (peer.AddrInfo, error) {
	pid, err := peer.Decode(r.PeerID)
	if err != nil {
		return peer.AddrInfo{}, fmt.Errorf("invalid peer ID: %w", err)
	}
	info := peer.AddrInfo{ID: pid}
	for _, s := range r.Addresses {
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			continue
		}
		addr, id := peer.SplitAddr(ma)
		if addr == nil || (id != "" && id != pid) {
			continue
		}
		info.Addrs = append(info.Addrs, addr)
	}
	if len(info.Addrs) == 0 {
		return peer.AddrInfo{}, fmt.Errorf("no dialable address in %v", r.Addresses)
	}
	return info, nil
}

func (p *P2PMeshAdapter) PublishEvent(ctx context.Context, eventType api.MeshEvent_Type, peerID string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var privKey ed25519.PrivateKey
	if p.store != nil {
		key, err := p.signingKeyFor(ctx, eventType, payload)
		if err != nil {
			return fmt.Errorf("failed to retrieve signing key for event publishing: %w", err)
		}
		privKey = key
	}

	var canonical string
	// Events can have an empty peer ID.
	if peerID != "" {
		pID, err := peer.Decode(peerID)
		if err != nil {
			return fmt.Errorf("invalid peer ID %q: %w", peerID, err)
		}
		canonical = pID.String()
	}

	event := &api.MeshEvent{
		Type:         eventType,
		PeerId:       canonical,
		EventTime:    timestamppb.Now(),
		NewPublicKey: payload,
	}

	if privKey != nil {
		eventData, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
		if err != nil {
			return fmt.Errorf("failed to marshal event for signing: %w", err)
		}
		event.Signature = ed25519.Sign(privKey, eventData)
	}

	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal signed mesh event: %w", err)
	}

	if err := p.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("failed to publish mesh event to topic %s: %w", api.GossipEvents, err)
	}

	logger.Infof("[P2PMeshAdapter] Published MeshEvent %v to topic %s (peerID: %s)", eventType, api.GossipEvents, peerID)
	return nil
}

// signingKeyFor picks the key receivers can verify with. A KEY_ROTATION
// announces newPub, which nobody trusts yet, so it is signed by the key just
// retired into its grace period (the one with the latest expiration that is
// not newPub); every other event is signed by the current key.
func (p *P2PMeshAdapter) signingKeyFor(ctx context.Context, eventType api.MeshEvent_Type, newPub []byte) (ed25519.PrivateKey, error) {
	if eventType != api.MeshEvent_KEY_ROTATION {
		key, _, err := p.store.GetCurrentKey(ctx)
		return key, err
	}
	pairs, err := p.store.GetAllValidKeys(ctx)
	if err != nil {
		return nil, err
	}
	var retiring *storage.KeyPair
	for i := range pairs {
		kp := &pairs[i]
		if kp.Expiration.IsZero() || bytes.Equal(kp.Public, newPub) {
			continue
		}
		if retiring == nil || kp.Expiration.After(retiring.Expiration) {
			retiring = kp
		}
	}
	if retiring == nil {
		// First key ever: there is no previous key and nobody to convince.
		key, _, err := p.store.GetCurrentKey(ctx)
		return key, err
	}
	return retiring.Private, nil
}

func (p *P2PMeshAdapter) DiscoverServices(ctx context.Context, serviceType string) ([]*ServiceAnnouncement, error) {
	return nil, nil
}

func (p *P2PMeshAdapter) GetNodeStatus(ctx context.Context, peerID string) (*NodeStatus, error) {
	return nil, nil
}

func (p *P2PMeshAdapter) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.close == nil {
		return nil
	}
	return p.close()
}
