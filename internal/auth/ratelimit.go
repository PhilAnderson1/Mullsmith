package auth

import (
	"sync"
	"time"
)

// Limiter is a small in-memory token bucket per key (spec §9.11). Lazy
// eviction on access keeps background goroutines unnecessary for M1.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
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

// Allow consumes one token from key's bucket, configured as "rate events per
// window" (burst capacity = rate).
func (l *Limiter) Allow(key string, rate int, window time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
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
	if len(l.buckets) > 4096 {
		l.gcLocked(now)
	}
	return true
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
type LoginLocks struct {
	mu   sync.Mutex
	m    map[string]*failRecord
	gcAt time.Time
}

type failRecord struct {
	count       int
	firstAt     time.Time
	lockedUntil time.Time
}

func NewLoginLocks() *LoginLocks {
	return &LoginLocks{m: map[string]*failRecord{}}
}

// Locked reports whether the account is currently locked.
func (l *LoginLocks) Locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.m[key]
	if !ok {
		return false
	}
	return time.Now().Before(r.lockedUntil)
}

// Fail records one failed attempt; when count reaches max within the window
// the account is locked for the same window length.
func (l *LoginLocks) Fail(key string, max int, window time.Duration) {
	now := time.Now()
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

	r, ok := l.m[key]
	if !ok || now.Sub(r.firstAt) > window {
		r = &failRecord{count: 0, firstAt: now}
		l.m[key] = r
	}
	r.count++
	if r.count >= max {
		r.lockedUntil = now.Add(window)
	}
}

// Reset clears failures after a successful login.
func (l *LoginLocks) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}
