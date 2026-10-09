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
	"crypto/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsub_pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// newPair builds two connected in-memory hosts with gossipsub and Discovery.
func newPair(t *testing.T) (provider, consumer *Discovery) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mn := mocknet.New()
	h1, err := mn.GenPeer()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := mn.GenPeer()
	if err != nil {
		t.Fatal(err)
	}
	if err := mn.LinkAll(); err != nil {
		t.Fatal(err)
	}
	if err := mn.ConnectAllButSelf(); err != nil {
		t.Fatal(err)
	}

	ps1, err := pubsub.NewGossipSub(ctx, h1)
	if err != nil {
		t.Fatal(err)
	}
	ps2, err := pubsub.NewGossipSub(ctx, h2)
	if err != nil {
		t.Fatal(err)
	}

	opts := []Option{WithIntervals(100*time.Millisecond, 5*time.Second, 2*time.Second)}
	provider = New(ps1, h1.ID(), opts...)
	consumer = New(ps2, h2.ID(), opts...)
	return provider, consumer
}

func TestAnnounceReachesInterestedConsumer(t *testing.T) {
	provider, consumer := newPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	provider.Start(ctx, func() []Announcement {
		return []Announcement{{
			Type:   api.ServiceType_SERVICE_TYPE_INFERENCE,
			Name:   "llm",
			Keys:   []string{"model-a", "model-b"},
			Labels: map[string]string{"region": "eu"},
			Load:   Load{ActiveRequests: 3},
		}}
	})
	consumer.Start(ctx, nil)

	// Interest registered after the announcer is already running: the
	// publish-when-subscribed gate must open once the subscription spreads.
	consumer.Ensure(api.ServiceType_SERVICE_TYPE_INFERENCE, "model-a")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ps := consumer.Providers(api.ServiceType_SERVICE_TYPE_INFERENCE, "model-a"); len(ps) == 1 {
			p := ps[0]
			if p.Service != "llm" || p.Labels["region"] != "eu" || p.Load.ActiveRequests != 3 {
				t.Fatalf("unexpected provider: %+v", p)
			}
			if !p.ServesKey("model-b") {
				t.Fatalf("provider should carry all keys, got %v", p.Keys)
			}
			// Uninterested key on the same service: not subscribed, but the
			// payload still resolves it once observed.
			if ps2 := consumer.Providers(api.ServiceType_SERVICE_TYPE_INFERENCE, "model-b"); len(ps2) != 1 {
				t.Fatalf("model-b should resolve from the same announcement, got %v", ps2)
			}
			// Wrong type must not match.
			if ps3 := consumer.Providers(api.ServiceType_SERVICE_TYPE_MCP, "model-a"); len(ps3) != 0 {
				t.Fatalf("MCP lookup should be empty, got %v", ps3)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout waiting for announcement to reach consumer")
}

func TestProvidersGoStaleWithoutAnnouncements(t *testing.T) {
	provider, consumer := newPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var announcing atomic.Bool
	announcing.Store(true)
	provider.Start(ctx, func() []Announcement {
		if !announcing.Load() {
			return nil
		}
		return []Announcement{{
			Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
			Name: "llm",
			Keys: []string{"model-a"},
		}}
	})
	consumer.Start(ctx, nil)
	consumer.Ensure(api.ServiceType_SERVICE_TYPE_INFERENCE, "model-a")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(consumer.Providers(api.ServiceType_SERVICE_TYPE_INFERENCE, "model-a")) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	announcing.Store(false)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(consumer.Providers(api.ServiceType_SERVICE_TYPE_INFERENCE, "model-a")) == 0 {
			return // went stale as expected
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("provider never went stale after announcements stopped")
}

// testPeerID generates a peer identity for constructing raw messages.
func testPeerID(t *testing.T) peer.ID {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func rawMessage(t *testing.T, from peer.ID, ann *api.ServiceAnnounce) *pubsub.Message {
	t.Helper()
	data, err := proto.Marshal(ann)
	if err != nil {
		t.Fatal(err)
	}
	return &pubsub.Message{Message: &pubsub_pb.Message{From: []byte(from), Data: data}}
}

func TestObserveRejectsForgedAndStale(t *testing.T) {
	d := New(nil, testPeerID(t))
	signer := testPeerID(t)
	other := testPeerID(t)

	valid := &api.ServiceAnnounce{
		PeerId:       signer.String(),
		Type:         api.ServiceType_SERVICE_TYPE_INFERENCE,
		ServiceName:  "llm",
		Keys:         []string{"m1"},
		AnnounceTime: timestamppb.Now(),
	}

	tests := []struct {
		name string
		msg  *pubsub.Message
		want int
	}{
		{"valid announce accepted", rawMessage(t, signer, valid), 1},
		{"peer_id not matching signer dropped", rawMessage(t, other, valid), 0},
		{"stale timestamp dropped", rawMessage(t, signer, &api.ServiceAnnounce{
			PeerId: signer.String(), Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
			ServiceName: "llm", Keys: []string{"m1"},
			AnnounceTime: timestamppb.New(time.Now().Add(-10 * time.Minute)),
		}), 0},
		{"future timestamp dropped", rawMessage(t, signer, &api.ServiceAnnounce{
			PeerId: signer.String(), Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
			ServiceName: "llm", Keys: []string{"m1"},
			AnnounceTime: timestamppb.New(time.Now().Add(10 * time.Minute)),
		}), 0},
		{"invalid payload dropped", &pubsub.Message{
			Message: &pubsub_pb.Message{From: []byte(signer), Data: []byte("junk")},
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.providers = map[string]Provider{}
			d.observe(tt.msg)
			if got := len(d.providers); got != tt.want {
				t.Errorf("providers: got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestProviderTableIsBounded(t *testing.T) {
	d := New(nil, testPeerID(t), WithMaxProviders(2))
	for range 3 {
		signer := testPeerID(t)
		d.observe(rawMessage(t, signer, &api.ServiceAnnounce{
			PeerId: signer.String(), Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
			ServiceName: "llm", Keys: []string{"m1"},
			AnnounceTime: timestamppb.Now(),
		}))
	}
	if got := len(d.providers); got != 2 {
		t.Fatalf("provider table size: got %d, want 2", got)
	}
}

// One peer announcing many distinct service names must not be able to push
// everyone else out of a full table: the global cap evicts the oldest entry,
// which is whoever announced least recently, not whoever announced most.
func TestProviderTableCapsEntriesPerSigner(t *testing.T) {
	d := New(nil, testPeerID(t), WithMaxProviders(MaxProvidersPerSigner+4))
	honest := testPeerID(t)
	d.observe(rawMessage(t, honest, &api.ServiceAnnounce{
		PeerId: honest.String(), Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
		ServiceName: "llm", Keys: []string{"m1"},
		AnnounceTime: timestamppb.Now(),
	}))

	loud := testPeerID(t)
	for i := range 3 * MaxProvidersPerSigner {
		d.observe(rawMessage(t, loud, &api.ServiceAnnounce{
			PeerId: loud.String(), Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
			ServiceName: "svc-" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Keys: []string{"m1"},
			AnnounceTime: timestamppb.Now(),
		}))
	}

	if got := d.countBySignerLocked(loud.String()); got != MaxProvidersPerSigner {
		t.Errorf("loud signer holds %d entries, want exactly %d", got, MaxProvidersPerSigner)
	}
	if got := len(d.Providers(api.ServiceType_SERVICE_TYPE_INFERENCE, "m1")); got != MaxProvidersPerSigner+1 {
		t.Errorf("providers for m1: got %d, want %d", got, MaxProvidersPerSigner+1)
	}
	if d.countBySignerLocked(honest.String()) != 1 {
		t.Error("the honest signer's entry was evicted by another peer's announcements")
	}
}

func TestPeerLabels(t *testing.T) {
	d := New(nil, testPeerID(t))
	signer := testPeerID(t)
	d.observe(rawMessage(t, signer, &api.ServiceAnnounce{
		PeerId: signer.String(), Type: api.ServiceType_SERVICE_TYPE_MCP,
		ServiceName: "reviewer", Keys: []string{"review_pr"},
		Labels:       map[string]string{"region": "eu"},
		AnnounceTime: timestamppb.Now(),
	}))

	if got := d.PeerLabels(signer.String()); got["region"] != "eu" {
		t.Errorf("PeerLabels: got %v, want region=eu", got)
	}
	if got := d.PeerLabels("unknown-peer"); got != nil {
		t.Errorf("PeerLabels for unknown peer: got %v, want nil", got)
	}
}

func TestValidateServiceAnnounceCaps(t *testing.T) {
	base := func() *api.ServiceAnnounce {
		return &api.ServiceAnnounce{
			PeerId: "12D3KooWTest", Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
			ServiceName: "llm", Keys: []string{"m1"}, AnnounceTime: timestamppb.Now(),
		}
	}
	tooMany := make([]string, api.MaxAnnounceKeys+1)
	for i := range tooMany {
		tooMany[i] = "m"
	}

	tests := []struct {
		name    string
		mutate  func(*api.ServiceAnnounce)
		wantErr bool
	}{
		{"valid", func(a *api.ServiceAnnounce) {}, false},
		{"no keys", func(a *api.ServiceAnnounce) { a.Keys = nil }, true},
		{"too many keys", func(a *api.ServiceAnnounce) { a.Keys = tooMany }, true},
		{"empty peer", func(a *api.ServiceAnnounce) { a.PeerId = "" }, true},
		{"unspecified type", func(a *api.ServiceAnnounce) { a.Type = api.ServiceType_SERVICE_TYPE_UNSPECIFIED }, true},
		{"no announce time", func(a *api.ServiceAnnounce) { a.AnnounceTime = nil }, true},
		{"oversized label", func(a *api.ServiceAnnounce) {
			a.Labels = map[string]string{"k": string(make([]byte, api.MaxAnnounceStringLen+1))}
		}, true},
		{"free-form region label", func(a *api.ServiceAnnounce) {
			a.Labels = map[string]string{"region": "us-east-1"}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := base()
			tt.mutate(a)
			if err := api.ValidateServiceAnnounce(a); (err != nil) != tt.wantErr {
				t.Errorf("ValidateServiceAnnounce: err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestObserveKeysOnTheCanonicalPeerID(t *testing.T) {
	d := New(nil, testPeerID(t))
	signer := testPeerID(t)

	alias := peer.ToCid(signer).String()
	if alias == signer.String() {
		t.Fatal("peer.ToCid did not produce a second spelling of the signer")
	}
	spellings := []string{signer.String(), alias}
	for _, spelling := range spellings {
		decoded, err := peer.Decode(spelling)
		if err != nil || decoded != signer {
			t.Fatalf("%q does not decode to the signer (err %v)", spelling, err)
		}
	}

	announce := func(spelling string) {
		t.Helper()
		d.observe(rawMessage(t, signer, &api.ServiceAnnounce{
			PeerId: spelling, Type: api.ServiceType_SERVICE_TYPE_INFERENCE,
			ServiceName: "llm", Keys: []string{"m1"},
			Labels:       map[string]string{"region": "eu"},
			AnnounceTime: timestamppb.Now(),
		}))
	}

	for _, spelling := range spellings {
		announce(spelling)
		if got := len(d.providers); got != 1 {
			t.Fatalf("after announcing as %q: got %d providers, want 1: %+v", spelling, got, d.providers)
		}
		for key, p := range d.providers {
			if p.PeerID != signer.String() {
				t.Errorf("provider keyed %q carries peer id %q, want %q", key, p.PeerID, signer.String())
			}
		}
	}

	if got := d.PeerLabels(signer.String()); got["region"] != "eu" {
		t.Errorf("PeerLabels under the canonical id: got %v, want region=eu", got)
	}
}
