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

package api

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

// The keys are the sha256 multihash of a fixed name, so a record a member
// announces in any language lands where the others look it up. The vectors
// here are shared with the SDK tests.
func TestDirectoryKeys(t *testing.T) {
	mcp, err := ServiceKey(ServiceType_SERVICE_TYPE_MCP, "")
	if err != nil {
		t.Fatal(err)
	}
	calc, err := ServiceKey(ServiceType_SERVICE_TYPE_MCP, "calculator")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(mcp, calc) {
		t.Fatal("a service type and one of its services share a key")
	}
	for name, key := range map[string][]byte{"mcp": mcp, "mcp:calculator": calc} {
		if err := ValidateDirectoryKey(key); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// sha256("mesh:service:mcp:calculator") behind the multihash header.
	const want = "1220" + "e99b7ec759ced5276520910d75b1f6fe3a5baf5fe46655ac34957139e04a0859"
	if got := hex.EncodeToString(calc); got != want {
		t.Fatalf("calculator key = %s, want %s", got, want)
	}

	if _, err := ServiceKey(ServiceType_SERVICE_TYPE_UNSPECIFIED, "x"); err == nil {
		t.Error("an unspecified service type got a key")
	}

	p, err := peer.Decode("12D3KooWA4Xop1JaT3MHxwYMkCepYsv4iPVopMXwCz5iHYdBfeSB")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDirectoryKey(PeerKey(p)); err != nil {
		t.Error(err)
	}
	if bytes.Equal(PeerKey(p), mcp) {
		t.Error("a peer key collides with a service key")
	}
	for _, bad := range [][]byte{nil, make([]byte, 33), append([]byte{0x11, 0x20}, make([]byte, 32)...)} {
		if err := ValidateDirectoryKey(bad); err == nil {
			t.Errorf("key %x accepted", bad)
		}
	}
}
