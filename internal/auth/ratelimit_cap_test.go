package auth

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// --- audit2 finding 1: both auth maps must be hard-capped -------------------

// --- audit1 L-1: the fail-open windows must be visible in the log -------

func TestSaturationAlarms(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	warnLines := func(substr string) int {
		n := 0
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, substr) {
				n++
			}
		}
		return n
	}

	// The flood passes the cap repeatedly (eviction fires once per
	// evictFraction inserts) — the alarm must still be a single line:
	// the flood may not use the log as collateral damage.
	t.Run("limiter eviction warns once per minute", func(t *testing.T) {
		buf.Reset()
		l := NewLimiter(log)
		for i := 0; i < 2*maxLimiterEntries; i++ {
			l.Allow(fmt.Sprintf("login:ip-%d", i), 10, time.Hour)
		}
		if n := warnLines("rate-limit map hit its 16 384-bucket cap"); n != 1 {
			t.Errorf("cap evictions logged %d lines, want exactly 1 (cooldown)", n)
		}
	})

	t.Run("lock-map eviction warns", func(t *testing.T) {
		buf.Reset()
		l := NewLoginLocks(log)
		for i := 0; i < 2*maxLockEntries; i++ {
			l.Fail(fmt.Sprintf("acct:junk%d@x.io", i), 5, time.Minute) // max=5: nothing locks
		}
		if n := warnLines("login-lock map hit its 16 384-record cap"); n != 1 {
			t.Errorf("cap evictions logged %d lines, want exactly 1 (cooldown)", n)
		}
	})

	// The true fail-open: an all-locked map drops a newcomer's failure
	// outright — that, too, must surface.
	t.Run("all-locked drops warn", func(t *testing.T) {
		buf.Reset()
		l := NewLoginLocks(log)
		for i := 0; i < maxLockEntries; i++ {
			l.Fail(fmt.Sprintf("acct:locked%d@x.io", i), 1, time.Minute) // max=1: everything locks
		}
		// Now every slot is locked: each newcomer failure is dropped, but
		// the alarm stays at one line per minute.
		l.Fail("acct:newcomer1@x.io", 1, time.Minute)
		l.Fail("acct:newcomer2@x.io", 1, time.Minute)
		if n := warnLines("failed login went untracked"); n != 1 {
			t.Errorf("drops logged %d lines, want exactly 1 (cooldown)", n)
		}
	})

	// nil logger (tests, zero values): silent, no panics — the cap tests
	// above all run with nil and pass regardless of what they log.
}

func TestLoginLocksHugeKeyRoundtrip(t *testing.T) {
	l := NewLoginLocks(nil)
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
	l := NewLoginLocks(nil)
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
	l := NewLoginLocks(nil)
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
	l := NewLimiter(nil)
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
	l := NewLimiter(nil)
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
