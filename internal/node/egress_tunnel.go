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
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/tlsinspect"
	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// HeaderSamEgressPort carries the requested TCP destination port when a
	// CONNECT tunnel is forwarded across /libp2p-http.
	HeaderSamEgressPort = "X-Sam-Egress-Port"
	// HeaderSamTunnelUpgrade marks an HTTP/1.1 request over /libp2p-http as a
	// raw TCP CONNECT tunnel.
	HeaderSamTunnelUpgrade = "sam-tcp-tunnel"

	clientHelloReadTimeout = 5 * time.Second
)

func (s *EgressService) isAllowedTCPPort(port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	ports := s.destination.GetPorts()
	if len(ports) == 0 {
		return false
	}
	return slices.Contains(ports, uint32(port))
}

func (s *EgressService) resolveTCPTargetAddr(reqPort int) string {
	if s.destination.GetTargetUrl() != "" && s.target != nil {
		if p := s.target.Port(); p != "" {
			return s.target.Host
		}
		return net.JoinHostPort(s.target.Hostname(), strconv.Itoa(reqPort))
	}
	return net.JoinHostPort(s.destination.GetName(), strconv.Itoa(reqPort))
}

func (s *EgressService) allowsLocalTarget() bool {
	if s.destination.GetTargetUrl() == "" || s.target == nil {
		return false
	}
	h := strings.TrimSpace(s.target.Hostname())
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		unmapped := ip.Unmap()
		return unmapped.IsLoopback() || unmapped.IsPrivate()
	}
	return false
}

func isForbiddenEgressAddr(addr netip.Addr, allowLocal bool) bool {
	ip := addr.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if !allowLocal {
		if ip.IsLoopback() || ip.IsPrivate() {
			return true
		}
		if ip.Is4() {
			b := ip.As4()
			// RFC 6598 Carrier-Grade NAT (100.64.0.0/10)
			if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
				return true
			}
		}
	}
	return false
}

func dialSafeEgressTCP(ctx context.Context, addr string, allowLocal bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if parsed, pErr := netip.ParseAddr(host); pErr == nil {
		ips = []netip.Addr{parsed}
	} else {
		resolved, rErr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if rErr != nil {
			return nil, rErr
		}
		ips = resolved
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses resolved for %s", host)
	}
	var allowed []netip.Addr
	for _, ip := range ips {
		if !isForbiddenEgressAddr(ip, allowLocal) {
			allowed = append(allowed, ip)
		}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("egress dial to %s refused: resolved IP is in a forbidden range", host)
	}
	var d net.Dialer
	var lastErr error
	for _, ip := range allowed {
		conn, dErr := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if dErr == nil {
			return conn, nil
		}
		lastErr = dErr
	}
	return nil, lastErr
}

