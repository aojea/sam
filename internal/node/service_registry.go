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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/ipfs/go-cid"
)

// dhtProvider is the narrow DHT surface ServiceRegistry depends on.
// Production wires this to *dht.IpfsDHT; tests use a fake.
type dhtProvider interface {
	Provide(ctx context.Context, c cid.Cid, broadcast bool) error
}

// backendProber is implemented by services that can be asked whether their
// backend is really serving. Services that cannot be asked are advertised
// unconditionally.
type backendProber interface {
	Probe(ctx context.Context) error
}

// defaultDHTProbeTimeout is the default bound on one backend probe before
// advertising, used when a ServiceRegistry isn't given an explicit
// BackendProbeTimeout (see Options.BackendProbeTimeout / --backend-probe-
// timeout). Command-spawned backends (agentmesh-node.yaml's `command`, launched as
// a local subprocess) can need longer than this to answer their first
// request - a moderately-featured interpreted-language MCP server's own
// import/startup cost alone can exceed 2s - so this is a floor, not
// something every backend is expected to meet.
const defaultDHTProbeTimeout = 2 * time.Second

// advertisable reports whether a service is fit to be published to the DHT.
//
// Gossip already withholds announcements for a backend that does not answer,
// but the DHT is the other half of discovery and had no such check, so a
// backend that is not an MCP server at all stayed discoverable as one: it was
// listed by discover_remote_services and only failed later, at initialize, in
// the caller's face. Advertising is a claim the node makes on the backend's
// behalf, so it is the node that should verify it.
func advertisable(ctx context.Context, svc Service, probeTimeout time.Duration) error {
	prober, ok := svc.(backendProber)
	if !ok {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return prober.Probe(probeCtx)
}

// ServiceRegistry is the type-agnostic owner of registered services.
type ServiceRegistry struct {
	mu       sync.RWMutex
	services map[string]Service
	dht      dhtProvider

	// reprovideNow asks the node to run a reprovide cycle now, so a service
	// registered after the loop last ran does not wait a whole interval to be
	// advertised. Optional; nil outside a running node.
	reprovideNow func()

	// backendProbeTimeout bounds each backend probe in advertisable. Set once
	// at construction (see NewServiceRegistry); never mutated afterwards, so
	// reading it needs no lock.
	backendProbeTimeout time.Duration
}

// NewServiceRegistry constructs a registry bounding backend probes by
// backendProbeTimeout. A zero or negative value falls back to
// defaultDHTProbeTimeout, so callers can pass an unset
// Options.BackendProbeTimeout straight through without an explicit
// zero-check.
func NewServiceRegistry(d dhtProvider, backendProbeTimeout time.Duration) *ServiceRegistry {
	if backendProbeTimeout <= 0 {
		backendProbeTimeout = defaultDHTProbeTimeout
	}
	return &ServiceRegistry{
		services:            map[string]Service{},
		dht:                 d,
		backendProbeTimeout: backendProbeTimeout,
	}
}

// Register initialises a service, advertises it on the DHT, and inserts it
// into the map. Init runs before Provide so a failed handler-build never
// briefly advertises an unservable name.
//
// A backend that does not answer is registered but not advertised, rather than
// rejected: backends routinely start after the node does, and the reprovide
// loop picks them up once they answer.
//
// registering a name again replaces the instance and tears the old one down
func (r *ServiceRegistry) Register(ctx context.Context, svc Service) error {
	info := svc.Info()
	if info.Type == api.ServiceType_SERVICE_TYPE_UNSPECIFIED {
		return fmt.Errorf("cannot register service with unspecified type")
	}
	// The same check the config loader runs, for services that arrive by
	// other routes (FFI, tests): a name carrying its own scheme, such as
	// "plugin://x", would be addressed as type://plugin://x and dispatch
	// could no longer tell which type the policy was evaluated on.
	typeStr, err := api.ServiceTypeToString(info.Type)
	if err != nil {
		return err
	}
	if err := api.ValidateServiceFormat(typeStr + "://" + info.Name); err != nil {
		return fmt.Errorf("invalid service name %q: %w", info.Name, err)
	}

	// Policy is evaluated on type://name and the registry is keyed on name,
	// so a second type under the same name would let a grant for one reach
	// the other. One name, one service.
	r.mu.RLock()
	existing, taken := r.services[info.Name]
	r.mu.RUnlock()
	if taken && existing.Info().Type != info.Type {
		return fmt.Errorf("service name %q is already registered as %s; a name cannot serve two types", info.Name, existing.Info().Type)
	}

	if err := svc.Init(ctx); err != nil {
		return fmt.Errorf("init %s: %w", info.Name, err)
	}

	srvNameCID, err := serviceNameToCID(info.Type, info.Name)
	if err != nil {
		return err
	}
	srvTypeCID, err := serviceTypeToCID(info.Type)
	if err != nil {
		return err
	}

	probeErr := advertisable(ctx, svc, r.backendProbeTimeout)
	if probeErr != nil {
		logger.Warnf("[ServiceRegistry] Registered %s/%s but not advertising it: backend did not answer: %v", info.Type, info.Name, probeErr)
	} else {
		nameCtx, nameCancel := context.WithTimeout(ctx, 5*time.Second)
		defer nameCancel()
		if err := r.dht.Provide(nameCtx, srvNameCID, true); err != nil {
			logger.Warnf("[ServiceRegistry] DHT Provide (name) for %s: %v", info.Name, err)
		}

		typeCtx, typeCancel := context.WithTimeout(ctx, 5*time.Second)
		defer typeCancel()
		if err := r.dht.Provide(typeCtx, srvTypeCID, true); err != nil {
			logger.Warnf("[ServiceRegistry] DHT Provide (type) for %s: %v", info.Name, err)
		}
	}

	r.mu.Lock()
	replaced := r.services[info.Name]
	r.services[info.Name] = svc
	r.mu.Unlock()

	if replaced != nil && replaced != svc {
		if err := replaced.Teardown(); err != nil {
			logger.Errorf("[ServiceRegistry] Teardown replaced %s: %v", info.Name, err)
		}
	}

	if probeErr == nil {
		logger.Infof("[ServiceRegistry] Registered %s/%s (name CID: %s, type CID: %s)", info.Type, info.Name, srvNameCID, srvTypeCID)
	} else if r.reprovideNow != nil {
		r.reprovideNow()
	}
	return nil
}

// Unregister removes the service from the map and calls Teardown.
// Unknown names are a no-op.
func (r *ServiceRegistry) Unregister(ctx context.Context, name string) error {
	r.mu.Lock()
	svc, ok := r.services[name]
	delete(r.services, name)
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if err := svc.Teardown(); err != nil {
		logger.Errorf("[ServiceRegistry] Teardown %s: %v", name, err)
	}
	logger.Infof("[ServiceRegistry] Unregistered %s", name)
	return nil
}

// Get returns the service registered under name, if any.
func (r *ServiceRegistry) Get(name string) (Service, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	svc, ok := r.services[name]
	return svc, ok
}

// GetTyped returns the service registered under name only if it is of type
// t. Dispatch after authorization must use this: the policy was evaluated
// on t://name, and a service of another type under that name is not what
// the caller was granted.
func (r *ServiceRegistry) GetTyped(t api.ServiceType, name string) (Service, bool) {
	svc, ok := r.Get(name)
	if !ok || svc.Info().Type != t {
		return nil, false
	}
	return svc, true
}

// List returns the ServiceInfo for every registered service, optionally
// filtered by type. SERVICE_TYPE_UNSPECIFIED means "all types."
func (r *ServiceRegistry) List(typeFilter api.ServiceType) []*api.ServiceInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*api.ServiceInfo{}
	for _, svc := range r.services {
		info := svc.Info()
		if typeFilter != api.ServiceType_SERVICE_TYPE_UNSPECIFIED && info.Type != typeFilter {
			continue
		}
		out = append(out, info)
	}
	return out
}

