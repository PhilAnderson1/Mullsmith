package server

import "sync"

// userTransferGate retains only accounts with active requests. Admission never
// queues, and upload/download handlers defer release before filesystem work.
// A nil gate disables this control, matching the optional-cap convention.
type userTransferGate struct {
	mu     sync.Mutex
	active map[string]int
	limit  int
}

func newUserTransferGate(limit int) *userTransferGate {
	if limit < 0 {
		return nil
	}
	return &userTransferGate{limit: limit, active: make(map[string]int)}
}

func (g *userTransferGate) tryAcquire(userID string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[userID] >= g.limit {
		return false
	}
	g.active[userID]++
	return true
}

func (g *userTransferGate) release(userID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[userID] <= 1 {
		delete(g.active, userID)
	} else {
		g.active[userID]--
	}
}
