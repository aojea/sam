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
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	kb "github.com/libp2p/go-libp2p-kbucket"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
)

const (
	// directoryRequestTimeout bounds one request, the lookup across the
	// routers included. A lookup on a mesh of routers that all know each
	// other is one round; the bound is for a router that stopped answering.
	directoryRequestTimeout = 20 * time.Second
	// directoryMaxMessage bounds a request or response frame.
	directoryMaxMessage = 64 * 1024
	// directoryDefaultLimit and directoryMaxLimit bound how many providers
	// a find_providers returns.
	directoryDefaultLimit = 20
	directoryMaxLimit     = 100
)

const (
	directoryOpProvide       = "provide"
	directoryOpFindProviders = "find_providers"
	directoryOpFindPeer      = "find_peer"
	directoryOpPlace         = "place"

	directoryResultOK      = "ok"
	directoryResultEmpty   = "empty"
	directoryResultInvalid = "invalid"
	directoryResultError   = "error"
)

var directoryRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agentmesh_router_directory_requests_total",
		Help: "Directory requests served to members and routers, by operation and result; place is a record another router put here",
	},
	[]string{"op", "result"},
)

var directoryRequestSeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "agentmesh_router_directory_request_seconds",
		Help:    "Time to answer a directory request, the lookup across the routers included",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	},
	[]string{"op"},
)

var directoryPlacementsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agentmesh_router_directory_placements_total",
		Help: "Copies of a member's record this router placed on the routers closest to the key, by result",
	},
	[]string{"result"},
)

func init() {
	for _, op := range []string{directoryOpProvide, directoryOpFindProviders, directoryOpFindPeer, directoryOpPlace} {
		for _, result := range []string{directoryResultOK, directoryResultEmpty, directoryResultInvalid, directoryResultError} {
			directoryRequestsTotal.WithLabelValues(op, result)
		}
	}
	for _, result := range []string{directoryResultOK, directoryResultError} {
		directoryPlacementsTotal.WithLabelValues(result)
	}
}

// handleDirectory serves one DirectoryRequest on a stream from a member
// or a router. It is registered through the auth gate, so the peer has
// authenticated by the time it runs. A member keeps no DHT: it announces
// its records here and the router keeps them and places them on the
// routers closest to the key; it looks keys up here and the router runs
// the lookup across the routers' DHT.
func (r *Router) handleDirectory(s network.Stream) {
	defer func() { _ = s.Close() }()
	remote := s.Conn().RemotePeer()
	_ = s.SetDeadline(time.Now().Add(directoryRequestTimeout))
	ctx, cancel := context.WithTimeout(r.lifetime(), directoryRequestTimeout)
	defer cancel()

	var req api.DirectoryRequest
	reader := msgio.NewVarintReaderSize(s, directoryMaxMessage)
	frame, err := reader.ReadMsg()
	if err != nil {
		logger.Debugf("[Directory] Request from %s not read: %v", remote, err)
		_ = s.Reset()
		return
	}
	err = proto.Unmarshal(frame, &req)
	reader.ReleaseMsg(frame)
	if err != nil {
		logger.Warnf("[Directory] Request from %s not decoded: %v", remote, err)
		directoryRequestsTotal.WithLabelValues("unknown", directoryResultInvalid).Inc()
		_ = s.Reset()
		return
	}

	op, resp := r.serveDirectory(ctx, remote, &req)
	data, err := proto.Marshal(resp)
	if err != nil {
		logger.Errorf("[Directory] Response to %s not encoded: %v", remote, err)
		_ = s.Reset()
		return
	}
	if err := msgio.NewVarintWriter(s).WriteMsg(data); err != nil {
		logger.Debugf("[Directory] Response to %s (%s) not written: %v", remote, op, err)
		_ = s.Reset()
	}
}

// serveDirectory answers one request and accounts for it.
func (r *Router) serveDirectory(ctx context.Context, remote peer.ID, req *api.DirectoryRequest) (string, *api.DirectoryResponse) {
	start := time.Now()
	var op string
	var resp *api.DirectoryResponse
	var err error
	switch q := req.GetRequest().(type) {
	case *api.DirectoryRequest_Provide:
		op = directoryOpProvide
		if q.Provide.GetProvider() != "" {
			op = directoryOpPlace
		}
		resp, err = r.directoryProvide(ctx, remote, q.Provide)
	case *api.DirectoryRequest_FindProviders:
		op = directoryOpFindProviders
		resp, err = r.directoryFindProviders(ctx, q.FindProviders)
	case *api.DirectoryRequest_FindPeer:
		op = directoryOpFindPeer
		resp, err = r.directoryFindPeer(ctx, q.FindPeer)
	default:
		return "unknown", r.directoryRefused("unknown", remote, errInvalidDirectoryRequest)
	}
	directoryRequestSeconds.WithLabelValues(op).Observe(time.Since(start).Seconds())
	if err != nil {
		return op, r.directoryRefused(op, remote, err)
	}
	result := directoryResultOK
	if len(resp.GetProviders()) == 0 && resp.GetPeer() == nil && op != directoryOpProvide && op != directoryOpPlace {
		result = directoryResultEmpty
	}
	directoryRequestsTotal.WithLabelValues(op, result).Inc()
	return op, resp
}