// insertService inserts a service into the registry without calling Init or
// advertising on the DHT. For tests only.
func (r *ServiceRegistry) insertService(svc Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[svc.Info().Name] = svc
}

// TeardownAll calls Teardown on every registered service and clears the
// map. Per-service errors are logged; iteration continues.
func (r *ServiceRegistry) TeardownAll() {
	r.mu.Lock()
	svcs := r.services
	r.services = map[string]Service{}
	r.mu.Unlock()
	for name, svc := range svcs {
		if err := svc.Teardown(); err != nil {
			logger.Errorf("[ServiceRegistry] Teardown %s: %v", name, err)
		}
	}
}

// ReprovideAll re-provides all registered services to the DHT concurrently and
// reports how many were withheld because their backend did not answer.
// A service whose backend has stopped answering falls out of the DHT by not
// being reprovided, and returns on a later tick once it answers again.
func (r *ServiceRegistry) ReprovideAll(ctx context.Context) int {
	r.mu.Lock()
	toProvide := make([]Service, 0, len(r.services))
	for _, svc := range r.services {
		toProvide = append(toProvide, svc)
	}
	r.mu.Unlock()

	var withheld atomic.Int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

Loop:
	for _, svc := range toProvide {
		svc := svc
		select {
		case <-ctx.Done():
			break Loop
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()

			info := svc.Info()
			if err := advertisable(ctx, svc, r.backendProbeTimeout); err != nil {
				withheld.Add(1)
				// On shutdown every service fails this way, and saying so
				// would blame backends for the node stopping.
				if ctx.Err() == nil {
					logger.Warnf("[ServiceRegistry] Withholding %s/%s from the DHT: backend did not answer: %v", info.Type, info.Name, err)
				}
				return
			}

			if srvNameCID, err := serviceNameToCID(info.Type, info.Name); err == nil {
				nameCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = r.dht.Provide(nameCtx, srvNameCID, true)
				cancel()
			}
			if srvTypeCID, err := serviceTypeToCID(info.Type); err == nil {
				typeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = r.dht.Provide(typeCtx, srvTypeCID, true)
				cancel()
			}
		}()
	}
	wg.Wait()
	return int(withheld.Load())
}
