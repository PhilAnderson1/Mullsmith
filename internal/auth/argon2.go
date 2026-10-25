// Package auth holds credential hashing, session tokens, cookies, password
// policy and throttling primitives (spec §9.1, §9.5).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"gabkeep/internal/config"
)

const (
	keyLen  = 32
	saltLen = 16
)

// HashPassword hashes with argon2id using the configured parameters and
// returns a self-describing string (parameters embedded, so later config
// changes never invalidate existing hashes):
//
//	$argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
func HashPassword(password string, p config.Argon2) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := argon2.IDKey([]byte(password), salt,
		uint32(p.Time), uint32(p.Memory), uint8(p.Threads), keyLen) // #nosec G115 -- config bounds 1..4096 / 1KiB..1GiB / 1..64 (§ auth.argon2)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// VerifyPassword checks a password against a stored hash with constant-time
// comparison against the stored parameters. Malformed stored hashes verify
// false (never panic).
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var (
		memory, iterations uint32
		parallelism        uint8
	)
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	if len(want) != keyLen {
		return false // every hash we ever wrote is keyLen bytes (§9.1 fail closed)
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, keyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummySalt is fixed so DummyVerify costs the same on every unknown-email
// login attempt (timing equalisation, spec §9.1).
var dummySalt = []byte("gabkeep-dummy-salt")

// DummyVerify performs a real argon2 hash of a throwaway value so responses
// for unknown emails take as long as wrong-password responses.
func DummyVerify(p config.Argon2) {
	want := argon2.IDKey([]byte("gabkeep-timing-equaliser"), dummySalt,
		uint32(p.Time), uint32(p.Memory), uint8(p.Threads), keyLen) // #nosec G115 -- validated params
	got := argon2.IDKey([]byte("gabkeep-timing-equaliser-other"), dummySalt,
		uint32(p.Time), uint32(p.Memory), uint8(p.Threads), keyLen) // #nosec G115 -- validated params
	_ = subtle.ConstantTimeCompare(got, want)
}