// ServeTunnel handles a named TCP CONNECT tunnel on an EGRESS_MODE_TCP
// EgressService. It enforces the ports allow-list before hijacking, reads the
// client's TLS ClientHello and verifies that SNI matches destination.Name (and
// that ECH is absent) before dialing upstream, and audits bytes in each
// direction and duration.
func (s *EgressService) ServeTunnel(ctx context.Context, w http.ResponseWriter, r *http.Request, reqPort int) {
	callerCtx := s.extractCallerContext(ctx)
	if s.destination.GetMode() != api.EgressMode_EGRESS_MODE_TCP {
		recordEgressDecision(s.info.Name, egressOutcomeDeny)
		refuse(w, http.StatusForbidden, fmt.Sprintf("egress destination %q is HTTP-only; TCP tunnels are not permitted", s.info.Name), proxyStatusDenied)
		return
	}
	if !s.isAllowedTCPPort(reqPort) {
		recordEgressDecision(s.info.Name, egressOutcomeDeny)
		logger.Warnw("Egress TCP Tunnel Verdict",
			"destination", s.info.Name,
			"port", reqPort,
			"sam_task", callerCtx.task,
			"verdict", "deny_port",
		)
		refuse(w, http.StatusForbidden, fmt.Sprintf("egress destination %q does not allow TCP port %d", s.info.Name, reqPort), proxyStatusDenied)
		return
	}

	rc := http.NewResponseController(w)
	clientConn, clientBuf, err := rc.Hijack()
	if err != nil {
		http.Error(w, "TCP tunnel hijacking not supported", http.StatusInternalServerError)
		return
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	var reader io.Reader = clientConn
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buffered, _ := clientBuf.Peek(clientBuf.Reader.Buffered())
		_, _ = clientBuf.Discard(len(buffered))
		reader = io.MultiReader(bytes.NewReader(buffered), clientConn)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(clientHelloReadTimeout))
	rawHello, sni, err := tlsinspect.VerifyClientHello(reader, s.destination.GetName())
	_ = clientConn.SetReadDeadline(time.Time{})
	if err != nil {
		recordEgressDecision(s.info.Name, egressOutcomeDeny)
		logger.Warnw("Egress TCP Tunnel Verdict",
			"destination", s.info.Name,
			"port", reqPort,
			"sni", sni,
			"sam_task", callerCtx.task,
			"verdict", "deny_sni",
			"error", err.Error(),
		)
		return
	}

	targetAddr := s.resolveTCPTargetAddr(reqPort)
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	upstreamConn, err := dialSafeEgressTCP(dialCtx, targetAddr, s.allowsLocalTarget())
	cancel()
	if err != nil {
		logger.Warnw("Egress TCP Tunnel Verdict",
			"destination", s.info.Name,
			"port", reqPort,
			"sni", sni,
			"sam_task", callerCtx.task,
			"verdict", "dial_error",
			"error", err.Error(),
		)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	start := time.Now()
	if _, err := upstreamConn.Write(rawHello); err != nil {
		return
	}

	var txBytes, rxBytes int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstreamConn, reader)
		txBytes = int64(len(rawHello)) + n
		if cw, ok := upstreamConn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = upstreamConn.Close()
		}
	}()
	go func() {
		defer wg.Done()
		rxBytes, _ = io.Copy(clientConn, upstreamConn)
		if cw, ok := clientConn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = clientConn.Close()
		}
	}()
	wg.Wait()

	logger.Infow("Egress TCP Tunnel Verdict",
		"destination", s.info.Name,
		"port", reqPort,
		"sni", sni,
		"sam_task", callerCtx.task,
		"verdict", "allow",
		"bytes_tx", txBytes,
		"bytes_rx", rxBytes,
		"duration", time.Since(start).String(),
	)
}

// withConnectTunnel intercepts HTTP CONNECT requests at the sidecar server,
// authenticates the caller via withCallerOrTokenAuth, and either serves the
// tunnel locally (when this node serves egress://<host>) or forwards it across
// the mesh to a serving egress node.
func withConnectTunnel(node *SamNode, token string, next http.Handler) http.Handler {
	authedConnect := withCallerOrTokenAuth(node, token, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleConnectTunnel(node, w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			next.ServeHTTP(w, r)
			return
		}
		if proxyAuth := r.Header.Get("Proxy-Authorization"); proxyAuth != "" && r.Header.Get(api.HeaderSamAuthentication) == "" {
			r.Header.Set(api.HeaderSamAuthentication, proxyAuth)
		}
		r.Header.Del("Proxy-Authorization")
		authedConnect.ServeHTTP(w, r)
	})
}

