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
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	// DefaultNodeRetention keeps a lapsed enrollment for a month past its
	// session, and a bootstrap enrollment for a month past its last refresh,
	// long enough to be looked up when investigating an incident, and is
	// what --node-retention defaults to.
	DefaultNodeRetention = 30 * 24 * time.Hour

	// DefaultEphemeralNodeRetention is how long a member an ephemeral token
	// enrolled is kept after its last refresh: an hour, long past the few
	// minutes a member of a fleet, a probe or a CI job stays, and what
	// --ephemeral-node-retention defaults to.
	DefaultEphemeralNodeRetention = time.Hour

	// maxNodeGCInterval bounds how often the sweep runs. Rows lapse on a
	// scale of days, so nothing is gained by sweeping more often than hourly;
	// a retention far shorter than that, as a test sets, sweeps at a tenth
	// of it so a lapsed row does not outlive the retention by much.
	maxNodeGCInterval = time.Hour
	minNodeGCInterval = time.Second
)

// nodeGCInterval is the sweep period for a retention: a tenth of it, within
// [minNodeGCInterval, maxNodeGCInterval].
func nodeGCInterval(retention time.Duration) time.Duration {
	return min(maxNodeGCInterval, max(minNodeGCInterval, retention/10))
}

var nodesDeletedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "agentmesh_control_plane_nodes_deleted_total",
	Help: "Enrolled node records removed by the node GC, by reason: session for a session that lapsed more than the retention ago, silent for a bootstrap member that did not refresh for the retention, ephemeral for one an ephemeral token enrolled that did not refresh for the ephemeral retention",
}, []string{"reason"})

const (
	deletedReasonSession   = "session"
	deletedReasonSilent    = "silent"
	deletedReasonEphemeral = "ephemeral"
)

func init() {
	for _, reason := range []string{deletedReasonSession, deletedReasonSilent, deletedReasonEphemeral} {
		nodesDeletedTotal.WithLabelValues(reason)
	}
}

// runNodeGCLoop sweeps once at start and then hourly. Replicas each sweep
// against the shared database; the delete is idempotent, so the race is
// harmless and not worth a claim.
func (s *Server) runNodeGCLoop() {
	defer s.wg.Done()
	s.gcExpiredNodes(time.Now())

	ticker := time.NewTicker(nodeGCInterval(s.config.NodeRetention))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.gcExpiredNodes(time.Now())
		case <-s.ctx.Done():
			return
		}
	}
}

// gcExpiredNodes deletes every unbanned node whose session expired more than
// NodeRetention before now, and every unbanned bootstrap member, which has
// no session, that did not refresh for NodeRetention, or for
// EphemeralNodeRetention when an ephemeral token enrolled it. Options.Validate
// has refused a retention that is not positive, so there is no "keep
// forever": an unbounded nodes table is what slowed every console refresh
// and metrics scrape.
func (s *Server) gcExpiredNodes(now time.Time) {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	deleted, err := s.store.DeleteExpiredNodes(ctx, now.Add(-s.config.NodeRetention))
	if err != nil {
		logger.Errorf("Failed to delete expired node records: %v", err)
		return
	}
	if deleted > 0 {
		nodesDeletedTotal.WithLabelValues(deletedReasonSession).Add(float64(deleted))
		logger.Infof("Deleted %d node records whose session expired more than %s ago", deleted, s.config.NodeRetention)
	}

	silent, ephemeral, err := s.store.DeleteSilentBootstrapNodes(ctx, now.Add(-s.config.NodeRetention), now.Add(-s.config.EphemeralNodeRetention))
	if err != nil {
		logger.Errorf("Failed to delete silent bootstrap node records: %v", err)
		return
	}
	if silent > 0 {
		nodesDeletedTotal.WithLabelValues(deletedReasonSilent).Add(float64(silent))
		logger.Infof("Deleted %d bootstrap node records that did not refresh for %s", silent, s.config.NodeRetention)
	}
	if ephemeral > 0 {
		nodesDeletedTotal.WithLabelValues(deletedReasonEphemeral).Add(float64(ephemeral))
		logger.Infof("Deleted %d ephemeral node records that did not refresh for %s", ephemeral, s.config.EphemeralNodeRetention)
	}
}
