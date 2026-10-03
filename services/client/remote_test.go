package client

import (
	"net/url"
	"testing"
)

// A --remote is the proxy in front of serve, so a url with no port dials 443 and never serve's own 2376. (SHARD-466)
func TestARemoteWithNoPortDialsTheHTTPSDefault(t *testing.T) {
	for host, want := range map[string]string{
		"https://shard.example.com":      "shard.example.com:443",
		"https://shard.example.com/":     "shard.example.com:443",
		"https://shard.example.com:8443": "shard.example.com:8443",
		"https://[::1]":                  "[::1]:443",
		"https://127.0.0.1:2376":         "127.0.0.1:2376",
	} {
		parsed, err := url.Parse(host)
		if err != nil {
			t.Fatalf("parse %q: %v", host, err)
		}
		if got := remoteAddress(parsed); got != want {
			t.Errorf("%s dials %s, want %s", host, got, want)
		}
	}
}
