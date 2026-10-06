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

package tlsinspect

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func buildTestClientHelloRecord(serverName string, includeECH bool) []byte {
	var exts []byte
	if serverName != "" {
		nameBytes := []byte(serverName)
		sniEntry := make([]byte, 3+len(nameBytes))
		sniEntry[0] = 0x00 // host_name
		binary.BigEndian.PutUint16(sniEntry[1:3], uint16(len(nameBytes)))
		copy(sniEntry[3:], nameBytes)

		sniList := make([]byte, 2+len(sniEntry))
		binary.BigEndian.PutUint16(sniList[0:2], uint16(len(sniEntry)))
		copy(sniList[2:], sniEntry)

		ext := make([]byte, 4+len(sniList))
		binary.BigEndian.PutUint16(ext[0:2], ExtServerName)
		binary.BigEndian.PutUint16(ext[2:4], uint16(len(sniList)))
		copy(ext[4:], sniList)
		exts = append(exts, ext...)
	}
	if includeECH {
		echPayload := []byte{0x01, 0x02, 0x03, 0x04}
		ext := make([]byte, 4+len(echPayload))
		binary.BigEndian.PutUint16(ext[0:2], ExtEncryptedClientHello)
		binary.BigEndian.PutUint16(ext[2:4], uint16(len(echPayload)))
		copy(ext[4:], echPayload)
		exts = append(exts, ext...)
	}

	var body []byte
	body = append(body, 0x03, 0x03)       // legacy_version TLS 1.2
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0x00)             // session_id length = 0
	body = append(body, 0x00, 0x02, 0x13, 0x01) // cipher_suites (TLS_AES_128_GCM_SHA256)
	body = append(body, 0x01, 0x00)       // compression_methods (null)
	if len(exts) > 0 {
		extBlock := make([]byte, 2+len(exts))
		binary.BigEndian.PutUint16(extBlock[0:2], uint16(len(exts)))
		copy(extBlock[2:], exts)
		body = append(body, extBlock...)
	}

	hs := make([]byte, 4+len(body))
	hs[0] = HandshakeTypeClientHello
	hs[1] = byte(len(body) >> 16)
	hs[2] = byte(len(body) >> 8)
	hs[3] = byte(len(body))
	copy(hs[4:], body)

	rec := make([]byte, 5+len(hs))
	rec[0] = RecordTypeHandshake
	rec[1] = 0x03
	rec[2] = 0x01
	binary.BigEndian.PutUint16(rec[3:5], uint16(len(hs)))
	copy(rec[5:], hs)
	return rec
}

func TestVerifyClientHello(t *testing.T) {
	t.Run("matching_sni_and_case_insensitive_normalization", func(t *testing.T) {
		raw := buildTestClientHelloRecord("DB.Internal.Example.COM.", false)
		gotRaw, gotSNI, err := VerifyClientHello(bytes.NewReader(raw), "db.internal.example.com")
		if err != nil {
			t.Fatalf("VerifyClientHello: %v", err)
		}
		if gotSNI != "db.internal.example.com" {
			t.Fatalf("normalizedSNI = %q, want db.internal.example.com", gotSNI)
		}
		if !bytes.Equal(gotRaw, raw) {
			t.Fatalf("RawRecord mismatch")
		}
	})

	t.Run("mismatched_sni_rejected", func(t *testing.T) {
		raw := buildTestClientHelloRecord("evil.example.com", false)
		_, _, err := VerifyClientHello(bytes.NewReader(raw), "db.internal.example.com")
		if err == nil || !strings.Contains(err.Error(), "does not match destination") {
			t.Fatalf("expected SNI mismatch error, got %v", err)
		}
	})

	t.Run("missing_sni_rejected", func(t *testing.T) {
		raw := buildTestClientHelloRecord("", false)
		_, _, err := VerifyClientHello(bytes.NewReader(raw), "db.internal.example.com")
		if err == nil || !strings.Contains(err.Error(), "missing SNI") {
			t.Fatalf("expected missing SNI error, got %v", err)
		}
	})

	t.Run("ech_extension_rejected", func(t *testing.T) {
		raw := buildTestClientHelloRecord("db.internal.example.com", true)
		_, _, err := VerifyClientHello(bytes.NewReader(raw), "db.internal.example.com")
		if err == nil || !strings.Contains(err.Error(), "Encrypted Client Hello") {
			t.Fatalf("expected ECH error, got %v", err)
		}
	})

	t.Run("trailing_bytes_after_handshake_rejected", func(t *testing.T) {
		raw := buildTestClientHelloRecord("db.internal.example.com", false)
		// Increase record length by 4 bytes to simulate a second smuggled handshake message in the same record.
		recLen := int(binary.BigEndian.Uint16(raw[3:5])) + 4
		binary.BigEndian.PutUint16(raw[3:5], uint16(recLen))
		raw = append(raw, 0x01, 0x00, 0x00, 0x00)
		_, _, err := VerifyClientHello(bytes.NewReader(raw), "db.internal.example.com")
		if err == nil || !strings.Contains(err.Error(), "trailing bytes or multiple handshake messages") {
			t.Fatalf("expected trailing bytes error, got %v", err)
		}
	})

	t.Run("non_handshake_record_rejected", func(t *testing.T) {
		raw := []byte{0x17, 0x03, 0x03, 0x00, 0x02, 0xaa, 0xbb}
		_, _, err := VerifyClientHello(bytes.NewReader(raw), "db.internal.example.com")
		if err == nil || !strings.Contains(err.Error(), "expected TLS Handshake record") {
			t.Fatalf("expected non-handshake record error, got %v", err)
		}
	})
}
