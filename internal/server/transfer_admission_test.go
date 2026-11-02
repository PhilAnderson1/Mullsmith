package server

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestUserTransferAdmissionAtomicAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit, want int
	}{{"bounded", 2, 2}, {"disabled", -1, 64}} {
		t.Run(tc.name, func(t *testing.T) {
			g := newUserTransferGate(tc.limit)
			var attempts, finished sync.WaitGroup
			var accepted atomic.Int32
			release := make(chan struct{})
			attempts.Add(64)
			finished.Add(64)
			for i := 0; i < 64; i++ {
				go func() {
					defer finished.Done()
					ok := g.tryAcquire("owner")
					if ok {
						accepted.Add(1)
					}
					attempts.Done()
					if ok {
						<-release
						g.release("owner")
					}
				}()
			}
			attempts.Wait()
			got := accepted.Load()
			close(release)
			finished.Wait()
			if int(got) != tc.want {
				t.Fatalf("admitted %d, want %d", got, tc.want)
			}
			if g != nil && len(g.active) != 0 {
				t.Fatalf("retained idle users: %v", g.active)
			}
		})
	}
}

func assertNoTransferSlots(t *testing.T, g *userTransferGate) {
	t.Helper()
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.active) != 0 {
		t.Fatalf("idle accounts retained: %v", g.active)
	}
}
