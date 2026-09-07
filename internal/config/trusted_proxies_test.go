package config

import (
	"testing"
)

func TestEdgePeerAllowed(t *testing.T) {
	management := RemoteManagement{TrustedProxies: []string{"172.18.0.0/16", "10.9.0.5", "::1", "not-a-cidr", ""}}
	tests := []struct {
		name    string
		address string
		allowed bool
	}{
		{name: "loopback v4", address: "127.0.0.1:40000", allowed: true},
		{name: "loopback v6", address: "[::1]:40000", allowed: true},
		{name: "bare loopback", address: "127.0.0.1", allowed: true},
		{name: "trusted cidr", address: "172.18.0.4:8317", allowed: true},
		{name: "trusted ip", address: "10.9.0.5:1234", allowed: true},
		{name: "outside cidr", address: "172.19.0.4:8317", allowed: false},
		{name: "public ip", address: "203.0.113.7:40000", allowed: false},
		{name: "garbage", address: "not-an-address", allowed: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := management.EdgePeerAllowed(tc.address); got != tc.allowed {
				t.Fatalf("EdgePeerAllowed(%q) = %v, want %v", tc.address, got, tc.allowed)
			}
		})
	}

	empty := RemoteManagement{}
	if !empty.EdgePeerAllowed("127.0.0.1:40000") {
		t.Fatal("empty trusted proxies must still allow loopback")
	}
	if empty.EdgePeerAllowed("172.18.0.4:8317") {
		t.Fatal("empty trusted proxies must deny non-loopback")
	}
}
