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
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"strings"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/parser"
	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"
)

// RequestContext carries the security metadata for a specific stream request
type RequestContext struct {
	PeerID   peer.ID
	User     string
	Group    string
	Protocol protocol.ID
}

// verifyStreamAuth encapsulates the logic for verifying a stream's AuthFrame and sending the AuthResponse.
func (n *SamNode) verifyStreamAuth(s network.Stream) error {
	remotePeer := s.Conn().RemotePeer()

	if !n.rateLimiter.Allow(remotePeer.String()) {
		_ = s.Reset()
		return fmt.Errorf("rate limit exceeded for %s", remotePeer)
	}

	reader := msgio.NewVarintReaderSize(s, 1024*64)
	msg, err := reader.ReadMsg()
	if err != nil {
		return fmt.Errorf("failed to read auth frame from %s: %w", remotePeer, err)
	}
	defer reader.ReleaseMsg(msg)

	var authFrame api.AuthFrame
	if err := proto.Unmarshal(msg, &authFrame); err != nil {
		return fmt.Errorf("invalid auth frame from %s", remotePeer)
	}

	reqCtx := RequestContext{
		PeerID:   remotePeer,
		User:     "", // Not used in Authorize
		Protocol: s.Protocol(),
	}

	writer := msgio.NewVarintWriter(s)

	if n.Store.IsBanned(remotePeer) {
		resp := &api.AuthResponse{Success: false, Error: "Peer is explicitly BANNED"}
		respBytes, _ := proto.Marshal(resp)
		_ = writer.WriteMsg(respBytes)
		return fmt.Errorf("peer %s is explicitly BANNED", remotePeer)
	}

	n.keysMu.RLock()
	keys := n.trustedKeys
	n.keysMu.RUnlock()

	var authorized bool
	var lastErr error
	for _, pubKey := range keys {
		logger.Infof("[Auth] Trying key: %x", pubKey.Key)
		if err := n.Authorize(authFrame.Biscuit, reqCtx, pubKey.Key); err == nil {
			authorized = true
			break
		} else {
			lastErr = err
		}
	}

	if !authorized {
		logger.Infof("[Auth] All keys failed, triggering re-enrollment fallback for %s", remotePeer)
		var jwtStr string
		var fallbackErr error

		if tokenURLFlag != "" {
			jwtStr, fallbackErr = n.FetchJWT(context.Background(), tokenURLFlag, clientIDFlag, clientSecretFlag)
			if fallbackErr != nil {
				logger.Errorf("[Auth] Failed to fetch JWT for fallback: %v", fallbackErr)
			}
		} else if jwtPathFlag != "" {
			data, fileErr := os.ReadFile(jwtPathFlag)
			if fileErr != nil {
				logger.Errorf("[Auth] Failed to read JWT file for fallback: %v", fileErr)
			} else {
				jwtStr = strings.TrimSpace(string(data))
			}
		}

		if jwtStr != "" {
			enrollErr := n.Enroll(context.Background(), jwtStr)
			if enrollErr != nil {
				logger.Errorf("[Auth] Fallback enrollment failed: %v", enrollErr)
			} else {
				n.keysMu.RLock()
				keys = n.trustedKeys
				n.keysMu.RUnlock()

				for _, pubKey := range keys {
					logger.Infof("[Auth] Retrying with key: %x", pubKey.Key)
					if err := n.Authorize(authFrame.Biscuit, reqCtx, pubKey.Key); err == nil {
						authorized = true
						break
					} else {
						lastErr = err
					}
				}
			}
		}
	}

	if !authorized {
		resp := &api.AuthResponse{Success: false, Error: "Authorization failed"}
		if lastErr != nil {
			resp.Error = lastErr.Error()
		}
		respBytes, _ := proto.Marshal(resp)
		_ = writer.WriteMsg(respBytes)
		return fmt.Errorf("AuthZ Denied %s: %v", remotePeer, lastErr)
	}

	// Valid
	resp := &api.AuthResponse{Success: true}
	respBytes, _ := proto.Marshal(resp)
	if err := writer.WriteMsg(respBytes); err != nil {
		return fmt.Errorf("failed to write ACK to %s: %w", remotePeer, err)
	}

	return nil
}

// WithBiscuitAuth enforces a Protobuf handshake on a stream before calling the next handler.
func (n *SamNode) WithBiscuitAuth(next network.StreamHandler) network.StreamHandler {
	return func(s network.Stream) {
		defer func() {
			if err := s.Close(); err != nil {
				logger.Errorf("[Auth] Failed to close stream: %v", err)
			}
		}()

		if err := n.verifyStreamAuth(s); err != nil {
			logger.Warnf("[Auth] Stream verification failed: %v", err)
			return
		}

		next(s)
	}
}

func (n *SamNode) Authorize(rawToken []byte, req RequestContext, pubKey ed25519.PublicKey) error {
	if len(pubKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public key size: %d", len(pubKey))
	}
	b, err := biscuit.Unmarshal(rawToken)
	if err != nil {
		return fmt.Errorf("invalid biscuit: %w", err)
	}

	authorizer, err := b.Authorizer(pubKey)
	if err != nil {
		return err
	}

	// Verify that the token is bound to the connecting peer's ID
	boundFact := biscuit.Fact{Predicate: biscuit.Predicate{
		Name: "node",
		IDs:  []biscuit.Term{biscuit.String(req.PeerID.String())},
	}}
	if _, err := b.GetBlockID(boundFact); err != nil {
		return fmt.Errorf("token is not bound to peer %s", req.PeerID)
	}

	// Inject the current action context (Standard Vocabulary)
	authorizer.AddFact(biscuit.Fact{
		Predicate: biscuit.Predicate{
			Name: "operation",
			IDs:  []biscuit.Term{biscuit.String(req.Protocol)},
		},
	})

	// Inject connection_peer_id fact for replay defense
	authorizer.AddFact(biscuit.Fact{
		Predicate: biscuit.Predicate{
			Name: "connection_peer_id",
			IDs:  []biscuit.Term{biscuit.String(req.PeerID.String())},
		},
	})

	// Enforce client_peer_id matches connection_peer_id
	replayCheckStr := `check if client_peer_id($id), connection_peer_id($id)`
	replayCheck, err := parser.FromStringCheck(replayCheckStr)
	if err != nil {
		return fmt.Errorf("failed to parse replay check: %w", err)
	}
	authorizer.AddCheck(replayCheck)

	// Apply Pre-compiled Local Attenuation
	if n.LocalPolicy != nil {
		for _, p := range n.LocalPolicy.Policies {
			authorizer.AddPolicy(p)
		}
		for _, c := range n.LocalPolicy.Checks {
			authorizer.AddCheck(c)
		}
		for _, r := range n.LocalPolicy.Rules {
			authorizer.AddRule(r)
		}
	}

	// Baseline Rules
	rule1Str := fmt.Sprintf(`allow if operation($op), %s($op)`, api.FactMCPTool)
	rule1, err := parser.FromStringPolicy(rule1Str)
	if err != nil {
		return fmt.Errorf("failed to parse baseline rule 1: %w", err)
	}
	authorizer.AddPolicy(rule1)

	rule2Str := fmt.Sprintf(`allow if operation($op), %s("*")`, api.FactMCPTool)
	rule2, err := parser.FromStringPolicy(rule2Str)
	if err != nil {
		return fmt.Errorf("failed to parse baseline rule 2: %w", err)
	}
	authorizer.AddPolicy(rule2)

	return authorizer.Authorize()
}
