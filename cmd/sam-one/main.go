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
	"os"
	"os/signal"
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
		externalURL          string
		p2pListen            []string
		dataDir              string
		dbDriver             string
		dbDSN                string
		joinToken            string
		adminToken           string
		policyFile           string
		oidcIssuer           string
		allowedAudiencesFlag string
		logLevel             string
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

			srv, err := standalone.New(standalone.Options{
				BindAddress:      bindAddress,
				ExternalURL:      externalURL,
				P2PListen:        p2pListen,
				DataDir:          dataDir,
				DBDriver:         dbDriver,
				DBDSN:            dbDSN,
				JoinToken:        joinToken,
				AdminToken:       adminToken,
				PolicyFile:       policyFile,
				OIDCIssuer:       oidcIssuer,
				AllowedAudiences: auds,
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

	rootCmd.Flags().StringVar(&bindAddress, "bind-address", "0.0.0.0:8080", "Address to bind the single HTTP/WebSocket listener")
	rootCmd.Flags().StringVar(&externalURL, "external-url", "", "Public URL reachable by nodes (or env SAM_EXTERNAL_URL)")
	rootCmd.Flags().StringSliceVar(&p2pListen, "p2p-listen", nil, "Optional extra native libp2p listen multiaddrs")
	rootCmd.Flags().StringVar(&dataDir, "data-dir", ".", "Directory for the database, router key and generated tokens")
	rootCmd.Flags().StringVar(&dbDriver, "db-driver", "sqlite", "Database driver (sqlite or postgres)")
	rootCmd.Flags().StringVar(&dbDSN, "db-dsn", "", "Database DSN (default <data-dir>/sam.db for sqlite)")
	rootCmd.Flags().StringVar(&joinToken, "token", "", "Cluster join token (or env SAM_TOKEN; auto-generated if empty)")
	rootCmd.Flags().StringVar(&adminToken, "admin-token", "", "Admin API bearer token (or env SAM_ADMIN_TOKEN; auto-generated if empty)")
	rootCmd.Flags().StringVar(&policyFile, "policy-file", "", "Path to a protojson PolicyConfigUpdateRequest seeding the mesh policy on first boot only")
	rootCmd.Flags().StringVar(&oidcIssuer, "issuer", "", "Optional external OIDC issuer URL (comma-separated)")
	rootCmd.Flags().StringVar(&allowedAudiencesFlag, "allowed-audiences", api.DefaultAudience, "Comma-separated list of allowed OIDC audiences")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "", "Log level: debug, info, warn, error")

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
	fmt.Printf("Router Peer:  %s\n", srv.PeerID())
	fmt.Printf("Admin Token:  %s\n", srv.AdminToken())
	fmt.Printf("Join Token:   %s\n", srv.JoinToken())
	fmt.Println()
	fmt.Println("To enroll a node:")
	fmt.Printf("  sam-node join %s --bootstrap-token %s\n", base, srv.JoinToken())
	fmt.Println("══════════════════════════════════════════════════════════════════")
}
