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

// Package standalone wires the control plane, the libp2p router and the
// shared SQL store into one process serving a single public port: the
// router's WebSocket listener carries libp2p upgrades while every other HTTP
// request falls through to the control-plane mux. The embedded router keeps
// its stock control-plane client, pointed at a loopback-only listener, so no
// component grows in-process shortcuts.
package standalone

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/console"
	"github.com/google/sam/internal/controlplane"
	"github.com/google/sam/internal/router"
	"github.com/google/sam/internal/storage"
	golog "github.com/ipfs/go-log/v2"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/encoding/protojson"
)

var logger = golog.Logger("sam-one")

const (
	joinTokenPrefix  = "sam_tok_"
	adminTokenPrefix = "sam_adm_"

	joinTokenFile  = "join-token"
	adminTokenFile = "admin-token"
	routerKeyFile  = "router.key"

	consoleBasePath = "/console"

	// joinTokenTTL bounds the auto-generated join token; it is a development
	// credential, not a production secret rotation scheme.
	joinTokenTTL       = 10 * 365 * 24 * time.Hour
	joinTokenMaxUsages = 1 << 30

	// routerTokenTTL bounds the per-boot single-use token the embedded router
	// enrolls with; it never leaves the process.
	routerTokenTTL = time.Hour
)

// Options configures the standalone all-in-one server.
type Options struct {
	// BindAddress is the host:port of the single public listener. The host
	// must be empty or an IP literal.
	BindAddress string
	// ExternalURL is the public URL nodes reach this server on; it is turned
	// into the ws/wss multiaddr the router advertises. Optional.
	ExternalURL string
	// P2PListen holds optional extra native libp2p listen multiaddrs.
	P2PListen []string
	// DataDir stores the SQLite database, the router identity key and the
	// generated token files.
	DataDir string
	// DBDriver selects "sqlite" (default) or "postgres".
	DBDriver string
	// DBDSN is the database connection string; defaults to
	// <DataDir>/sam.db for sqlite.
	DBDSN string
	// JoinToken is the cluster join token; auto-generated and persisted in
	// DataDir when empty.
	JoinToken string
	// AdminToken protects the admin REST API; auto-generated and persisted in
	// DataDir when empty.
	AdminToken string
	// PolicyFile optionally seeds the mesh policy on first boot from a
	// protojson PolicyConfigUpdateRequest payload.
	PolicyFile string
	// OIDCIssuer optionally enables full OIDC enrollment.
	OIDCIssuer       string
	AllowedAudiences []string
}

// Default fills unset options with development-friendly values.
func (o *Options) Default() {
	if o.BindAddress == "" {
		o.BindAddress = "0.0.0.0:8080"
	}
	if o.DataDir == "" {
		o.DataDir = "."
	}
	if o.DBDriver == "" {
		o.DBDriver = "sqlite"
	}
	if o.DBDSN == "" && o.DBDriver == "sqlite" {
		o.DBDSN = filepath.Join(o.DataDir, "sam.db")
	}
	if len(o.AllowedAudiences) == 0 {
		o.AllowedAudiences = []string{api.DefaultAudience}
	}
}

// Validate rejects option combinations Start could not honor.
func (o *Options) Validate() error {
	if _, err := wsListenMultiaddr(o.BindAddress); err != nil {
		return fmt.Errorf("invalid bind address %q: %w", o.BindAddress, err)
	}
	if o.ExternalURL != "" {
		if _, err := externalMultiaddr(o.ExternalURL); err != nil {
			return fmt.Errorf("invalid external URL %q: %w", o.ExternalURL, err)
		}
	}
	if o.DBDSN == "" {
		return fmt.Errorf("a database DSN is required for driver %q", o.DBDriver)
	}
	for _, a := range o.P2PListen {
		if _, err := multiaddr.NewMultiaddr(a); err != nil {
			return fmt.Errorf("invalid p2p listen multiaddr %q: %w", a, err)
		}
	}
	return nil
}

// Server is the running all-in-one instance.
type Server struct {
	opts Options

	store      storage.Store
	cp         *controlplane.Server
	router     *router.Router
	adminToken string
	joinToken  string
	publicAddr string
}

// New validates the options and prepares a standalone server.
func New(opts Options) (*Server, error) {
	opts.Default()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &Server{opts: opts}, nil
}

