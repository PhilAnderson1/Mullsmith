package id

import (
	"strings"
	"testing"
)

func TestNewIsStrictlyMonotonic(t *testing.T) {
	// Thousands of ids span only a few milliseconds: the entropy increment
	// must keep every id greater than the previous one.
	const n = 5000
	prev := New()
	for i := 0; i < n; i++ {
		cur := New()
		if cur <= prev {
			t.Fatalf("id %d not monotonic: %q <= %q", i, cur, prev)
		}
		prev = cur
	}
}

func TestNewShape(t *testing.T) {
	for i := 0; i < 20; i++ {
		s := New()
		if len(s) != 26 || !Valid(s) {
			t.Fatalf("New() = %q, want 26 valid Crockford characters", s)
		}
		// I, L, O, U are excluded from Crockford's base32.
		if strings.ContainsAny(s, "ILOU") {
			t.Fatalf("New() = %q contains excluded letters", s)
		}
	}
}

func TestValid(t *testing.T) {
	ok := []string{New(), strings.ToLower(New())}
	bad := []string{"", "short", New() + "X", "0000000000000000000000000O"} // O invalid
	for _, s := range ok {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false", s)
		}
	}
	for _, s := range bad {
		if Valid(s) {
			t.Errorf("Valid(%q) = true", s)
		}
	}
}