var errInvalidDirectoryRequest = errors.New("request is not provide, find_providers or find_peer")

// directoryRefused answers a request the router did not run, and says why
// at the level the cause deserves: a malformed request is the peer's
// doing, a failed lookup is the mesh's.
func (r *Router) directoryRefused(op string, remote peer.ID, err error) *api.DirectoryResponse {
	var invalid *directoryInvalidError
	if errors.As(err, &invalid) {
		directoryRequestsTotal.WithLabelValues(op, directoryResultInvalid).Inc()
		logger.Warnf("[Directory] Refusing %s from %s: %v", op, remote, err)
	} else {
		directoryRequestsTotal.WithLabelValues(op, directoryResultError).Inc()
		logger.Warnf("[Directory] %s from %s failed: %v", op, remote, err)
	}
	return &api.DirectoryResponse{Error: err.Error()}
}

// directoryInvalidError is a request the peer got wrong.
type directoryInvalidError struct{ err error }

func (e *directoryInvalidError) Error() string { return e.err.Error() }
func (e *directoryInvalidError) Unwrap() error { return e.err }

func invalidDirectoryRequest(format string, args ...any) error {
	return &directoryInvalidError{err: fmt.Errorf(format, args...)}
}

// directoryProvide stores a record. From a member it is the member's own,
// kept here and placed on the routers closest to the key; from a router it
// is a member's record that router placed here, which only a router may do.
func (r *Router) directoryProvide(ctx context.Context, remote peer.ID, q *api.DirectoryProvide) (*api.DirectoryResponse, error) {
	if err := api.ValidateDirectoryKey(q.GetKey()); err != nil {
		return nil, invalidDirectoryRequest("%w", err)
	}
	provider := remote
	placed := q.GetProvider() != ""
	if placed {
		if !r.isAuthenticatedPeerRouter(remote) {
			return nil, invalidDirectoryRequest("only a router may name a provider; %s is not one", remote)
		}
		p, err := peer.Decode(q.GetProvider())
		if err != nil {
			return nil, invalidDirectoryRequest("provider: %w", err)
		}
		provider = p
	}
	addrs, err := recordAddrs(provider, q.GetAddrs())
	if err != nil {
		return nil, invalidDirectoryRequest("%w", err)
	}
	if r.DHT == nil {
		return nil, errors.New("the directory is not up")
	}
	if err := r.DHT.ProviderStore().AddProvider(ctx, q.GetKey(), peer.AddrInfo{ID: provider, Addrs: addrs}); err != nil {
		return nil, fmt.Errorf("store record: %w", err)
	}
	if !placed {
		r.placeRecord(ctx, q.GetKey(), provider, q.GetAddrs())
	}
	return &api.DirectoryResponse{}, nil
}

// recordAddrs parses a record's addresses: at most DirectoryMaxAddrs, each
// with no peer suffix or the provider's own, stored without it as the
// peerstore keeps them.
func recordAddrs(provider peer.ID, raw []string) ([]multiaddr.Multiaddr, error) {
	if len(raw) > api.DirectoryMaxAddrs {
		return nil, fmt.Errorf("%d addresses, at most %d", len(raw), api.DirectoryMaxAddrs)
	}
	addrs := make([]multiaddr.Multiaddr, 0, len(raw))
	for _, a := range raw {
		ma, err := multiaddr.NewMultiaddr(a)
		if err != nil {
			return nil, fmt.Errorf("address %q: %w", a, err)
		}
		transport, p := peer.SplitAddr(ma)
		if p != "" && p != provider {
			return nil, fmt.Errorf("address %q names %s, the record is %s's", a, p, provider)
		}
		if transport == nil {
			return nil, fmt.Errorf("address %q has no transport", a)
		}
		addrs = append(addrs, transport)
	}
	return addrs, nil
}

