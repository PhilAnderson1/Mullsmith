// Package id generates ULIDs — 128-bit, time-ordered, Crockford-base32
// identifiers (spec §4). The time component makes ids sortable by creation;
// the 80-bit random component comes from crypto/rand.
package id

import (
	"crypto/rand"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a new ULID (26 characters).
func New() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil { // crypto/rand does not fail on linux
		panic("id: crypto/rand unavailable: " + err.Error())
	}

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
