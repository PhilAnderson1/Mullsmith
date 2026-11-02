package auth

import (
	"sync"
	"testing"
	"time"
)

func TestSignupBudgetReservationsAndRefill(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	b := NewSignupBudget(2)
	b.now, b.last = c.now, c.now()
	a, ok := b.Reserve()
	if !ok {
		t.Fatal("first reservation refused")
	}
	second, ok := b.Reserve()
	if !ok {
		t.Fatal("second reservation refused")
	}
	if _, ok := b.Reserve(); ok {
		t.Fatal("over-reserved capacity")
	}
	a.Release()
	a.Release() // Deferred cleanup must not restore capacity twice.
	replacement, ok := b.Reserve()
	if !ok {
		t.Fatal("failed signup did not release capacity")
	}
	replacement.Commit()
	replacement.Release()
	second.Commit()
	second.Commit()
	if _, ok := b.Reserve(); ok {
		t.Fatal("committed signups did not consume budget")
	}
	c.after(29 * time.Minute)
	if _, ok := b.Reserve(); ok {
		t.Fatal("budget refilled too early")
	}
	c.after(time.Minute)
	r, ok := b.Reserve()
	if !ok {
		t.Fatal("budget did not refill")
	}
	r.Commit()
	if _, ok := b.Reserve(); ok {
		t.Fatal("refill granted more than one token")
	}
}

func TestSignupBudgetPendingReservationsStayBounded(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	b := NewSignupBudget(1)
	b.now, b.last = c.now, c.now()
	r, _ := b.Reserve()
	c.after(2 * time.Hour)
	if _, ok := b.Reserve(); ok {
		t.Fatal("refill bypassed pending reservation")
	}
	r.Commit()
	if _, ok := b.Reserve(); ok {
		t.Fatal("time before commit replenished a newly charged token")
	}
	c.after(time.Hour)
	r, ok := b.Reserve()
	if !ok {
		t.Fatal("committed token did not refill")
	}
	r.Release()
}

func TestSignupBudgetConcurrentReservations(t *testing.T) {
	b := NewSignupBudget(2)
	var wg sync.WaitGroup
	admitted := make(chan *SignupReservation, 32)
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			if r, ok := b.Reserve(); ok {
				admitted <- r
			}
		})
	}
	wg.Wait()
	close(admitted)
	if len(admitted) != 2 {
		t.Fatalf("admitted %d concurrent signups, want 2", len(admitted))
	}
	for r := range admitted {
		r.Release()
	}
	r, ok := b.Reserve()
	if !ok {
		t.Fatal("released capacity was lost")
	}
	r.Release()
}

func TestSignupBudgetDisabled(t *testing.T) {
	b := NewSignupBudget(0)
	for i := 0; i < 10; i++ {
		r, ok := b.Reserve()
		if !ok || r != nil {
			t.Fatal("disabled budget refused or tracked a reservation")
		}
		r.Commit()
		r.Release()
	}
}
