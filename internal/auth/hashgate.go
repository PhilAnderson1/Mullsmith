package auth

import (
	"context"
	"errors"
)

// ErrHashBusy means the gate's admission quota (running + waiting) is full.
// Callers must answer 429 rather than queue unboundedly (audit finding 4).
var ErrHashBusy = errors.New("password hashing capacity reached")

// HashGate bounds concurrent Argon2 work globally (spec §9.11, audit
// finding 4). The hash parameters are deliberately expensive (64 MiB,
// 4 threads by default), so per-IP buckets — which bound each requester —
// never bound the total: a distributed flood of login attempts would start
// one memory-hungry hash per request. The gate admits at most concurrency
// hashes at once, parks at most queue more waiters, and answers ErrHashBusy
// immediately beyond that. A nil gate runs work ungated (app_limits.
// hash_concurrency: -1).
type HashGate struct {
	sem   chan struct{}
	quota chan struct{} // running + waiting tickets
}

// NewHashGate builds a gate. concurrency < 1 clamps to 1 — "no hashing" is
// not expressible here, only "disabled" via a nil gate.
func NewHashGate(concurrency, queue int) *HashGate {
	if concurrency < 1 {
		concurrency = 1
	}
	if queue < 0 {
		queue = 0
	}
	return &HashGate{
		sem:   make(chan struct{}, concurrency),
		quota: make(chan struct{}, concurrency+queue),
	}
}

// Run executes f while holding one gate slot. It fails fast with
// ErrHashBusy when the queue is full, returns ctx.Err() when the caller
// loses patience (browser tab closed ⇒ queue spot released), and otherwise
// returns whatever f returns.
func (g *HashGate) Run(ctx context.Context, f func() error) error {
	if g == nil {
		return f()
	}
	select {
	case g.quota <- struct{}{}:
	default:
		return ErrHashBusy
	}
	defer func() { <-g.quota }()

	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.sem }()
	return f()
}
