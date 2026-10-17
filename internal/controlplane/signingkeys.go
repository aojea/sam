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
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/storage"
)

// operatorSigningKeys are the control plane's private signing keys when the
// operator supplies them (Options.SigningKeyPath) instead of letting the
// control plane generate and rotate its own. The operator then owns the root
// of trust: the keys live where the operator keeps them (a secret manager, a
// KMS-wrapped file) and rotate on the operator's schedule.
//
// The file is PEM. Each "PRIVATE KEY" (PKCS#8) or "EC PRIVATE KEY" (SEC 1)
// block is one key; Ed25519 keys sign Biscuits, P-256 keys sign the ES256
// JWTs behind /sts/token and /jwks. For each kind, the first key in the file
// signs and every later one only verifies, which is how a rotation is
// expressed: put the new key first, keep the previous one until every
// credential it signed has expired, then drop it.
type operatorSigningKeys struct {
	ed25519 []ed25519.PrivateKey
	es256   []*ecdsa.PrivateKey
	// digest of the file content these keys came from, so the watcher
	// reconciles only when the operator published something new.
	digest [sha256.Size]byte
}

// currentEd25519 is the public key the file names as the Biscuit signing
// key, nil when the file carries no Ed25519 key.
func (k *operatorSigningKeys) currentEd25519() ed25519.PublicKey {
	if k == nil || len(k.ed25519) == 0 {
		return nil
	}
	return k.ed25519[0].Public().(ed25519.PublicKey)
}

// loadOperatorSigningKeys reads and validates the PEM file at path.
func loadOperatorSigningKeys(path string) (*operatorSigningKeys, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing keys: %w", err)
	}
	keys, err := parseOperatorSigningKeys(path, data)
	if err != nil {
		return nil, err
	}
	keys.digest = sha256.Sum256(data)
	return keys, nil
}

// parseOperatorSigningKeys validates the PEM content of the file at path.
func parseOperatorSigningKeys(path string, data []byte) (*operatorSigningKeys, error) {
	var err error
	keys := &operatorSigningKeys{}
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		var parsed any
		switch block.Type {
		case "PRIVATE KEY":
			parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			parsed, err = x509.ParseECPrivateKey(block.Bytes)
		default:
			return nil, fmt.Errorf("signing keys %s: unsupported PEM block %q (want PRIVATE KEY or EC PRIVATE KEY)", path, block.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("signing keys %s: %w", path, err)
		}
		switch k := parsed.(type) {
		case ed25519.PrivateKey:
			keys.ed25519 = append(keys.ed25519, k)
		case *ecdsa.PrivateKey:
			if k.Curve != elliptic.P256() {
				return nil, fmt.Errorf("signing keys %s: EC key on %s, want P-256", path, k.Curve.Params().Name)
			}
			keys.es256 = append(keys.es256, k)
		default:
			return nil, fmt.Errorf("signing keys %s: unsupported key type %T (want Ed25519 or P-256)", path, parsed)
		}
	}
	if len(keys.ed25519) == 0 && len(keys.es256) == 0 {
		return nil, fmt.Errorf("signing keys %s: no private key found", path)
	}
	return keys, nil
}