// Start boots the store, the control plane on a loopback-only listener, and
// the router owning the single public port. It returns once the mesh accepts
// enrollments.
func (s *Server) Start(ctx context.Context) error {
	if err := os.MkdirAll(s.opts.DataDir, 0o755); err != nil {
		return fmt.Errorf("failed to create data dir: %w", err)
	}

	store, err := storage.NewSQLStore(s.opts.DBDriver, s.opts.DBDSN)
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	s.store = store

	s.adminToken = s.opts.AdminToken
	if s.adminToken == "" {
		if s.adminToken, err = loadOrCreateTokenFile(filepath.Join(s.opts.DataDir, adminTokenFile), adminTokenPrefix); err != nil {
			return fmt.Errorf("failed to provision admin token: %w", err)
		}
	}

	// Loopback-only control plane listener: the embedded router (and any
	// other in-process client) bootstraps against it before the public port
	// exists. Public traffic reaches the same handlers via the router's
	// HTTP fallback mux.
	cp, err := controlplane.NewServer(controlplane.Options{
		ListenAddr:            "127.0.0.1:0",
		DriverName:            s.opts.DBDriver,
		DataSourceName:        s.opts.DBDSN,
		OIDCIssuer:            s.opts.OIDCIssuer,
		AllowedAudiences:      s.opts.AllowedAudiences,
		BiscuitTimeout:        10 * time.Second,
		AdminToken:            s.adminToken,
		AutoApproveEnrollment: true,
	}, store)
	if err != nil {
		return fmt.Errorf("failed to create control plane: %w", err)
	}
	if err := cp.Start(); err != nil {
		return fmt.Errorf("failed to start control plane: %w", err)
	}
	s.cp = cp

	if err := s.seedPolicyOnFirstBoot(ctx); err != nil {
		return err
	}

	s.joinToken = s.opts.JoinToken
	if s.joinToken == "" {
		if s.joinToken, err = loadOrCreateTokenFile(filepath.Join(s.opts.DataDir, joinTokenFile), joinTokenPrefix); err != nil {
			return fmt.Errorf("failed to provision join token: %w", err)
		}
	}
	if err := s.ensureBootstrapToken(ctx, s.joinToken, api.RoleNode, joinTokenMaxUsages, joinTokenTTL, "sam-one join token"); err != nil {
		return fmt.Errorf("failed to register join token: %w", err)
	}

	// Per-boot single-use credential for the embedded router's stock
	// enrollment flow; never persisted or displayed.
	routerToken, err := generateToken("sam_rtr_")
	if err != nil {
		return err
	}
	if err := s.ensureBootstrapToken(ctx, routerToken, api.RoleRouter, 1, routerTokenTTL, "sam-one embedded router token"); err != nil {
		return fmt.Errorf("failed to register router token: %w", err)
	}

	mux := http.NewServeMux()
	cp.RegisterRoutes(mux)

	// The console proxies /console/api/* to the loopback control plane and
	// serves the embedded frontend; it queries /info at construction, which is
	// already live on the loopback listener.
	consoleSrv, err := console.NewServer(console.Config{
		ControlPlaneURL: "http://" + cp.Addr(),
		AdminToken:      s.adminToken,
		StaticFS:        console.EmbeddedAssets(),
		BasePath:        consoleBasePath,
		ExternalURL:     s.opts.ExternalURL,
	})
	if err != nil {
		return fmt.Errorf("failed to create console: %w", err)
	}
	mux.Handle(consoleBasePath+"/", consoleSrv.Handler())

	wsAddr, err := wsListenMultiaddr(s.opts.BindAddress)
	if err != nil {
		return err
	}
	var externalAddrs []string
	if s.opts.ExternalURL != "" {
		ext, err := externalMultiaddr(s.opts.ExternalURL)
		if err != nil {
			return err
		}
		externalAddrs = []string{ext}
	}

	rtr, err := router.NewRouter(ctx, router.Options{
		ControlPlaneURL:     "http://" + cp.Addr(),
		ListenAddrs:         append([]string{wsAddr}, s.opts.P2PListen...),
		ExternalAddrs:       externalAddrs,
		AllowLoopback:       true,
		KeysDBPath:          filepath.Join(s.opts.DataDir, routerKeyFile),
		BootstrapToken:      routerToken,
		HTTPFallbackHandler: mux,
	})
	if err != nil {
		return fmt.Errorf("failed to create router: %w", err)
	}
	if err := rtr.Start(); err != nil {
		return fmt.Errorf("failed to start router: %w", err)
	}
	s.router = rtr

	if s.publicAddr, err = s.resolvePublicAddr(); err != nil {
		_ = rtr.Close()
		return err
	}
	return nil
}

