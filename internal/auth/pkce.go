// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE holds a cryptographic code verifier and its corresponding SHA-256 code challenge.
type PKCE struct {
	// Verifier is the 32-byte cryptographically random URL-safe base64 string.
	Verifier string

	// Challenge is the SHA-256 hash of the Verifier, URL-safe base64 encoded without padding.
	Challenge string
}

// GeneratePKCE generates a cryptographic 32-byte URL-safe base64-encoded verifier
// without padding and its corresponding SHA-256 challenge (method S256).
func GeneratePKCE() (*PKCE, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generating random bytes for PKCE verifier: %w", err)
	}

	verifier := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	return &PKCE{
		Verifier:  verifier,
		Challenge: challenge,
	}, nil
}

// GenerateState generates a cryptographic 24-byte URL-safe base64-encoded random state string
// used to mitigate Cross-Site Request Forgery (CSRF) attacks during OAuth authorization.
func GenerateState() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating random bytes for OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
