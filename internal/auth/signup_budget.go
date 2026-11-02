package auth

import (
	"sync"
	"time"
)

// SignupBudget reserves capacity for in-flight signups and charges only created
// accounts. Its single counter is independent of evictable per-client limits.
type SignupBudget struct {
	mu      sync.Mutex
	limit   float64
	tokens  float64
	pending int
	last    time.Time
	now     func() time.Time
}

func NewSignupBudget(perHour int) *SignupBudget {
	if perHour <= 0 {
		return nil
	}
	return &SignupBudget{limit: float64(perHour), tokens: float64(perHour), last: time.Now(), now: time.Now}
}

// SignupReservation must be committed after account creation or released on
// failure. Both operations are idempotent; a nil reservation disables the cap.
type SignupReservation struct {
	budget *SignupBudget
	done   bool // guarded by budget.mu
}

func (b *SignupBudget) refill() {
	now := b.now()
	b.tokens = min(b.limit, b.tokens+now.Sub(b.last).Seconds()*b.limit/time.Hour.Seconds())
	b.last = now
}

func (b *SignupBudget) Reserve() (*SignupReservation, bool) {
	if b == nil {
		return nil, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.tokens-float64(b.pending) < 1 {
		return nil, false
	}
	b.pending++
	return &SignupReservation{budget: b}, true
}

func (r *SignupReservation) Commit()  { r.finish(true) }
func (r *SignupReservation) Release() { r.finish(false) }

func (r *SignupReservation) finish(commit bool) {
	if r == nil {
		return
	}
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.done {
		return
	}
	b.refill()
	b.pending--
	if commit {
		b.tokens--
	}
	r.done = true
}
