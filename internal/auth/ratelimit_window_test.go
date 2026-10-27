package auth

import (
	"testing"
	"time"
)

// Regression tests for the login-failure counting window: failures count
// inside a FIXED window that opens with the first failure; an expired
// window never contributes to a new lockout; a lockout lasts its full
// window from the threshold-crossing failure and is not extended by
// attempts while active. The clock is stepped explicitly — no sleeps.

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) after(d time.Duration) { c.t = c.t.Add(d) }

func newFakeLocks() (*LoginLocks, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	l := NewLoginLocks()
	l.now = c.now
	return l, c
}

const (
	winKey    = "acct:victim@x.io"
	winMax    = 3
	winWindow = 15 * time.Minute
)

// 1. Failures within the window reach the threshold and lock the account.
func TestLoginLocksWindowThresholdLocks(t *testing.T) {
	l, c := newFakeLocks()
	l.Fail(winKey, winMax, winWindow)
	l.Fail(winKey, winMax, winWindow)
	if l.Locked(winKey) {
		t.Fatal("locked at 2 of 3 failures")
	}
	c.after(14 * time.Minute) // still inside the window opened by failure #1
	l.Fail(winKey, winMax, winWindow)
	if !l.Locked(winKey) {
		t.Fatal("crossing the threshold inside the window did not lock")
	}
}

// 2. Failures from an expired window do not count toward a new lockout.
func TestLoginLocksExpiredWindowDoesNotCount(t *testing.T) {
	l, c := newFakeLocks()
	l.Fail(winKey, winMax, winWindow)
	l.Fail(winKey, winMax, winWindow)
	c.after(winWindow) // the window opened at failure #1; it is now over
	l.Fail(winKey, winMax, winWindow)
	if l.Locked(winKey) {
		t.Fatal("stale failures from an expired window armed a lockout")
	}
	// The fresh window locks only on its OWN third failure.
	l.Fail(winKey, winMax, winWindow)
	if l.Locked(winKey) {
		t.Fatal("locked at 2 of 3 inside the fresh window")
	}
	l.Fail(winKey, winMax, winWindow)
	if !l.Locked(winKey) {
		t.Fatal("fresh window failed to lock at its own threshold")
	}
}

// 3. A lockout triggered near the end of the counting window lasts its
// full duration — it must not die when the counting window expires, and
// must not lapse before its own deadline.
func TestLoginLocksNearWindowEndLastsFullDuration(t *testing.T) {
	l, c := newFakeLocks()
	const max, window = 2, winWindow
	l.Fail(winKey, max, window)              // opens the window
	c.after(14*time.Minute + 59*time.Second) // lock fires 1 s before it ends
	l.Fail(winKey, max, window)
	c.after(time.Minute) // t+15:59: counting window long gone
	if !l.Locked(winKey) {
		t.Fatal("lockout expired together with the counting window")
	}
	c.after(13*time.Minute + 58*time.Second) // t+29:57, two seconds left
	if !l.Locked(winKey) {
		t.Fatal("lockout lapsed before its full window")
	}
	c.after(3 * time.Second) // t+30:00: past deadline (locked at 14:59 + 15:00)
	if l.Locked(winKey) {
		t.Fatal("still locked after the lockout deadline")
	}
}

// 4. Attempts during an active lockout do not extend its expiry.
func TestLoginLocksDuringLockoutDoNotExtend(t *testing.T) {
	l, c := newFakeLocks()
	const max, window = 2, winWindow
	l.Fail(winKey, max, window)
	l.Fail(winKey, max, window) // locks from t0 for exactly one window
	for i := 0; i < 3; i++ {
		c.after(time.Minute)
		l.Fail(winKey, max, window) // absorbed by the active lockout
		if !l.Locked(winKey) {
			t.Fatalf("lockout vanished mid-flight at absorbed attempt %d", i+1)
		}
	}
	c.after(11*time.Minute + 59*time.Second) // t+14:59 (loop consumed 3:00), one second before the original deadline
	l.Fail(winKey, max, window)
	if !l.Locked(winKey) {
		t.Fatal("lockout ended early")
	}
	c.after(time.Second) // t+15:00: the ORIGINAL expiry, unmoved
	if l.Locked(winKey) {
		t.Fatal("attempts during the lockout extended its expiry")
	}
}

// 5. The first failure after lockout expiry starts a fresh count of 1.
func TestLoginLocksFirstFailureAfterLockoutIsFresh(t *testing.T) {
	l, c := newFakeLocks()
	l.Fail(winKey, winMax, winWindow)
	l.Fail(winKey, winMax, winWindow)
	l.Fail(winKey, winMax, winWindow) // locks at count 3
	c.after(winWindow + time.Second)  // lockout lapses
	l.Fail(winKey, winMax, winWindow)
	if l.Locked(winKey) {
		t.Fatal("a single post-lockout failure re-locked the account (stale count)")
	}
	l.Fail(winKey, winMax, winWindow)
	l.Fail(winKey, winMax, winWindow) // count 3 of the NEW window
	if !l.Locked(winKey) {
		t.Fatal("post-lockout window failed to lock at its own threshold")
	}
}

// 6. A successful login (Reset) clears the counter and any lockout.
func TestLoginLocksResetClearsCounter(t *testing.T) {
	l, _ := newFakeLocks()
	l.Fail(winKey, winMax, winWindow)
	l.Fail(winKey, winMax, winWindow)
	l.Reset(winKey) // successful login
	l.Fail(winKey, winMax, winWindow)
	l.Fail(winKey, winMax, winWindow)
	if l.Locked(winKey) {
		t.Fatal("pre-reset failures still counted after a successful login")
	}
	l.Fail(winKey, winMax, winWindow)
	if !l.Locked(winKey) {
		t.Fatal("post-reset window failed to lock at its own threshold")
	}
	// Reset also lifts an active lockout.
	l.Reset(winKey)
	if l.Locked(winKey) {
		t.Fatal("Reset did not clear an active lockout")
	}
}
