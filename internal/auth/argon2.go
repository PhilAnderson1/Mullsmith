// Package auth holds credential hashing, session tokens, cookies, password
// policy and throttling primitives (spec §9.1, §9.5).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"

	"gabkeep/internal/config"
)

const (
	keyLen  = 32
	saltLen = 16
)

// hashWorkCount counts every Argon2 computation this process performs.
// Cheap (one atomic) and used for observability plus the audit finding 4
// regression tests: DummyVerify must cost exactly one hash, matching the
// wrong-password path (spec §9.1 timing equalisation).
var hashWorkCount atomic.Int64

// HashWorkCount reports the number of Argon2 computations since process start.
func HashWorkCount() int64 { return hashWorkCount.Load() }

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
	hashWorkCount.Add(1)
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
	hashWorkCount.Add(1)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummySalt is fixed so DummyVerify costs the same on every unknown-email
// login attempt (timing equalisation, spec §9.1).
var dummySalt = []byte("gabkeep-dummy-salt")

// DummyVerify performs one real Argon2 computation of a throwaway value so
// that unknown-email logins cost exactly what wrong-password logins cost:
// one IDKey with the configured parameters (spec §9.1). It used to compute
// two (a want/got pair compared against itself), which inverted the
// equalisation — unknown emails responded ~2x slower, an enumeration oracle
// — and doubled the attacker-induced hashing load (audit finding 4).
func DummyVerify(p config.Argon2) {
	_ = argon2.IDKey([]byte("gabkeep-timing-equaliser"), dummySalt,
		uint32(p.Time), uint32(p.Memory), uint8(p.Threads), keyLen) // #nosec G115 -- validated params
	hashWorkCount.Add(1)
}