func parseConnectHostPort(r *http.Request) (string, int, error) {
	rawHost := r.Host
	if rawHost == "" {
		rawHost = r.URL.Host
	}
	host, portStr, err := net.SplitHostPort(rawHost)
	if err != nil {
		return "", 0, fmt.Errorf("CONNECT target %q must be host:port", rawHost)
	}
	host = api.NormalizeMeshHost(host)
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	if err := api.ValidateEgressName(host); err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func handleConnectTunnel(node *SamNode, w http.ResponseWriter, r *http.Request) {
	host, port, err := parseConnectHostPort(r)
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error(), proxyStatusDenied)
		return
	}
	if node == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	identity := node.GetRequestIdentity(r.Context())
	if len(identity) == 0 {
		http.Error(w, "node has no credential yet", http.StatusServiceUnavailable)
		return
	}

	// 1. If this node serves egress://<host> locally, authorize and serve directly.
	if node.services != nil {
		if svc, ok := node.services.GetTyped(api.ServiceType_SERVICE_TYPE_EGRESS, host); ok {
			es, ok := svc.(*EgressService)
			if !ok {
				refuse(w, http.StatusNotFound, "invalid egress service", proxyStatusDestinationNotFound)
				return
			}
			var localPID peer.ID
			if pid, pErr := node.localPeerID(); pErr == nil {
				localPID = pid
			}
			reqCtx := RequestContext{
				PeerID:   localPID,
				Protocol: "local-api",
				Target:   api.EgressServicePrefix + host,
				HTTP:     &HTTPRequestFacts{Method: http.MethodConnect, Path: ""},
				Egress:   &EgressFacts{Host: host, Port: port},
				Local:    true,
			}
			if err := node.VerifyBiscuitToken(identity, reqCtx); err != nil {
				recordEgressDecision(host, egressOutcomeDeny)
				refuse(w, http.StatusForbidden, "Authorization failed", proxyStatusDenied)
				return
			}
			recordEgressDecision(host, egressOutcomeAllow)
			es.ServeTunnel(WithCallerBiscuit(r.Context(), identity), w, r, port)
			return
		}
	}

	// 2. Otherwise discover a remote provider in the mesh and forward the tunnel over /libp2p-http.
	if node.Host == nil {
		refuse(w, http.StatusNotFound, fmt.Sprintf("no egress destination %q is assigned to this node", host), proxyStatusDestinationNotFound)
		return
	}
	providers, err := node.DiscoverRemoteServices(r.Context(), api.ServiceType_SERVICE_TYPE_EGRESS, host)
	if err != nil || len(providers) == 0 {
		refuse(w, http.StatusNotFound, fmt.Sprintf("no provider found for egress://%s", host), proxyStatusDestinationNotFound)
		return
	}
	targetPeer, err := peer.Decode(providers[0].GetPeerId())
	if err != nil {
		refuse(w, http.StatusBadGateway, "invalid provider peer ID", proxyStatusConfigurationError)
		return
	}
	forwardConnectTunnelToPeer(node, w, r, targetPeer, host, port, identity)
}

func forwardConnectTunnelToPeer(node *SamNode, w http.ResponseWriter, r *http.Request, targetPeer peer.ID, host string, port int, biscuitBytes []byte) {
	dialCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	meshConn, err := gostream.Dial(dialCtx, node.Host, targetPeer, "/libp2p-http")
	cancel()
	if err != nil {
		refuse(w, http.StatusBadGateway, fmt.Sprintf("failed to dial egress peer: %v", err), proxyStatusConfigurationError)
		return
	}
	defer func() { _ = meshConn.Close() }()

	reqLine := fmt.Sprintf("GET /egress/%s HTTP/1.1\r\nHost: %s:%d\r\nConnection: Upgrade\r\nUpgrade: %s\r\n%s: %d\r\n%s: %s\r\n\r\n",
		host,
		host,
		port,
		HeaderSamTunnelUpgrade,
		HeaderSamEgressPort,
		port,
		api.HeaderSamBiscuit,
		base64.StdEncoding.EncodeToString(biscuitBytes),
	)
	if _, err := io.WriteString(meshConn, reqLine); err != nil {
		refuse(w, http.StatusBadGateway, "failed to send tunnel request to egress peer", proxyStatusConfigurationError)
		return
	}

	meshBuf := bufio.NewReader(meshConn)
	resp, err := http.ReadResponse(meshBuf, r)
	if err != nil {
		refuse(w, http.StatusBadGateway, "failed to read tunnel response from egress peer", proxyStatusConfigurationError)
		return
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if ps := resp.Header.Get("Proxy-Status"); ps != "" {
			w.Header().Set("Proxy-Status", ps)
		}
		http.Error(w, strings.TrimSpace(string(body)), resp.StatusCode)
		return
	}

	rc := http.NewResponseController(w)
	clientConn, clientBuf, err := rc.Hijack()
	if err != nil {
		http.Error(w, "TCP tunnel hijacking not supported", http.StatusInternalServerError)
		return
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	var clientReader io.Reader = clientConn
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buffered, _ := clientBuf.Peek(clientBuf.Reader.Buffered())
		_, _ = clientBuf.Discard(len(buffered))
		clientReader = io.MultiReader(bytes.NewReader(buffered), clientConn)
	}

	var meshReader io.Reader = meshConn
	if meshBuf.Buffered() > 0 {
		buffered, _ := meshBuf.Peek(meshBuf.Buffered())
		_, _ = meshBuf.Discard(len(buffered))
		meshReader = io.MultiReader(bytes.NewReader(buffered), meshConn)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(meshConn, clientReader)
		_ = meshConn.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, meshReader)
		_ = clientConn.Close()
	}()
	wg.Wait()
}
