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

package standalone

import "testing"

func TestWsListenMultiaddr(t *testing.T) {
	cases := []struct {
		bind    string
		want    string
		wantErr bool
	}{
		{bind: "0.0.0.0:8080", want: "/ip4/0.0.0.0/tcp/8080/ws"},
		{bind: "127.0.0.1:0", want: "/ip4/127.0.0.1/tcp/0/ws"},
		{bind: ":9090", want: "/ip4/0.0.0.0/tcp/9090/ws"},
		{bind: "[::1]:8080", want: "/ip6/::1/tcp/8080/ws"},
		{bind: "example.com:8080", wantErr: true},
		{bind: "8080", wantErr: true},
	}
	for _, tc := range cases {
		got, err := wsListenMultiaddr(tc.bind)
		if tc.wantErr {
			if err == nil {
				t.Errorf("wsListenMultiaddr(%q) = %q, want error", tc.bind, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("wsListenMultiaddr(%q) failed: %v", tc.bind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("wsListenMultiaddr(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

func TestExternalMultiaddr(t *testing.T) {
	cases := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{url: "https://my-sam.a.run.app", want: "/dns4/my-sam.a.run.app/tcp/443/wss"},
		{url: "http://192-168-1-50.nip.io:8080", want: "/dns4/192-168-1-50.nip.io/tcp/8080/ws"},
		{url: "http://192.168.1.50:8080", want: "/ip4/192.168.1.50/tcp/8080/ws"},
		{url: "https://mesh.example:8443", want: "/dns4/mesh.example/tcp/8443/wss"},
		{url: "ftp://mesh.example", wantErr: true},
		{url: "http://", wantErr: true},
	}
	for _, tc := range cases {
		got, err := externalMultiaddr(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("externalMultiaddr(%q) = %q, want error", tc.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("externalMultiaddr(%q) failed: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("externalMultiaddr(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
