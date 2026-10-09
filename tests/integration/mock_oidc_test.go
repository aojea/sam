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
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// mockOIDCUser is the subject the mock provider signs in as whoever
// completes its browser authorization flow.
const mockOIDCUser = "browser-user"

// startCustomMockOIDC is the identity provider the mesh trusts in these
// tests: discovery, a JWKS, and the authorization code grant with PKCE that
// an interactive join drives. The tokens it issues are real RS256 JWTs a
// control plane verifies against the JWKS. mintToken issues one directly,
// for components that are configured with a token instead of logging in.
func startCustomMockOIDC(t *testing.T) (string, func(claims map[string]interface{}) string) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rsa key: %v", err)
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer := srv.URL

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"issuer":                 issuer,
			"jwks_uri":               issuer + "/keys",
			"authorization_endpoint": issuer + "/auth",
			"token_endpoint":         issuer + "/token",
		})
	})

	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"keys": []map[string]interface{}{
				{
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"kid": "mock-key",
					"n":   base64.RawURLEncoding.EncodeToString(privKey.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privKey.E)).Bytes()),
				},
			},
		})
	})

	mintToken := func(customClaims map[string]interface{}) string {
		claims := jwt.MapClaims{
			"iss": issuer,
			"aud": "agentmesh-audience",
			"exp": time.Now().Add(time.Hour).Unix(),
		}
		for k, v := range customClaims {
			claims[k] = v
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "mock-key"
		jwtStr, err := token.SignedString(privKey)
		if err != nil {
			t.Fatalf("failed to sign jwt: %v", err)
		}
		return jwtStr
	}

	// Authorization codes handed out by /auth, redeemed once at /token.
	type authCode struct {
		challenge   string
		redirectURI string
		offline     bool
	}
	var mu sync.Mutex
	codes := map[string]authCode{}
	newCode := func() string {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			t.Fatalf("random code: %v", err)
		}
		return hex.EncodeToString(b)
	}

	// The user opens this URL; the provider signs them in as mockOIDCUser
	// and sends them back to the client with a code bound to its PKCE
	// challenge.
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("response_type") != "code" || q.Get("client_id") == "" || q.Get("redirect_uri") == "" || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "unsupported authorization request", http.StatusBadRequest)
			return
		}
		code := newCode()
		mu.Lock()
		codes[code] = authCode{
			challenge:   q.Get("code_challenge"),
			redirectURI: q.Get("redirect_uri"),
			offline:     strings.Contains(q.Get("scope"), "offline_access"),
		}
		mu.Unlock()
		back, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		bq := back.Query()
		bq.Set("code", code)
		bq.Set("state", q.Get("state"))
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})

	oauthError := func(w http.ResponseWriter, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			oauthError(w, "invalid_request")
			return
		}
		if r.PostForm.Get("grant_type") != "authorization_code" {
			oauthError(w, "unsupported_grant_type")
			return
		}
		mu.Lock()
		granted, ok := codes[r.PostForm.Get("code")]
		delete(codes, r.PostForm.Get("code"))
		mu.Unlock()
		verifier := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || granted.redirectURI != r.PostForm.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(verifier[:]) != granted.challenge {
			oauthError(w, "invalid_grant")
			return
		}
		idToken := mintToken(map[string]interface{}{
			"sub":   mockOIDCUser,
			"email": mockOIDCUser + "@example.com",
		})
		resp := map[string]interface{}{
			"access_token": idToken,
			"id_token":     idToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if granted.offline {
			resp["refresh_token"] = newCode()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	return issuer, mintToken
}
