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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/standalone"
	"github.com/google/sam/internal/storage"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// adminClient talks to a running sam-one's admin API; subcommands never open
// the database from a second process.
type adminClient struct {
	client *http.Client
	server string
	token  string
}

// resolveAdminToken picks the admin credential: explicit flag, then the
// SAM_ADMIN_TOKEN env, then the token persisted in data-dir by a previous run.
func resolveAdminToken(flagVal, dataDir string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := os.Getenv("SAM_ADMIN_TOKEN"); env != "" {
		return env, nil
	}
	tok, err := standalone.AdminTokenFromDataDir(dataDir)
	if err != nil {
		return "", fmt.Errorf("no admin token: pass --admin-token, set SAM_ADMIN_TOKEN, or point --data-dir at a sam-one data directory (%v)", err)
	}
	return tok, nil
}

func (c *adminClient) do(method, path, contentType string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, c.server+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(respBody))
	}
	return respBody, nil
}

type createdToken struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
}

func (c *adminClient) createToken(role string, ttlHours, maxUsages int, description string) (*createdToken, error) {
	payload, err := json.Marshal(map[string]any{
		"role":        role,
		"ttl_hours":   ttlHours,
		"max_usages":  maxUsages,
		"description": description,
	})
	if err != nil {
		return nil, err
	}
	body, err := c.do(http.MethodPost, "/admin/bootstrap-tokens", "application/json", payload)
	if err != nil {
		return nil, err
	}
	var created createdToken
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("failed to decode response %q: %w", body, err)
	}
	return &created, nil
}

func (c *adminClient) listTokens() ([]storage.BootstrapToken, error) {
	body, err := c.do(http.MethodGet, "/admin/bootstrap-tokens", "", nil)
	if err != nil {
		return nil, err
	}
	var list []storage.BootstrapToken
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("failed to decode response %q: %w", body, err)
	}
	return list, nil
}

func (c *adminClient) banPeer(peerID string) error {
	payload, err := proto.Marshal(&api.TokenRevokeRequest{PeerId: peerID})
	if err != nil {
		return err
	}
	_, err = c.do(http.MethodPost, "/admin/revoke", "application/x-protobuf", payload)
	return err
}

// newAdminSubcommands wires the token and admin command trees onto root.
func newAdminSubcommands() []*cobra.Command {
	var (
		server        string
		adminToken    string
		dataDir       string
		clientFactory = func() (*adminClient, error) {
			tok, err := resolveAdminToken(adminToken, dataDir)
			if err != nil {
				return nil, err
			}
			return &adminClient{
				client: &http.Client{Timeout: 10 * time.Second},
				server: server,
				token:  tok,
			}, nil
		}
	)

	addSharedFlags := func(cmd *cobra.Command) {
		cmd.PersistentFlags().StringVar(&server, "server", "http://127.0.0.1:8080", "Base URL of the running sam-one server")
		cmd.PersistentFlags().StringVar(&adminToken, "admin-token", "", "Admin API bearer token (or env SAM_ADMIN_TOKEN, or read from --data-dir)")
		cmd.PersistentFlags().StringVar(&dataDir, "data-dir", ".", "sam-one data directory holding the persisted admin token")
	}

	var (
		role        string
		ttlHours    int
		maxUsages   int
		description string
	)
	tokenCreate := &cobra.Command{
		Use:   "create",
		Short: "Generate a new scoped bootstrap token",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFactory()
			if err != nil {
				return err
			}
			created, err := c.createToken(role, ttlHours, maxUsages, description)
			if err != nil {
				return err
			}
			cmd.Printf("Token:    %s\n", created.Token)
			cmd.Printf("Role:     %s\n", created.Role)
			cmd.Printf("Expires:  %s\n", created.ExpiresAt)
			cmd.Println("The plain token is shown only once; store it now.")
			return nil
		},
	}
	tokenCreate.Flags().StringVar(&role, "role", api.RoleNode, "Role bound to the token")
	tokenCreate.Flags().IntVar(&ttlHours, "ttl-hours", 24, "Token validity in hours")
	tokenCreate.Flags().IntVar(&maxUsages, "max-usages", 1, "How many enrollments the token allows")
	tokenCreate.Flags().StringVar(&description, "description", "", "Free-form note stored with the token")

	tokenList := &cobra.Command{
		Use:   "list",
		Short: "List active bootstrap tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFactory()
			if err != nil {
				return err
			}
			list, err := c.listTokens()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tROLE\tUSAGES\tEXPIRES\tDESCRIPTION")
			for _, tok := range list {
				id := tok.ID
				if len(id) > 12 {
					id = id[:12]
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\n",
					id, tok.Role, tok.UsagesCount, tok.MaxUsages,
					tok.ExpiresAt.Format(time.RFC3339), tok.Description)
			}
			return tw.Flush()
		},
	}

	tokenCmd := &cobra.Command{Use: "token", Short: "Manage bootstrap tokens on a running sam-one"}
	addSharedFlags(tokenCmd)
	tokenCmd.AddCommand(tokenCreate, tokenList)

	adminBan := &cobra.Command{
		Use:   "ban <peer-id>",
		Short: "Ban a peer ID from the mesh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFactory()
			if err != nil {
				return err
			}
			if err := c.banPeer(args[0]); err != nil {
				return err
			}
			cmd.Printf("Peer %s banned\n", args[0])
			return nil
		},
	}

	adminCmd := &cobra.Command{Use: "admin", Short: "Administrative actions on a running sam-one"}
	addSharedFlags(adminCmd)
	adminCmd.AddCommand(adminBan)

	return []*cobra.Command{tokenCmd, adminCmd}
}
