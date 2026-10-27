package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// maxLimiterEntries / maxLockEntries hard-cap the two auth maps (audit2
// finding 1): before the caps, distinct unauthenticated keys (fresh IPs, or
// attacker-sized login emails retained by failed attempts) grew the maps
// without bound and turned every admitted request into an O(n) cleanup
// scan. Buckets and hashed lock records are small and uniform, so the caps
// bound retained bytes to a few MB.
const (
	maxLimiterEntries = 16384
	maxLockEntries    = 16384

	// gcThreshold: above this many buckets, sweep opportunistically
	// (rate-limited to ~1/s — the old code scanned on EVERY admitted
	// request, audit2 finding 1).
	gcThreshold = 4096

	// evictFraction is the batch size cleared at saturation, so the O(n)
	// selection runs once per evictionFraction inserts, not per insert.
	evictFraction = maxLimiterEntries / 4
)

// Limiter is a small in-memory token bucket per key (spec §9.11). Lazy
// eviction on access keeps background goroutines unnecessary for M1.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	gcAt    time.Time
}

type bucket struct {
	tokens float64
	cap    float64
	refill float64 // tokens per second
	last   time.Time
}

func NewLimiter() *Limiter {
	return &Limiter{buckets: map[string]*bucket{}}
}

// Len reports retained buckets (observability and cap regression tests).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Allow consumes one token from key's bucket, configured as "rate events per
// window" (burst capacity = rate).
func (l *Limiter) Allow(key string, rate int, window time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxLimiterEntries && !l.makeRoomLocked(now) {
			// A flood of fresh keys must not cost unbounded memory, and a
			// brand-new IP must not be punished for it either: admit
			// WITHOUT retaining a bucket. Existing buckets keep throttling
			// their own IPs; the hash gate is the real backstop (spec §9.1).
			return true
		}
		b = &bucket{tokens: float64(rate), cap: float64(rate), refill: float64(rate) / window.Seconds(), last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * b.refill
	if b.tokens > b.cap {
		b.tokens = b.cap
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	// Opportunistic cleanup, rate-limited to ~1/s: above gcThreshold entries
	// the old code scanned the whole map on EVERY admitted request (audit2).
	if len(l.buckets) > gcThreshold && now.Sub(l.gcAt) > time.Second {
		l.gcAt = now
		l.gcLocked(now)
	}
	return true
}

// makeRoomLocked frees space for one new key: idle buckets first, then the
// least-recently-used quarter in a single batch (amortising the selection
// over evictFraction later inserts). Idle buckets were within a refill of
// expiry anyway; evicting one costs that IP a stale counter, never the
// server's memory.
func (l *Limiter) makeRoomLocked(now time.Time) bool {
	l.gcLocked(now)
	if len(l.buckets) < maxLimiterEntries {
		return true
	}
	byLast := make([]time.Time, 0, len(l.buckets))
	for _, b := range l.buckets {
		byLast = append(byLast, b.last)
	}
	sort.Slice(byLast, func(i, j int) bool { return byLast[i].Before(byLast[j]) })
	cutoff := byLast[evictFraction-1]
	for k, b := range l.buckets {
		if !b.last.After(cutoff) {
			delete(l.buckets, k)
		}
	}
	return len(l.buckets) < maxLimiterEntries
}

// gcLocked drops buckets idle for more than 10 minutes; called opportunistically.
func (l *Limiter) gcLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, k)
		}
	}
}

// LoginLocks tracks failed logins per account and locks after a threshold
// (spec §9.1: max_failed_logins per 15 minutes → 15-minute lockout).
//
// Counting uses a FIXED window that opens with the first failure and ends
// at firstAt+window: a failure past the boundary opens a fresh window at
// count 1, so stale failures can neither arm a lockout nor survive one.
// The window is evaluated inside Fail on every attempt — correctness never
// waits for the garbage sweep to have run.
//
// Keys are stored as SHA-256 digests: the raw input may be an
// attacker-sized string (login never echoed a length check before
// audit2), and a digest pins every record to a fixed ~64-byte key. The map
// is additionally hard-capped — digests alone bound the entry SIZE, not
// the entry COUNT.
type LoginLocks struct {
	mu          sync.Mutex
	m           map[string]*failRecord
	gcAt        time.Time
	noRoomUntil time.Time // negative cache: nothing evictable until then
	// now is a clock seam (nil = time.Now) so window/lockout tests step
	// time explicitly instead of sleeping.
	now func() time.Time
}

