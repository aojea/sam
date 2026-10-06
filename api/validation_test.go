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
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestValidateServiceFormat(t *testing.T) {
	tests := []struct {
		name    string
		svc     string
		wantErr bool
	}{
		{"valid exact", "mcp://my-service.local", false},
		{"valid prefix wildcard", "mcp://*.service.local", false},
		{"valid suffix wildcard", "mcp://service.*", false},
		{"valid just wildcard", "mcp://*", false},
		{"valid subdomains", "mcp://a.b.c.d", false},
		{"invalid no type", "://my-service", true},
		{"invalid fallback", "mcp-service", true},
		{"invalid consecutive dots", "mcp://my..service", true},
		{"invalid wildcard middle", "mcp://my.*.service", true},
		{"valid with underscore", "mcp://service_name", false},
		{"invalid suffix wildcard without dot", "mcp://service.inc*", true},
		{"valid exact with path", "mcp://my-service/local", false},
		{"invalid legacy mcp: format", "mcp:my-service.local", true},
		{"invalid with query", "mcp://my-service?query=1", true},
		{"invalid with fragment", "mcp://my-service#fragment", true},
		{"invalid with query and fragment", "mcp://my-service?query=1#fragment", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateServiceFormat(tt.svc)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateServiceFormat() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTargetFormat(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"valid target", "group:backend", false},
		{"valid email", "email:foo@bar.com", false},
		{"valid wildcard", "*", false},
		{"wildcard fact and value", "*:*", false},
		{"wildcard value for one fact", "group:*", false},
		{"invalid no colon", "group-backend", true},
		{"invalid empty fact", ":backend", true},
		{"invalid empty value", "group:", true},
		// A fact nothing derives would mint a grant that never matches, so it
		// is refused at config time rather than denying silently at runtime.
		{"unknown fact", "banana:yellow", true},
		{"agent is not a target", "agent:reviewer-7.prod.acme.example", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTargetFormat(tt.target)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateTargetFormat() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateServiceAnnounce(t *testing.T) {
	validAnnounce := func() *ServiceAnnounce {
		return &ServiceAnnounce{
			PeerId:       "12D3KooWA4Xop1JaT3MHxwYMkCepYsv4iPVopMXwCz5iHYdBfeSB",
			Type:         ServiceType_SERVICE_TYPE_MCP,
			ServiceName:  "weather",
			Keys:         []string{"get_weather"},
			Labels:       map[string]string{"env": "prod"},
			AnnounceTime: timestamppb.Now(),
		}
	}
	if err := ValidateServiceAnnounce(validAnnounce()); err != nil {
		t.Fatalf("ValidateServiceAnnounce(valid) = %v", err)
	}

	epochAnnounce := validAnnounce()
	epochAnnounce.AnnounceTime.Seconds = 0
	epochAnnounce.AnnounceTime.Nanos = 0
	if err := ValidateServiceAnnounce(epochAnnounce); err == nil {
		t.Fatal("expected epoch AnnounceTime to be rejected")
	}

	badLabelAnnounce := validAnnounce()
	badLabelAnnounce.Labels = map[string]string{"bad key!": "val"}
	if err := ValidateServiceAnnounce(badLabelAnnounce); err == nil {
		t.Fatal("expected invalid label key in ServiceAnnounce to be rejected")
	}
}