// placeRecord puts a member's record on the routers closest to the key,
// as many as the mesh's replication, over the directory protocol with the
// provider named. Every copy is attempted; a router that fails to take it
// is logged and counted, the member's own router keeps a copy regardless.
func (r *Router) placeRecord(ctx context.Context, key []byte, provider peer.ID, addrs []string) {
	targets := r.DHT.RoutingTable().NearestPeers(kb.ConvertKey(string(key)), r.directory.Replication)
	req := &api.DirectoryRequest{Request: &api.DirectoryRequest_Provide{Provide: &api.DirectoryProvide{
		Key: key, Addrs: addrs, Provider: provider.String(),
	}}}
	data, err := proto.Marshal(req)
	if err != nil {
		logger.Errorf("[Directory] Placement for %s not encoded: %v", provider, err)
		return
	}
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(target peer.ID) {
			defer wg.Done()
			if err := r.sendDirectory(ctx, target, data); err != nil {
				directoryPlacementsTotal.WithLabelValues(directoryResultError).Inc()
				logger.Warnf("[Directory] Record of %s not placed on router %s: %v", provider, target, err)
				return
			}
			directoryPlacementsTotal.WithLabelValues(directoryResultOK).Inc()
		}(target)
	}
	wg.Wait()
}

// sendDirectory sends one encoded request to a peer router and reads its
// answer, over the gated host so the router has authenticated first.
func (r *Router) sendDirectory(ctx context.Context, target peer.ID, data []byte) error {
	s, err := r.gated.NewStream(ctx, target, api.DirectoryProtocolID)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.SetDeadline(deadline)
	}
	if err := msgio.NewVarintWriter(s).WriteMsg(data); err != nil {
		_ = s.Reset()
		return err
	}
	reader := msgio.NewVarintReaderSize(s, directoryMaxMessage)
	frame, err := reader.ReadMsg()
	if err != nil {
		_ = s.Reset()
		return err
	}
	defer reader.ReleaseMsg(frame)
	var resp api.DirectoryResponse
	if err := proto.Unmarshal(frame, &resp); err != nil {
		return err
	}
	if resp.GetError() != "" {
		return errors.New(resp.GetError())
	}
	return nil
}

// directoryFindProviders runs the lookup across the routers' DHT.
func (r *Router) directoryFindProviders(ctx context.Context, q *api.DirectoryFindProviders) (*api.DirectoryResponse, error) {
	if err := api.ValidateDirectoryKey(q.GetKey()); err != nil {
		return nil, invalidDirectoryRequest("%w", err)
	}
	limit := int(q.GetLimit())
	if limit <= 0 {
		limit = directoryDefaultLimit
	}
	if limit > directoryMaxLimit {
		limit = directoryMaxLimit
	}
	records, err := r.lookupProviders(ctx, q.GetKey(), limit)
	if err != nil {
		return nil, err
	}
	return &api.DirectoryResponse{Providers: records}, nil
}

// lookupProviders is the lookup both find_providers and find_peer run.
func (r *Router) lookupProviders(ctx context.Context, key []byte, limit int) ([]*api.PeerRecord, error) {
	if r.DHT == nil {
		return nil, errors.New("the directory is not up")
	}
	c := cid.NewCidV1(cid.Raw, key)
	records := make([]*api.PeerRecord, 0, limit)
	seen := map[peer.ID]bool{}
	for info := range r.DHT.FindProvidersAsync(ctx, c, limit) {
		if seen[info.ID] {
			continue
		}
		seen[info.ID] = true
		records = append(records, peerRecord(info))
		if len(records) >= limit {
			break
		}
	}
	if err := ctx.Err(); err != nil && len(records) == 0 {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	return records, nil
}

// directoryFindPeer answers where a peer is reached: from this router's
// own peerstore for a member attached here, from the record the member
// announced under its peer key otherwise.
func (r *Router) directoryFindPeer(ctx context.Context, q *api.DirectoryFindPeer) (*api.DirectoryResponse, error) {
	p, err := peer.Decode(q.GetPeerId())
	if err != nil {
		return nil, invalidDirectoryRequest("peer_id: %w", err)
	}
	if r.isPeerAuthenticated(p) && r.Host.Network().Connectedness(p) == network.Connected {
		if addrs := r.Host.Peerstore().Addrs(p); len(addrs) > 0 {
			return &api.DirectoryResponse{Peer: peerRecord(peer.AddrInfo{ID: p, Addrs: addrs})}, nil
		}
	}
	records, err := r.lookupProviders(ctx, api.PeerKey(p), 1)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		if rec.GetPeerId() == p.String() {
			return &api.DirectoryResponse{Peer: rec}, nil
		}
	}
	return &api.DirectoryResponse{}, nil
}

// peerRecord renders a peer and its addresses, each with the peer suffix.
func peerRecord(info peer.AddrInfo) *api.PeerRecord {
	rec := &api.PeerRecord{PeerId: info.ID.String()}
	for _, a := range info.Addrs {
		if _, p := peer.SplitAddr(a); p == "" {
			a = a.Encapsulate(multiaddr.StringCast("/p2p/" + info.ID.String()))
		}
		rec.Addrs = append(rec.Addrs, a.String())
	}
	return rec
}
