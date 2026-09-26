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

package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/sam/internal/standalone"
)

// TestStandaloneSDKAgents pins that both SDKs work with sam-one. Its
// embedded router listens on WebSocket alone, on the single port that also
// serves the control plane, so a member that dials TCP only never gets on.
// One agent example per language (sdk/js/examples/a2a-agent.ts,
// sdk/python/examples/a2a_agent.py) enrolls with the join token and accepts
// A2A requests; the other language's caller (its own when the other
// toolchain is missing) reaches it by peer ID through the router with the
// A2A SDK's client and gets an answer naming the verified caller. A language
// whose toolchain or A2A SDK is missing is skipped.
func TestStandaloneSDKAgents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv := startStandalone(t, ctx, standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir()})
	runStandaloneSDKAgents(t, ctx, srv, srv.PublicURL(), nil)
}

// TestStandaloneSDKAgentsBehindTLSEdge is the same pair behind what
// `sam-one --tunnel` puts in front of it: a TLS-terminating edge reached by
// name, which selects the origin by the TLS server name and the Host header
// and answers anything else with 403, as Cloudflare does. sam-one advertises
// its router as /dns4/localhost/tcp/<edge>/wss/p2p/<id>, so each SDK must
// carry the name to the edge instead of what it resolves to. Both SDKs
// trust the edge's certificate through the file their TLS stacks read
// (NODE_EXTRA_CA_CERTS, SSL_CERT_FILE).
func TestStandaloneSDKAgentsBehindTLSEdge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	edge, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	edgeURL := "https://localhost:" + fmt.Sprint(edge.Addr().(*net.TCPAddr).Port)
	srv := startStandalone(t, ctx, standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir(), ExternalURL: edgeURL})
	caFile := serveTLSEdge(t, edge, "localhost", srv.Addr())
	runStandaloneSDKAgents(t, ctx, srv, edgeURL, []string{"NODE_EXTRA_CA_CERTS=" + caFile, "SSL_CERT_FILE=" + caFile})
}

func startStandalone(t *testing.T, ctx context.Context, opts standalone.Options) *standalone.Server {
	t.Helper()
	srv, err := standalone.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// runStandaloneSDKAgents runs each language's A2A agent example against
// sam-one at publicURL and calls it with the other language's A2A caller.
func runStandaloneSDKAgents(t *testing.T, ctx context.Context, srv *standalone.Server, publicURL string, extraEnv []string) {
	t.Helper()
	root := repoRoot(t)
	var launchers []sdkExampleLauncher
	for _, l := range sdkExampleLaunchers {
		_, skip := l.cmd(ctx, root, "a2a-agent")
		if skip == "" {
			skip = a2aSDKMissing(root, l.name)
		}
		if skip != "" {
			t.Logf("%s SDK skipped: %s", l.name, skip)
			continue
		}
		launchers = append(launchers, l)
	}
	if len(launchers) == 0 {
		t.Skip("no SDK toolchain with the A2A SDK available; see sdk/README.md")
	}

	tokenPath := filepath.Join(t.TempDir(), "join-token")
	if err := os.WriteFile(tokenPath, []byte(srv.JoinToken()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(stateDir string) []string {
		return append([]string{
			"SAM_CONTROL_PLANE_URL=" + publicURL,
			"SAM_BOOTSTRAP_TOKEN_PATH=" + tokenPath,
			"SAM_STATE_DIR=" + filepath.Join(t.TempDir(), stateDir),
			"PYTHONUNBUFFERED=1",
		}, extraEnv...)
	}

	agents := make([]*sdkExampleAgent, len(launchers))
	errs := make([]error, len(launchers))
	var wg sync.WaitGroup
	for i, l := range launchers {
		cmd, _ := l.cmd(context.Background(), root, "a2a-agent")
		cmd.Env = append(os.Environ(), env(l.name+"-agent")...)
		cmd.Dir = root
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agents[i], errs[i] = startExampleAgent(t, launchers[i].name, cmd)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for i, l := range launchers {
		l, target := l, agents[(i+1)%len(agents)]
		t.Run(l.name+"-calls-"+target.name, func(t *testing.T) {
			t.Parallel()
			out := runExample(t, root, l, env(l.name+"-caller"), "a2a-call", target.peerID, "hello from "+l.name)
			caller := expectLine(t, out, "on the mesh as ")
			expectLine(t, out, "agent: Echo agent, ")
			if want := caller + " said: hello from " + l.name; !strings.Contains(out, want) {
				t.Fatalf("%s calling the %s agent through sam-one: want %q in\n%s", l.name, target.name, want, out)
			}
		})
	}
}

// serveTLSEdge terminates TLS on the listener for host and proxies to
// origin, HTTP and WebSocket upgrades alike. A handshake for another server
// name fails and a request for another host gets 403, the way an edge that
// routes by name treats a client that reached it by IP. Returns the path of
// the certificate the clients must trust.
func serveTLSEdge(t *testing.T, ln net.Listener, host, origin string) string {
	t.Helper()
	certPEM, keyPEM := selfSignedCert(t, host)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "edge-ca.pem")
	if err := os.WriteFile(caFile, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	target := &url.URL{Scheme: "http", Host: origin}
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.Host = origin
	}}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h, _, err := net.SplitHostPort(r.Host); (err == nil && !strings.EqualFold(h, host)) || (err != nil && !strings.EqualFold(r.Host, host)) {
				http.Error(w, "403 Forbidden (edge: unknown host "+r.Host+")", http.StatusForbidden)
				return
			}
			proxy.ServeHTTP(w, r)
		}),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if !strings.EqualFold(hello.ServerName, host) {
					return nil, errors.New("edge: unknown server name " + hello.ServerName)
				}
				return &cert, nil
			},
		},
	}
	go func() { _ = server.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	return caFile
}

// selfSignedCert is a self-signed certificate for host; clients trust it by adding it to their roots.
func selfSignedCert(t *testing.T, host string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
