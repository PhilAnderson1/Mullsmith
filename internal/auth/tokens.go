package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// NewSessionToken returns a 256-bit session token in raw (cookie value) and
// hashed (database value, sha256) form (spec §9.1).
func NewSessionToken() (raw string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("session token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashToken(raw), nil
}

// HashToken is the sha256 of the raw token string — the only form ever stored.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// NewCSRFToken returns a random hex token for the double-submit pair (spec §9.5).
func NewCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("csrf token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ConstantTimeEqual compares two strings in constant time.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Invite codes: "gk-" + 26 base32 chars (~130 bits).
func NewInviteCode() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("invite code: %w", err)
	}
	return "gk-" + base64.RawURLEncoding.EncodeToString(b), nil
}
