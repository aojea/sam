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
	"context"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/node/discovery"
)

// discoverySourceProbeTimeout bounds each backend model probe per tick.
const discoverySourceProbeTimeout = 2 * time.Second

// activeTracker is implemented by services that report in-flight load.
type activeTracker interface {
	ActiveRequests() uint32
}

// toolLister is implemented by services that report served MCP tool names.
type toolLister interface {
	Tools(ctx context.Context) ([]string, error)
}

// capKeys bounds announced keys to the wire cap; keys are expected sorted so
// truncation is deterministic across ticks.
func capKeys(keys []string) []string {
	if len(keys) > api.MaxAnnounceKeys {
		return keys[:api.MaxAnnounceKeys]
	}
	return keys
}

// discoverySource builds gossip announcements from the registered services:
// inference services announce model IDs, MCP services announce tool names.
func (n *SamNode) discoverySource() []discovery.Announcement {
	labels := n.labels()
	var out []discovery.Announcement
	for _, info := range n.services.List(api.ServiceType_SERVICE_TYPE_UNSPECIFIED) {
		svc, ok := n.services.Get(info.GetName())
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), discoverySourceProbeTimeout)
		keys, err := serviceKeys(ctx, svc, info.GetType())
		cancel()
		if err != nil || len(keys) == 0 {
			continue
		}
		ann := discovery.Announcement{
			Type:   info.GetType(),
			Name:   info.GetName(),
			Keys:   capKeys(keys),
			Labels: labels,
		}
		if tracker, ok := svc.(activeTracker); ok {
			ann.Load.ActiveRequests = tracker.ActiveRequests()
		}
		out = append(out, ann)
	}
	return out
}

// serviceKeys returns the routing keys a service serves, per its type.
func serviceKeys(ctx context.Context, svc Service, t api.ServiceType) ([]string, error) {
	switch t {
	case api.ServiceType_SERVICE_TYPE_INFERENCE:
		if lister, ok := svc.(modelLister); ok {
			return lister.Models(ctx)
		}
	case api.ServiceType_SERVICE_TYPE_MCP:
		if lister, ok := svc.(toolLister); ok {
			return lister.Tools(ctx)
		}
	case api.ServiceType_SERVICE_TYPE_EGRESS:
		// The destination name is the only key: callers look a hostname up.
		return []string{svc.Info().GetName()}, nil
	}
	return nil, nil
}