// keyFingerprint identifies a public key in logs without revealing anything
// a verifier does not already learn from /keys or /jwks.
func keyFingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// reconcileEd25519Keyring makes the store's Biscuit keyring reflect keys:
// keys[0] current, the rest valid for verification until gracePeriod from
// now. It goes through the same SaveInitialKey / RotateKeys path as
// automatic rotation, so a store that already matches is left alone, a store
// that holds a key the operator does not know (one the control plane
// generated before the operator took over) keeps it valid for the grace
// period, and every verifier learns the new set from /keys the usual way.
func reconcileEd25519Keyring(ctx context.Context, store storage.Store, keys []ed25519.PrivateKey, gracePeriod time.Duration) error {
	if len(keys) == 0 {
		return nil
	}
	valid, err := store.GetAllValidKeys(ctx)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("read keyring: %w", err)
	}
	known := func(pub ed25519.PublicKey) bool {
		for _, k := range valid {
			if k.Public.Equal(pub) {
				return true
			}
		}
		return false
	}
	// Oldest first, so that the first key in the file ends up current.
	for i := len(keys) - 1; i >= 0; i-- {
		priv := keys[i]
		pub := priv.Public().(ed25519.PublicKey)
		if known(pub) {
			continue
		}
		if len(valid) == 0 {
			if err := store.SaveInitialKey(ctx, priv, pub); err != nil {
				return fmt.Errorf("save signing key %s: %w", keyFingerprint(pub), err)
			}
		} else if err := store.RotateKeys(ctx, priv, pub, gracePeriod); err != nil {
			return fmt.Errorf("install signing key %s: %w", keyFingerprint(pub), err)
		}
		valid = append(valid, storage.KeyPair{Private: priv, Public: pub})
		logger.Infow("Installed operator signing key", "algorithm", "ed25519", "fingerprint", keyFingerprint(pub))
	}
	_, currentPub, err := store.GetCurrentKey(ctx)
	if err != nil {
		return fmt.Errorf("read current signing key: %w", err)
	}
	if wantPub := keys[0].Public().(ed25519.PublicKey); !currentPub.Equal(wantPub) {
		// The first key in the file is known but not current, which happens
		// when the operator moves a previous key back to the front; the
		// store's current key becomes a verification-only key.
		if err := store.RotateKeys(ctx, keys[0], wantPub, gracePeriod); err != nil {
			return fmt.Errorf("make signing key %s current: %w", keyFingerprint(wantPub), err)
		}
		logger.Infow("Operator signing key made current", "algorithm", "ed25519", "fingerprint", keyFingerprint(wantPub))
	}
	return nil
}

// reconcileES256Keyring is reconcileEd25519Keyring for the ES256 keys behind
// /sts/token and /jwks. The key id is the one generateES256KeyEntry derives,
// so a verifier that pinned a kid from /jwks keeps matching.
func reconcileES256Keyring(ctx context.Context, store storage.Store, keys []*ecdsa.PrivateKey, gracePeriod time.Duration) error {
	if len(keys) == 0 {
		return nil
	}
	valid, err := store.GetAllValidOIDCKeys(ctx)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("read ES256 keyring: %w", err)
	}
	known := make(map[string]bool, len(valid))
	for _, k := range valid {
		known[k.Kid] = true
	}
	encode := func(priv *ecdsa.PrivateKey) (kid string, privBytes, pubBytes []byte, err error) {
		pubBytes, err = priv.PublicKey.Bytes()
		if err != nil || len(pubBytes) != 65 {
			return "", nil, nil, fmt.Errorf("encode P-256 public key: %w", err)
		}
		privBytes, err = x509.MarshalECPrivateKey(priv)
		if err != nil {
			return "", nil, nil, fmt.Errorf("encode P-256 private key: %w", err)
		}
		return es256Kid(pubBytes), privBytes, pubBytes, nil
	}
	for i := len(keys) - 1; i >= 0; i-- {
		kid, privBytes, pubBytes, err := encode(keys[i])
		if err != nil {
			return err
		}
		if known[kid] {
			continue
		}
		if len(known) == 0 {
			if err := store.SaveInitialOIDCKey(ctx, kid, privBytes, pubBytes); err != nil {
				return fmt.Errorf("save ES256 key %s: %w", kid, err)
			}
		} else if err := store.RotateOIDCKeys(ctx, kid, privBytes, pubBytes, gracePeriod); err != nil {
			return fmt.Errorf("install ES256 key %s: %w", kid, err)
		}
		known[kid] = true
		logger.Infow("Installed operator signing key", "algorithm", "es256", "kid", kid)
	}
	wantKid, privBytes, pubBytes, err := encode(keys[0])
	if err != nil {
		return err
	}
	current, err := store.GetCurrentOIDCKey(ctx)
	if err != nil {
		return fmt.Errorf("read current ES256 key: %w", err)
	}
	if current == nil || current.Kid != wantKid {
		if err := store.RotateOIDCKeys(ctx, wantKid, privBytes, pubBytes, gracePeriod); err != nil {
			return fmt.Errorf("make ES256 key %s current: %w", wantKid, err)
		}
		logger.Infow("Operator signing key made current", "algorithm", "es256", "kid", wantKid)
	}
	return nil
}