type failRecord struct {
	count       int
	firstAt     time.Time // first failure of the current counting window
	lockedUntil time.Time // zero until a threshold crossing arms a lockout
}

func NewLoginLocks() *LoginLocks {
	return &LoginLocks{m: map[string]*failRecord{}, now: time.Now}
}

// clockNow keeps Fail and Locked on one notion of "now"; a zero-value
// LoginLocks keeps working even without the seam installed.
func (l *LoginLocks) clockNow() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

func hashLockKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Len reports retained records (observability and cap regression tests).
func (l *LoginLocks) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// Locked reports whether the account is currently locked. The lockout is an
// absolute deadline: it stays effective regardless of what the counting
// window or the garbage sweep has done.
func (l *LoginLocks) Locked(key string) bool {
	now := l.clockNow()
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.m[hashLockKey(key)]
	if !ok {
		return false
	}
	return now.Before(r.lockedUntil)
}

// Fail records one failed attempt against a fixed counting window that
// opened with the account's first failure (firstAt … firstAt+window). A
// failure at or past the window's end opens a fresh one at count 1 instead
// of inheriting stale counts. Crossing the threshold locks the account for
// a full window measured from the locking failure. An active lockout
// absorbs attempts: they are neither counted nor do they extend its
// deadline; once it lapses, the next failure starts a fresh count of 1
// (a lockout always ends at or after firstAt+window, so its expiry implies
// the counting window expired too). Saturation policy
// (audit2): existing records always update, and ACTIVE LOCKOUTS ARE NEVER
// EVICTED — when the map holds nothing but locked records, a brand-new
// key's failure is dropped instead of evicting someone's lockout. The
// leftover exposure (a newcomer momentarily untracked) stays bounded by
// the per-IP login bucket and the hash gate.
func (l *LoginLocks) Fail(key string, max int, window time.Duration) {
	now := l.clockNow()
	hk := hashLockKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.gcAt) > time.Minute {
		l.gcAt = now
		for k, r := range l.m {
			if now.Sub(r.firstAt) > 2*window && now.After(r.lockedUntil) {
				delete(l.m, k)
			}
		}
	}

	r, ok := l.m[hk]
	if !ok {
		if len(l.m) >= maxLockEntries {
			// noRoomUntil short-circuits the scan while the map is known
			// immovable, so refusals stay O(1) instead of O(map) each.
			if now.Before(l.noRoomUntil) || !l.makeRoomLocked(now) {
				return
			}
		}
		r = &failRecord{count: 0, firstAt: now}
		l.m[hk] = r
	}

	// Active lockout: this attempt counts for nothing and moves nothing —
	// the deadline set when the threshold was crossed is the whole story.
	if now.Before(r.lockedUntil) {
		return
	}
	// Fixed window expired: open a fresh one at count 1. This is evaluated
	// here rather than relying on deletion, so correctness holds even when
	// the sweep has not run.
	if !now.Before(r.firstAt.Add(window)) {
		r.count = 0
		r.firstAt = now
	}
	r.count++
	if r.count >= max {
		r.lockedUntil = now.Add(window)
	}
}

// makeRoomLocked evicts the oldest NON-locked records in a batch (unlocked
// first — a live lockout is untouchable). Returns false only if every
// retained record is locked.
func (l *LoginLocks) makeRoomLocked(now time.Time) bool {
	var unlocked []time.Time
	for _, r := range l.m {
		if !now.Before(r.lockedUntil) {
			unlocked = append(unlocked, r.firstAt)
		}
	}
	if len(unlocked) == 0 {
		l.noRoomUntil = now.Add(time.Second)
		return false
	}
	n := len(unlocked) / 4
	if n < 1 {
		n = 1
	}
	sort.Slice(unlocked, func(i, j int) bool { return unlocked[i].Before(unlocked[j]) })
	cutoff := unlocked[n-1]
	freed := 0
	for k, r := range l.m {
		if freed >= evictFraction {
			break
		}
		if !now.Before(r.lockedUntil) && !r.firstAt.After(cutoff) {
			delete(l.m, k)
			freed++
		}
	}
	return len(l.m) < maxLockEntries
}

// Reset clears failures after a successful login.
func (l *LoginLocks) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, hashLockKey(key))
}
