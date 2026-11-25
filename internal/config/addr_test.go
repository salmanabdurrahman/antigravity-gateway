// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import "testing"

// TestLoopbackHost verifies non-loopback and empty hosts are clamped to the default loopback address
// while the localhost alias and both IPv4/IPv6 loopback literals are preserved.
func TestLoopbackHost(t *testing.T) {
	cases := map[string]string{
		"":             DefaultHost,
		"0.0.0.0":      DefaultHost,
		"192.168.1.10": DefaultHost,
		"example.com":  DefaultHost,
		"127.0.0.1":    "127.0.0.1",
		"localhost":    "localhost",
		"::1":          "::1",
		"127.0.0.2":    "127.0.0.2",
	}

	for input, want := range cases {
		if got := LoopbackHost(input); got != want {
			t.Errorf("LoopbackHost(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestListenAddressAndURLIPv6 verifies IPv6 literals are bracketed in listen addresses and HTTP URLs
// so "::1" never produces the invalid "::1:51400" form.
func TestListenAddressAndURLIPv6(t *testing.T) {
	if got := ListenAddress("::1", 51400); got != "[::1]:51400" {
		t.Errorf("ListenAddress(::1) = %q, want [::1]:51400", got)
	}
	if got := ListenAddress("127.0.0.1", 51400); got != "127.0.0.1:51400" {
		t.Errorf("ListenAddress(127.0.0.1) = %q, want 127.0.0.1:51400", got)
	}
	if got := BaseURL("::1", 51400); got != "http://[::1]:51400" {
		t.Errorf("BaseURL(::1) = %q, want http://[::1]:51400", got)
	}
	if got := URL("::1", 51400, "/health"); got != "http://[::1]:51400/health" {
		t.Errorf("URL(::1, /health) = %q, want http://[::1]:51400/health", got)
	}
}
