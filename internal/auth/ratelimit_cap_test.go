package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- audit2 finding 1: both auth maps must be hard-capped -------------------

func TestLoginLocksHugeKeyRoundtrip(t *testing.T) {
	l := NewLoginLocks()
	huge := "acct:" + strings.Repeat("x", 1<<20) + "@x.io"
	if l.Locked(huge) {
		t.Fatal("unknown key reports locked")
	}
	l.Fail(huge, 2, time.Minute)
	if l.Locked(huge) {
		t.Fatal("locked after one of two failures")
	}
	l.Fail(huge, 2, time.Minute)
	if !l.Locked(huge) {
		t.Fatal("not locked after reaching the threshold")
	}
	l.Reset(huge)
	if l.Locked(huge) {
		t.Fatal("still locked after Reset")
	}
}

// Scenario A: a flood of distinct failures fills the map to the cap and
// stops there; a locked victim record outlives every eviction.
func TestLoginLocksFloodStaysCappedAndSparesLockouts(t *testing.T) {
	l := NewLoginLocks()
	const window = time.Minute // gc needs 2×window idleness — won't fire mid-test

	l.Fail("acct:victim@x.io", 2, window)
	l.Fail("acct:victim@x.io", 2, window)
	if !l.Locked("acct:victim@x.io") {
		t.Fatal("victim never locked")
	}

	blob := strings.Repeat("j", 4096)
	for i := 0; i < 2*maxLockEntries; i++ {
		l.Fail(fmt.Sprintf("acct:junk%d%s@x.io", i, blob), 5, window)
		if n := l.Len(); n > maxLockEntries {
			t.Fatalf("map grew past the cap: %d > %d at insert %d", n, maxLockEntries, i)
		}
	}
	if !l.Locked("acct:victim@x.io") {
		t.Fatal("eviction destroyed a live lockout (saturation policy violated)")
	}
}

// Scenario B — the pathological worst case: every retained record is
// locked, so nothing may be evicted and NEW failures are dropped instead.
func TestLoginLocksAllSaturatedDropsNewFailures(t *testing.T) {
	l := NewLoginLocks()
	const window = time.Minute

	l.Fail("acct:victim@x.io", 1, window) // max=1: every record locks instantly
	for i := 0; i < 2*maxLockEntries; i++ {
		l.Fail(fmt.Sprintf("acct:junk%d@x.io", i), 1, window)
	}
	if n := l.Len(); n > maxLockEntries {
		t.Fatalf("map size %d exceeds cap %d", n, maxLockEntries)
	}
	if !l.Locked("acct:victim@x.io") {
		t.Fatal("victim lockout evicted from an all-locked map")
	}
	// A newcomer cannot be tracked while every slot is locked — the
	// failure is dropped rather than evicting someone's lockout.
	l.Fail("acct:newcomer@x.io", 1, window)
	if l.Locked("acct:newcomer@x.io") {
		t.Fatal("newcomer inserted past an immovable all-locked map")
	}
}

func TestLimiterStaysCappedUnderFreshKeyFlood(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 2*maxLimiterEntries; i++ {
		if !l.Allow(fmt.Sprintf("login:ip-%d", i), 10, time.Hour) {
			t.Fatalf("fresh key %d denied — new IPs must not be punished", i)
		}
		if n := l.Len(); n > maxLimiterEntries {
			t.Fatalf("bucket map grew past the cap: %d > %d at key %d", n, maxLimiterEntries, i)
		}
	}
}

func TestLimiterStillThrottlesItsOwnKeysAtCap(t *testing.T) {
	l := NewLimiter()
	// Saturate, then verify an existing bucket keeps counting down:
	// eviction must not hand a brute-forcing IP fresh tokens.
	for i := 0; i < 2*maxLimiterEntries; i++ {
		l.Allow(fmt.Sprintf("login:ip-%d", i), 10, time.Hour)
	}
	for i := 0; i < 10; i++ {
		if !l.Allow("login:victim-ip", 10, time.Hour) {
			t.Fatalf("victim-ip denied at attempt %d before exhausting its own rate", i+1)
		}
	}
	if l.Allow("login:victim-ip", 10, time.Hour) {
		t.Fatal("victim-ip got an 11th allowance — its bucket was evicted mid-throttle")
	}
}
