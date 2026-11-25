// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"net"
	"strconv"
)

// LoopbackHost clamps host to the default loopback interface unless it is the localhost alias or an
// IP literal that is genuinely loopback.
//
// The gateway must never be reachable from other machines, so a mis-typed configuration (an empty
// value, a LAN address, or a public hostname) is silently coerced back to 127.0.0.1 instead of
// widening the bind surface. IPv6 loopback (::1) is preserved so operators can opt into IPv6.
func LoopbackHost(host string) string {
	trimmed := host
	if trimmed == "localhost" {
		return trimmed
	}
	if ip := net.ParseIP(trimmed); ip != nil && ip.IsLoopback() {
		return trimmed
	}
	return DefaultHost
}

// ListenAddress formats a host and port into a network address accepted by net.Listen.
//
// IPv6 literals must be bracketed, so "::1" with port 51400 becomes "[::1]:51400". The naive
// "host:port" concatenation produces the invalid "::1:51400" and fails with
// "too many colons in address".
func ListenAddress(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// BaseURL returns the loopback HTTP base URL for a host and port, with IPv6 brackets applied.
func BaseURL(host string, port int) string {
	return "http://" + ListenAddress(host, port)
}

// URL returns an absolute HTTP URL for a path on the loopback gateway, formatting the authority with
// IPv6-safe bracketing. The path is expected to start with a slash.
func URL(host string, port int, path string) string {
	return BaseURL(host, port) + path
}
