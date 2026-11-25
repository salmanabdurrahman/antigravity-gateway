// Package config defines gateway configuration data structures, filesystem paths,
// environment overrides, and upstream Google HTTP headers.
package config

import (
	"cmp"
	"context"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// PreferIPv4Env opts in to IPv4-first dialing. It exists for hosts whose published IPv6 route
// blackholes SYN packets, which otherwise stalls every dual-stack connection until Happy Eyeballs
// falls back.
const PreferIPv4Env = "AGW_PREFER_IPV4"

// NewTransport constructs an optimized *http.Transport configured for multiplexed,
// low-latency communication with Google Cloud Code Assist and Google OAuth endpoints.
// It sets high per-host connection pools to avoid connection churn, enables HTTP/2,
// bounds response header latency, and honors standard proxy environment variables.
func NewTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           newDialContext(dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// newDialContext returns the transport dialing function. The default keeps the standard Happy Eyeballs
// dialer untouched; AGW_PREFER_IPV4 switches to an explicit IPv4-first ordering that still falls back
// to IPv6 addresses when the IPv4 path fails.
func newDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	base := dialer.DialContext
	if !preferIPv4() {
		return base
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" {
			return base(ctx, network, addr)
		}

		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil || net.ParseIP(host) != nil {
			return base(ctx, network, addr)
		}

		addrs, lookupErr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if lookupErr != nil || len(addrs) == 0 {
			return base(ctx, network, addr)
		}
		slices.SortStableFunc(addrs, func(a, b net.IPAddr) int {
			return cmp.Compare(ipFamilyRank(a.IP), ipFamilyRank(b.IP))
		})

		var lastErr error
		for _, candidate := range addrs {
			conn, dialErr := dialer.DialContext(ctx, "tcp", net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

// preferIPv4 reports whether the operator opted in to IPv4-first dialing.
func preferIPv4() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(PreferIPv4Env))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ipFamilyRank orders IPv4 addresses ahead of IPv6 addresses for the opt-in dialer.
func ipFamilyRank(ip net.IP) int {
	if ip.To4() != nil {
		return 0
	}
	return 1
}

// DefaultHTTPClient returns an *http.Client configured with NewTransport and the provided timeout.
func DefaultHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewTransport(),
		Timeout:   timeout,
	}
}
