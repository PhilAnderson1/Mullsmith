package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mullsmith/internal/config"
)

func TestHashGateSerializesAndRefuses(t *testing.T) {
	g := NewHashGate(1, 1) // one slot, one waiting spot

	var running, peak atomic.Int32
	started := make(chan struct{})
	unhold := make(chan struct{})
	var wg sync.WaitGroup

	// Occupant: holds the single slot until told.
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := g.Run(context.Background(), func() error {
			if c := running.Add(1); c > peak.Load() {
				peak.Store(c)
			}
			close(started)
			<-unhold
			running.Add(-1)
			return nil
		})
		if err != nil {
			t.Errorf("occupant: %v", err)
		}
	}()
	<-started

	// Waiter: takes the queue spot and parks on the slot.
	waiterErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		waiterErr <- g.Run(context.Background(), func() error {
			if c := running.Add(1); c > peak.Load() {
				peak.Store(c)
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			return nil
		})
	}()

	// Give the waiter a moment to park, then arrive over capacity:
	// 1 running + 1 waiting = quota full ⇒ immediate ErrHashBusy.
	time.Sleep(30 * time.Millisecond)
	if err := g.Run(context.Background(), func() error { return nil }); !errors.Is(err, ErrHashBusy) {
		t.Errorf("over-capacity Run = %v, want ErrHashBusy", err)
	}

	close(unhold)
	wg.Wait()
	if err := <-waiterErr; err != nil {
		t.Errorf("waiter: %v", err)
	}
	if p := peak.Load(); p > 1 {
		t.Errorf("peak concurrency %d exceeded gate size 1", p)
	}

	// Gate fully drained: work flows again.
	if err := g.Run(context.Background(), func() error { return nil }); err != nil {
		t.Errorf("after drain: %v", err)
	}
}

func TestHashGateCancelReleasesSpot(t *testing.T) {
	g := NewHashGate(1, 1)

	started := make(chan struct{})
	unhold := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = g.Run(context.Background(), func() error { close(started); <-unhold; return nil })
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		canceled <- g.Run(ctx, func() error { return nil })
	}()
	time.Sleep(30 * time.Millisecond) // let the waiter park on the slot

	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Errorf("canceled waiter = %v, want context.Canceled", err)
	}

	// The canceled spot came back: a fresh arrival now PARKS (gets a quota
	// ticket) instead of being refused. Had the ticket leaked, Run would
	// answer ErrHashBusy immediately.
	second := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		second <- g.Run(context.Background(), func() error { return nil })
	}()
	select {
	case err := <-second:
		t.Errorf("post-cancel arrival returned %v while the slot is still held; want it parked", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(unhold)
	wg.Wait()
	if err := <-second; err != nil {
		t.Errorf("post-cancel waiter: %v", err)
	}
}

func TestHashGateNilRunsUngated(t *testing.T) {
	var g *HashGate // app_limits.hash_concurrency: -1
	ran := false
	if err := g.Run(context.Background(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("nil gate: err=%v ran=%v, want nil/true", err, ran)
	}
}

// Finding 4 regression: DummyVerify must cost exactly ONE argon2 op — the
// same as one VerifyPassword — so unknown-email logins neither leak by
// timing nor double the hashing load.
func TestDummyVerifyCostsOneHash(t *testing.T) {
	p := config.Argon2{Time: 1, Memory: 1024, Threads: 1}

	before := HashWorkCount()
	DummyVerify(p)
	if got := HashWorkCount() - before; got != 1 {
		t.Errorf("DummyVerify = %d argon2 ops, want 1", got)
	}

	h, err := HashPassword("s3cret-passphrase", p)
	if err != nil {
		t.Fatal(err)
	}
	before = HashWorkCount()
	if !VerifyPassword("s3cret-passphrase", h) {
		t.Fatal("valid password failed verification")
	}
	if got := HashWorkCount() - before; got != 1 {
		t.Errorf("VerifyPassword = %d argon2 ops, want 1", got)
	}
}
