// Package config defines gateway configuration data structures, filesystem paths,
// environment overrides, and upstream Google HTTP headers.
package config

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCredentialKeyIsOpaqueAndDeterministic verifies that the pool key is stable for one credential, the
// same value across callers, distinct across credentials, and never contains the secret it derives from.
func TestCredentialKeyIsOpaqueAndDeterministic(t *testing.T) {
	refresh := "refresh-token-alpha"
	key := CredentialKey(refresh, "", "user@example.com")

	if again := CredentialKey(refresh, "", "user@example.com"); again != key {
		t.Errorf("CredentialKey is not deterministic: %q vs %q", key, again)
	}
	if strings.Contains(key, refresh) || strings.Contains(key, "user@example.com") {
		t.Errorf("CredentialKey leaked credential material: %q", key)
	}
	if other := CredentialKey("refresh-token-beta", "", "user@example.com"); other == key {
		t.Errorf("CredentialKey collided across credentials: %q", key)
	}
	if got := CredentialKey("", "desktop-access-token", "user@example.com"); got == key {
		t.Errorf("access-token-derived key collided with refresh-token key: %q", got)
	}
	if got := CredentialKey("", "", "user@example.com"); got == "" {
		t.Error("CredentialKey returned empty for an email-only credential")
	}
	if got := CredentialKey("", "", ""); got != "" {
		t.Errorf("CredentialKey returned %q for an empty credential; want empty", got)
	}
}

// TestWireModeSelection verifies the AGW_WIRE_MODE escape hatch defaults to HTTP/1.1 and only trips on an
// explicit auto value.
func TestWireModeSelection(t *testing.T) {
	tests := []struct {
		// name labels the environment value under test.
		name string
		// value is the AGW_WIRE_MODE value.
		value string
		// want is the resolved wire mode.
		want string
	}{
		{name: "unset defaults to http1", value: "", want: WireModeHTTP1},
		{name: "explicit http1", value: "http1", want: WireModeHTTP1},
		{name: "auto opts into shared pool", value: "auto", want: WireModeAuto},
		{name: "case insensitive auto", value: "  AUTO  ", want: WireModeAuto},
		{name: "unknown value stays http1", value: "http2", want: WireModeHTTP1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(WireModeEnv, tt.value)
			if got := WireMode(); got != tt.want {
				t.Errorf("WireMode() = %q; want %q", got, tt.want)
			}
		})
	}
}

// TestCredentialHTTPClientUsesHTTP11Pools verifies the default wire mode yields one HTTP/1.1 transport per
// credential, with no ALPN protocol list and no implicit HTTP/2 upgrade path.
func TestCredentialHTTPClientUsesHTTP11Pools(t *testing.T) {
	t.Setenv(WireModeEnv, WireModeHTTP1)

	client := CredentialHTTPClient(5*time.Second, "k:alpha")
	if client.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v; want 5s", client.Timeout)
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T; want *http.Transport", client.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true; want false for the native wire shape")
	}
	if len(transport.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto has %d entry/entries; want none so HTTP/2 cannot be negotiated", len(transport.TLSNextProto))
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil; want an explicit empty ALPN list")
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 0 {
		t.Errorf("NextProtos = %v; want empty so the ClientHello advertises no ALPN", got)
	}
	if transport.MaxIdleConnsPerHost != 2 {
		t.Errorf("MaxIdleConnsPerHost = %d; want 2", transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != 30*time.Second {
		t.Errorf("IdleConnTimeout = %v; want 30s", transport.IdleConnTimeout)
	}

	// One pool per credential, shared across callers that resolve the same key.
	again, ok := CredentialHTTPClient(0, "k:alpha").Transport.(*http.Transport)
	if !ok || again != transport {
		t.Error("CredentialHTTPClient did not reuse the pool for the same credential key")
	}
	other, ok := CredentialHTTPClient(0, "k:beta").Transport.(*http.Transport)
	if !ok || other == transport {
		t.Error("CredentialHTTPClient shared one pool across two credential keys")
	}

	// Callers without a credential identity share a single anonymous pool instead of growing one each.
	anonA, _ := CredentialHTTPClient(0, "").Transport.(*http.Transport)
	anonB, _ := CredentialHTTPClient(0, "").Transport.(*http.Transport)
	if anonA == nil || anonA != anonB {
		t.Error("CredentialHTTPClient allocated more than one anonymous pool")
	}
}

// TestCredentialHTTPClientAutoModeUsesSharedTransport verifies the rollback escape hatch restores the
// process-wide multiplexed client.
func TestCredentialHTTPClientAutoModeUsesSharedTransport(t *testing.T) {
	t.Setenv(WireModeEnv, WireModeAuto)

	client := CredentialHTTPClient(3*time.Second, "k:alpha")
	if client.Transport != DefaultHTTPClient(0).Transport {
		t.Error("auto wire mode did not reuse the shared transport")
	}
}

// TestCredentialHTTPClientNegotiatesNoALPN verifies the no-ALPN claim against a TLS server that advertises
// HTTP/2: the credential pool must still speak HTTP/1.1 with an empty negotiated protocol.
func TestCredentialHTTPClientNegotiatesNoALPN(t *testing.T) {
	t.Setenv(WireModeEnv, WireModeHTTP1)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	client := CredentialHTTPClient(5*time.Second, "k:tls-alpn-test")
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T; want *http.Transport", client.Transport)
	}

	// The credential pool is shared, so the test installs the test server's CA on its own transport only.
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig.RootCAs = roots

	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET against TLS test server: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.ProtoMajor != 1 {
		t.Errorf("negotiated HTTP/%d; want HTTP/1.1", response.ProtoMajor)
	}
	if response.TLS == nil {
		t.Fatal("expected a TLS connection")
	}
	if got := response.TLS.NegotiatedProtocol; got != "" {
		t.Errorf("NegotiatedProtocol = %q; want empty so the client advertises no ALPN", got)
	}
}