// installOperatorSigningKeys reconciles both keyrings from keys and reports
// whether the Biscuit signing key changed, so the caller can announce it.
func (s *Server) installOperatorSigningKeys(ctx context.Context, keys *operatorSigningKeys) (bool, error) {
	_, before, err := s.store.GetCurrentKey(ctx)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, fmt.Errorf("read current signing key: %w", err)
	}
	if err := reconcileES256Keyring(ctx, s.store, keys.es256, s.config.KeyGracePeriod); err != nil {
		return false, err
	}
	if err := reconcileEd25519Keyring(ctx, s.store, keys.ed25519, s.config.KeyGracePeriod); err != nil {
		return false, err
	}
	operatorSigningKeysGauge.WithLabelValues("ed25519").Set(float64(len(keys.ed25519)))
	operatorSigningKeysGauge.WithLabelValues("es256").Set(float64(len(keys.es256)))
	now := keys.currentEd25519()
	return now != nil && !before.Equal(now), nil
}

// operatorSigningKeysReloadInterval is how often the control plane looks at
// the key file for a change. A package variable so a test can shorten it.
var operatorSigningKeysReloadInterval = 30 * time.Second

// watchOperatorSigningKeys re-reads the key file on a timer and installs
// what changed, so that a rotation is the operator publishing a new file
// and nothing else: no restart, no redeploy. Polling rather than inotify,
// because a Kubernetes Secret volume updates through an atomic symlink swap
// and a Cloud Run secret volume through the platform, neither of which a
// watch on the file itself sees reliably. A file that fails to parse keeps
// the keys in use and is counted and logged; a change of the signing key
// is announced to the mesh the way automatic rotation announces one.
func (s *Server) watchOperatorSigningKeys() {
	defer s.wg.Done()
	ticker := time.NewTicker(operatorSigningKeysReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		s.reloadOperatorSigningKeys()
	}
}

// ReloadOperatorSigningKeys is one pass of the key file watcher, for callers
// that cannot wait for its timer (tests, an operator signal).
func (s *Server) ReloadOperatorSigningKeys() {
	if s.operatorKeys == nil {
		return
	}
	s.reloadOperatorSigningKeys()
}

// reloadOperatorSigningKeys is one pass of watchOperatorSigningKeys.
func (s *Server) reloadOperatorSigningKeys() {
	keys, err := loadOperatorSigningKeys(s.config.SigningKeyPath)
	if err != nil {
		operatorSigningKeysReloadErrors.Inc()
		logger.Warnw("Operator signing keys not reloaded; keeping the keys in use", "path", s.config.SigningKeyPath, "error", err)
		return
	}
	s.operatorKeysMu.Lock()
	unchanged := s.operatorKeys != nil && keys.digest == s.operatorKeys.digest
	s.operatorKeysMu.Unlock()
	if unchanged {
		return
	}
	changed, err := s.installOperatorSigningKeys(s.ctx, keys)
	if err != nil {
		operatorSigningKeysReloadErrors.Inc()
		logger.Errorw("Operator signing keys changed but could not be installed", "path", s.config.SigningKeyPath, "error", err)
		return
	}
	s.operatorKeysMu.Lock()
	s.operatorKeys = keys
	s.operatorKeysMu.Unlock()
	logger.Infow("Operator signing keys reloaded", "path", s.config.SigningKeyPath,
		"ed25519_keys", len(keys.ed25519), "es256_keys", len(keys.es256), "signing_key_changed", changed)
	if changed {
		if err := s.getMeshAdapter().PublishEvent(s.ctx, api.MeshEvent_KEY_ROTATION, "", keys.currentEd25519()); err != nil {
			logger.Warnf("Failed to publish KEY_ROTATION event to mesh: %v", err)
		}
	}
}