// Close shuts down the router, control plane and store.
func (s *Server) Close() error {
	var errs []string
	if s.router != nil {
		if err := s.router.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if s.cp != nil {
		if err := s.cp.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("standalone shutdown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Addr returns the public host:port actually bound (useful with port 0).
func (s *Server) Addr() string { return s.publicAddr }

// AdminToken returns the resolved admin API token.
func (s *Server) AdminToken() string { return s.adminToken }

// JoinToken returns the resolved cluster join token.
func (s *Server) JoinToken() string { return s.joinToken }

// PeerID returns the embedded router's peer ID.
func (s *Server) PeerID() string { return s.router.Host.ID().String() }

// seedPolicyOnFirstBoot installs the mesh policy only when none exists; the
// database stays authoritative afterwards.
func (s *Server) seedPolicyOnFirstBoot(ctx context.Context) error {
	roles, _, err := s.store.GetMeshPolicy(ctx)
	if err != nil && err != storage.ErrNotFound {
		return fmt.Errorf("failed to inspect mesh policy: %w", err)
	}
	if len(roles) > 0 {
		if s.opts.PolicyFile != "" {
			logger.Warnf("Mesh policy already present; ignoring --policy-file %s (edit via POST /policies)", s.opts.PolicyFile)
		}
		return nil
	}

	var seed api.PolicyConfigUpdateRequest
	if s.opts.PolicyFile != "" {
		data, err := os.ReadFile(s.opts.PolicyFile)
		if err != nil {
			return fmt.Errorf("failed to read policy file: %w", err)
		}
		if err := protojson.Unmarshal(data, &seed); err != nil {
			return fmt.Errorf("failed to parse policy file %s (expects protojson PolicyConfigUpdateRequest): %w", s.opts.PolicyFile, err)
		}
		logger.Infof("Seeding mesh policy from %s", s.opts.PolicyFile)
	} else {
		seed.Roles = defaultDevPolicyRoles()
		logger.Warn("Seeding OPEN development mesh policy (enrolled nodes may declare any label and register any service); provide --policy-file to restrict")
	}
	if err := s.store.SaveMeshPolicy(ctx, seed.Roles, seed.Bindings); err != nil {
		return fmt.Errorf("failed to seed mesh policy: %w", err)
	}
	return nil
}

// defaultDevPolicyRoles mirrors the Helm bootstrap job's role set with the
// node role opened up for zero-config development use.
func defaultDevPolicyRoles() []*api.PolicyRole {
	return []*api.PolicyRole{
		{Name: "sam-admin", AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}},
		{Name: api.RoleRouter, AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}},
		{Name: api.RoleNode, AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}, AllowedLabels: []string{"*"}},
	}
}

// ensureBootstrapToken stores the hashed token if absent (idempotent).
func (s *Server) ensureBootstrapToken(ctx context.Context, plaintext, role string, maxUsages int, ttl time.Duration, description string) error {
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(plaintext)))
	now := time.Now()
	return s.store.SaveBootstrapToken(ctx, &storage.BootstrapToken{
		ID:          id,
		TokenHash:   id,
		Role:        role,
		MaxUsages:   maxUsages,
		Description: description,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	})
}

// resolvePublicAddr recovers the actually-bound port from the router's WS
// listen addr and pairs it with the configured bind host.
func (s *Server) resolvePublicAddr() (string, error) {
	host, _, err := net.SplitHostPort(s.opts.BindAddress)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = "0.0.0.0"
	}
	for _, a := range s.router.Host.Network().ListenAddresses() {
		if _, err := a.ValueForProtocol(multiaddr.P_WS); err != nil {
			continue
		}
		port, err := a.ValueForProtocol(multiaddr.P_TCP)
		if err != nil {
			continue
		}
		return net.JoinHostPort(host, port), nil
	}
	return "", fmt.Errorf("router reports no WebSocket listen address")
}

func generateToken(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// loadOrCreateTokenFile reuses the token persisted at path, generating and
// saving a fresh one (0600) on first boot.
func loadOrCreateTokenFile(path, prefix string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", fmt.Errorf("token file %s is empty", path)
		}
		return tok, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	tok, err := generateToken(prefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("failed to persist token: %w", err)
	}
	return tok, nil
}

// wsListenMultiaddr turns a host:port bind address into a WebSocket listen
// multiaddr.
func wsListenMultiaddr(bind string) (string, error) {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = "0.0.0.0"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("bind host %q must be an IP literal", host)
	}
	if ip.To4() != nil {
		return fmt.Sprintf("/ip4/%s/tcp/%s/ws", ip, port), nil
	}
	return fmt.Sprintf("/ip6/%s/tcp/%s/ws", ip, port), nil
}

// externalMultiaddr turns a public http(s) URL into the ws/wss multiaddr the
// router advertises (http -> /ws, https -> /wss with TLS terminated at the
// platform edge).
func externalMultiaddr(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	var wsProto, defaultPort string
	switch u.Scheme {
	case "http":
		wsProto, defaultPort = "ws", "80"
	case "https":
		wsProto, defaultPort = "wss", "443"
	default:
		return "", fmt.Errorf("scheme %q not supported (use http or https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("URL has no host")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	hostProto := "dns4"
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			hostProto = "ip4"
		} else {
			hostProto = "ip6"
		}
	}
	addr := fmt.Sprintf("/%s/%s/tcp/%s/%s", hostProto, host, port, wsProto)
	if _, err := multiaddr.NewMultiaddr(addr); err != nil {
		return "", err
	}
	return addr, nil
}
