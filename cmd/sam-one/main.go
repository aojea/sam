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

// sam-one is the all-in-one SAM distribution: control plane, libp2p router
// and storage in a single binary serving a single public port.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/standalone"
	golog "github.com/ipfs/go-log/v2"
	"github.com/spf13/cobra"
)

var logger = golog.Logger("sam-one")

func main() {
	var (
		bindAddress          string
		port                 int
		externalURL          string
		p2pListen            []string
		dataDir              string
		dbDriver             string
		dbDSN                string
		joinToken            string
		adminToken           string
		policyFile           string
		oidcIssuer           string
		oidcClientID         string
		allowedAudiencesFlag string
		logLevel             string
		cpTunables           standalone.ControlPlaneTunables
		routerTunables       standalone.RouterTunables
		routerAllowLoopback  bool
	)

	rootCmd := &cobra.Command{
		Use:   "sam-one",
		Short: "Sovereign Agent Mesh - all-in-one standalone server",
		Run: func(cmd *cobra.Command, args []string) {
			if os.Getenv("LOG_FORMAT") == "json" {
				_ = os.Setenv("GOLOG_LOG_FMT", "json")
			}
			golog.SetAllLoggers(golog.LevelInfo)
			if logLevel != "" {
				if lvl, err := golog.LevelFromString(logLevel); err == nil {
					golog.SetAllLoggers(lvl)
				}
			}

			// Env fallbacks keep single-container platforms (Cloud Run)
			// configurable without flags.
			if joinToken == "" {
				joinToken = os.Getenv("SAM_TOKEN")
			}
			if adminToken == "" {
				adminToken = os.Getenv("SAM_ADMIN_TOKEN")
			}
			if externalURL == "" {
				externalURL = os.Getenv("SAM_EXTERNAL_URL")
			}

			var auds []string
			for _, aud := range strings.Split(allowedAudiencesFlag, ",") {
				if aud = strings.TrimSpace(aud); aud != "" {
					auds = append(auds, aud)
				}
			}

			routerTunables.DisallowLoopback = !routerAllowLoopback
			srv, err := standalone.New(standalone.Options{
				BindAddress:      net.JoinHostPort(bindAddress, strconv.Itoa(port)),
				ExternalURL:      externalURL,
				P2PListen:        p2pListen,
				DataDir:          dataDir,
				DBDriver:         dbDriver,
				DBDSN:            dbDSN,
				JoinToken:        joinToken,
				AdminToken:       adminToken,
				PolicyFile:       policyFile,
				OIDCIssuer:       oidcIssuer,
				OIDCClientID:     oidcClientID,
				AllowedAudiences: auds,
				ControlPlane:     cpTunables,
				Router:           routerTunables,
			})
			if err != nil {
				logger.Fatalf("Invalid configuration: %v", err)
			}
			if err := srv.Start(cmd.Context()); err != nil {
				logger.Fatalf("Failed to start: %v", err)
			}
			defer func() {
				if err := srv.Close(); err != nil {
					logger.Errorf("Shutdown: %v", err)
				}
			}()

			printBanner(srv, externalURL)
			<-cmd.Context().Done()
		},
	}

	rootCmd.Flags().StringVar(&bindAddress, "bind-address", "0.0.0.0", "Host/IP to bind the single HTTP/WebSocket listener")
	rootCmd.Flags().IntVar(&port, "port", 0, "TCP port of the single listener; 0 picks a free port, published in the startup banner")
	rootCmd.Flags().StringVar(&externalURL, "external-url", "", "Public URL reachable by nodes (or env SAM_EXTERNAL_URL)")
	rootCmd.Flags().StringSliceVar(&p2pListen, "p2p-listen", nil, "Optional extra native libp2p listen multiaddrs")
	rootCmd.Flags().StringVar(&dataDir, "data-dir", ".", "Directory for the database, router key and generated tokens")
	rootCmd.Flags().StringVar(&dbDriver, "db-driver", "sqlite", "Database driver (sqlite or postgres)")
	rootCmd.Flags().StringVar(&dbDSN, "db-dsn", "", "Database DSN (default <data-dir>/sam.db for sqlite)")
	rootCmd.Flags().StringVar(&joinToken, "token", "", "Cluster join token (or env SAM_TOKEN; auto-generated if empty)")
	rootCmd.Flags().StringVar(&adminToken, "admin-token", "", "Admin API bearer token (or env SAM_ADMIN_TOKEN; auto-generated if empty)")
	rootCmd.Flags().StringVar(&policyFile, "policy-file", "", "Path to a protojson PolicyConfigUpdateRequest seeding the mesh policy on first boot only")
	rootCmd.Flags().StringVar(&oidcIssuer, "issuer", "", "Optional external OIDC issuer URL (comma-separated)")
	rootCmd.Flags().StringVar(&oidcClientID, "oidc-client-id", "", "OAuth client id advertised via /info (defaults to the first allowed audience)")
	rootCmd.Flags().StringVar(&allowedAudiencesFlag, "allowed-audiences", api.DefaultAudience, "Comma-separated list of allowed OIDC audiences")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "", "Log level: debug, info, warn, error")

	// Embedded control plane tunables.
	rootCmd.Flags().DurationVar(&cpTunables.LeaseDuration, "control-plane-lease-duration", 0, "Router lease validity (0 keeps the component default)")
	rootCmd.Flags().DurationVar(&cpTunables.KeyRotationInterval, "control-plane-key-rotation-interval", 0, "Biscuit signing key rotation interval (0 keeps the component default)")
	rootCmd.Flags().DurationVar(&cpTunables.KeyGracePeriod, "control-plane-key-grace-period", 0, "How long rotated-out keys stay valid for verification (0 keeps the component default)")
	rootCmd.Flags().DurationVar(&cpTunables.BiscuitTTL, "control-plane-biscuit-ttl", 0, "Lifespan minted into issued biscuits (0 keeps the component default)")
	rootCmd.Flags().BoolVar(&cpTunables.ManualEnrollment, "control-plane-manual-enrollment", false, "Queue bootstrap enrollments for admin approval instead of auto-approving")

	// Embedded router tunables.
	rootCmd.Flags().DurationVar(&routerTunables.KeysSyncInterval, "router-keys-sync-interval", 0, "Biscuit public key refresh interval (0 keeps the component default)")
	rootCmd.Flags().DurationVar(&routerTunables.LeaseRenewInterval, "router-lease-renew-interval", 0, "Lease renewal interval (0 keeps the component default)")
	rootCmd.Flags().IntVar(&routerTunables.LowWaterMark, "router-low-watermark", 0, "Connection manager low watermark (0 keeps the component default)")
	rootCmd.Flags().IntVar(&routerTunables.HighWaterMark, "router-high-watermark", 0, "Connection manager high watermark (0 keeps the component default)")
	rootCmd.Flags().IntVar(&routerTunables.ConnsPerSourceIP, "router-conns-per-source-ip", 0, "Per-source-IP connection budget (0 follows the high watermark; proxied peers share source IPs)")
	rootCmd.Flags().DurationVar(&routerTunables.DHTProviderAddrTTL, "router-dht-provider-addr-ttl", 0, "DHT provider address TTL (0 keeps the library default)")
	rootCmd.Flags().DurationVar(&routerTunables.DHTMaxRecordAge, "router-dht-max-record-age", 0, "DHT record max age (0 keeps the library default)")
	rootCmd.Flags().BoolVar(&routerAllowLoopback, "router-allow-loopback", true, "Advertise loopback addresses (disable on public deployments)")

	rootCmd.AddCommand(newAdminSubcommands()...)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func printBanner(srv *standalone.Server, externalURL string) {
	base := externalURL
	if base == "" {
		base = "http://" + srv.Addr()
	}
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println("SAM standalone mesh is ready!")
	fmt.Println()
	fmt.Printf("API URL:      %s\n", base)
	fmt.Printf("Web Console:  %s/console\n", base)
	fmt.Printf("Router Peer:  %s\n", srv.PeerID())
	fmt.Printf("Admin Token:  %s\n", srv.AdminToken())
	fmt.Printf("Join Token:   %s\n", srv.JoinToken())
	fmt.Println()
	fmt.Println("To enroll a node:")
	fmt.Printf("  sam-node join %s --bootstrap-token %s\n", base, srv.JoinToken())
	fmt.Println("══════════════════════════════════════════════════════════════════")
}
