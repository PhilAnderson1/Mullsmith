// Package id generates ULIDs — 128-bit, time-ordered, Crockford-base32
// identifiers (spec §4). The time component makes ids sortable by creation;
// the 80-bit random component comes from crypto/rand.
//
// Within a process the ids are strictly monotonic: ids minted in the same
// millisecond (or during a clock rewind) increment the entropy instead of
// re-rolling it, so "newer row has the larger id" always holds. Message
// ordering (created_at DESC, id DESC) relies on that when two rows land in
// the same unix second.
package id

import (
	"crypto/rand"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu      sync.Mutex
	lastMS  int64
	entropy [10]byte
)

// New returns a new ULID (26 characters), strictly greater than any id this
// process returned before.
func New() string {
	mu.Lock()
	defer mu.Unlock()

	ms := time.Now().UnixMilli()
	if ms <= lastMS {
		// Same millisecond (or clock skew): bump the big-endian entropy.
		ms = lastMS
		for i := len(entropy) - 1; i >= 0; i-- {
			entropy[i]++
			if entropy[i] != 0 {
				break
			}
		}
	} else {
		if _, err := rand.Read(entropy[:]); err != nil { // crypto/rand does not fail on linux
			panic("id: crypto/rand unavailable: " + err.Error())
		}
		lastMS = ms
	}

	var b [16]byte
	// Timestamp is the big-endian 48 high bits of ms: byte() is the intended
	// per-byte mask, truncation here is the ULID layout, not an overflow.
	b[0] = byte(ms >> 40) // #nosec G115
	b[1] = byte(ms >> 32) // #nosec G115
	b[2] = byte(ms >> 24) // #nosec G115
	b[3] = byte(ms >> 16) // #nosec G115
	b[4] = byte(ms >> 8)  // #nosec G115
	b[5] = byte(ms)       // #nosec G115
	copy(b[6:], entropy[:])

	var out [26]byte
	// Encode 128 bits big-endian, 5 bits per character (125 used, 3 zero-pad).
	acc, bits, di := 0, 0, 0
	for _, v := range b {
		acc = acc<<8 | int(v)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[di] = crockford[(acc>>bits)&31]
			di++
		}
	}
	if bits > 0 {
		out[di] = crockford[(acc<<(5-bits))&31]
	}
	return string(out[:])
}

// Valid reports whether s looks like a ULID (26 Crockford chars). Used to
// guard path components (spec §9.7).
func Valid(s string) bool {
	if len(s) != 26 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if !isCrockford(c) {
			return false
		}
	}
	return true
}

func isCrockford(c byte) bool {
	for i := 0; i < len(crockford); i++ {
		if crockford[i] == c {
			return true
		}
	}
	return false
}
