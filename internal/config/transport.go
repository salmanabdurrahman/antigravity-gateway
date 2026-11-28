// Package config defines gateway configuration data structures, filesystem paths,
// environment overrides, and upstream Google HTTP headers.
package config

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// PreferIPv4Env opts in to IPv4-first dialing. It exists for hosts whose published IPv6 route
// blackholes SYN packets, which otherwise stalls every dual-stack connection until Happy Eyeballs
// falls back.
const PreferIPv4Env = "AGW_PREFER_IPV4"

var (
	// sharedDefaultTransport reuses a single HTTP/2 connection pool when IPv4-first dialing is disabled.
	sharedDefaultTransport = sync.OnceValue(func() *http.Transport {
		return newTransportForMode(false)
	})

	// sharedIPv4Transport reuses a single HTTP/2 connection pool when AGW_PREFER_IPV4 is enabled.
	sharedIPv4Transport = sync.OnceValue(func() *http.Transport {
		return newTransportForMode(true)
	})
)

// NewTransport constructs an optimized *http.Transport configured for multiplexed,
// low-latency communication with Google Cloud Code Assist and Google OAuth endpoints.
// It sets high per-host connection pools to avoid connection churn, enables HTTP/2,
// bounds response header latency, and honors standard proxy environment variables.
func NewTransport() *http.Transport {
	return newTransportForMode(preferIPv4())
}

// sharedTransport returns the process-wide pooled *http.Transport matching the current
// AGW_PREFER_IPV4 setting so short-lived clients reuse TLS and HTTP/2 connections instead of
// allocating fresh idle connection pools on every call. Keeping it unexported prevents external
// callers from mutating the shared pool configuration.
func sharedTransport() *http.Transport {
	if preferIPv4() {
		return sharedIPv4Transport()
	}
	return sharedDefaultTransport()
}

// newTransportForMode builds a configured *http.Transport with either Happy Eyeballs or IPv4-first dialing.
func newTransportForMode(ipv4First bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           newDialContext(dialer, ipv4First),
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
func newDialContext(dialer *net.Dialer, ipv4First bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	base := dialer.DialContext
	if !ipv4First {
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

// DefaultHTTPClient returns an *http.Client sharing the pooled gateway transport with the
// provided request timeout.
func DefaultHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: sharedTransport(),
		Timeout:   timeout,
	}
}

// WireModeEnv selects how the gateway speaks to upstream Google endpoints.
const WireModeEnv = "AGW_WIRE_MODE"

const (
	// WireModeHTTP1 is the default wire mode: one HTTP/1.1 connection pool per credential, without
	// ALPN negotiation, matching the native Antigravity client's per-process wire shape.
	WireModeHTTP1 = "http1"
	// WireModeAuto restores the legacy process-wide HTTP/2 multiplexed pool.
	WireModeAuto = "auto"

	// credentialTransportLimit bounds how many per-credential HTTP/1.1 pools stay alive. An idle pool
	// costs a handful of connections, and eviction only closes idle ones, so the cap exists purely to
	// stop unbounded growth when credentials churn.
	credentialTransportLimit = 32
)

// WireMode returns the normalized `AGW_WIRE_MODE` value, defaulting to WireModeHTTP1 for any unset or
// unrecognized value. The escape hatch exists so an operator can restore the legacy shared HTTP/2 pool
// without downgrading the binary.
func WireMode() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(WireModeEnv)), WireModeAuto) {
		return WireModeAuto
	}
	return WireModeHTTP1
}

// CredentialKey derives an opaque, non-reversible connection-pool key for one Google credential. It
// prefers the refresh token because that value survives access-token rotation, then a desktop bearer
// token, and finally the account identity. Only a truncated digest is returned, so the key is safe to
// use in logs and maps without exposing credential material.
func CredentialKey(refreshToken, accessToken, fallback string) string {
	secret := cmp.Or(strings.TrimSpace(refreshToken), strings.TrimSpace(accessToken), strings.ToLower(strings.TrimSpace(fallback)))
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return "k:" + hex.EncodeToString(sum[:8])
}

// CredentialHTTPClient returns an HTTP client scoped to one credential. In the default wire mode it
// uses a dedicated HTTP/1.1 pool so two Google identities never share a TCP/TLS connection; the legacy
// auto mode falls back to the shared multiplexed client.
func CredentialHTTPClient(timeout time.Duration, credentialKey string) *http.Client {
	if WireMode() == WireModeAuto {
		return DefaultHTTPClient(timeout)
	}
	return &http.Client{
		Transport: credentialTransport(credentialKey),
		Timeout:   timeout,
	}
}

var (
	// credentialTransportsMu guards the per-credential pool registry below.
	credentialTransportsMu sync.Mutex

	// credentialTransports maps a credential key to its HTTP/1.1 pool, in insertion order.
	credentialTransports = make(map[string]*http.Transport)

	// credentialTransportOrder tracks insertion order so the oldest idle pool can be evicted.
	credentialTransportOrder []string

	// anonymousTransport serves callers with no credential identity, avoiding an unbounded pool per
	// anonymous call while still keeping the HTTP/1.1 wire shape.
	anonymousTransport = sync.OnceValue(func() *http.Transport {
		return newHTTP1Transport()
	})
)

// credentialTransport returns the memoized HTTP/1.1 pool for credentialKey, evicting the oldest pool
// once the bounded registry is full. Eviction closes idle connections only, so an in-flight request
// keeps its connection.
func credentialTransport(credentialKey string) *http.Transport {
	if credentialKey == "" {
		return anonymousTransport()
	}

	credentialTransportsMu.Lock()
	defer credentialTransportsMu.Unlock()

	if transport, ok := credentialTransports[credentialKey]; ok {
		return transport
	}

	if len(credentialTransportOrder) >= credentialTransportLimit {
		oldest := credentialTransportOrder[0]
		credentialTransportOrder = credentialTransportOrder[1:]
		if evicted, ok := credentialTransports[oldest]; ok {
			evicted.CloseIdleConnections()
			delete(credentialTransports, oldest)
		}
	}

	transport := newHTTP1Transport()
	credentialTransports[credentialKey] = transport
	credentialTransportOrder = append(credentialTransportOrder, credentialKey)
	return transport
}

// newHTTP1Transport builds a transport matching the native client's wire shape: HTTP/1.1 only, no ALPN
// protocol list, and a small idle pool so connections do not look like a long-lived server process.
func newHTTP1Transport() *http.Transport {
	transport := newTransportForMode(preferIPv4())
	transport.ForceAttemptHTTP2 = false
	// Wipe TLSNextProto so net/http cannot implicitly upgrade through an already-negotiated HTTP/2
	// connection, and leave NextProtos empty so the ClientHello advertises no ALPN protocol at all.
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = nil
	transport.MaxIdleConns = 8
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = 30 * time.Second
	return transport
}
