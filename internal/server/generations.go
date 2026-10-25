package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"gabkeep/internal/id"
)

// errGenerating enforces "one in-flight generation per conversation"
// (spec §6.2): the second POST returns 409.
var errGenerating = errors.New("generation already running for this conversation")

type generation struct {
	ID     string
	ConvID string
	UserID string
	cancel context.CancelFunc
}

// genRegistry tracks live generations. It doubles as the concurrency
// counter for limits.concurrent_streams_per_user.
type genRegistry struct {
	mu     sync.Mutex
	byConv map[string]*generation
	byID   map[string]*generation
}

func newGenRegistry() *genRegistry {
	return &genRegistry{byConv: map[string]*generation{}, byID: map[string]*generation{}}
}

func (r *genRegistry) Start(convID, userID string, cancel context.CancelFunc) (*generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.byConv[convID]; busy {
		return nil, errGenerating
	}
	g := &generation{ID: id.New(), ConvID: convID, UserID: userID, cancel: cancel}
	r.byConv[convID] = g
	r.byID[g.ID] = g
	return g, nil
}

// Finish removes the generation (idempotent, only if it is still the same
// one for the conversation).
func (r *genRegistry) Finish(g *generation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.byConv[g.ConvID]; ok && cur == g {
		delete(r.byConv, g.ConvID)
	}
	delete(r.byID, g.ID)
}

// Abort cancels the generation when id matches and the caller owns it.
func (r *genRegistry) Abort(genID, userID string) bool {
	r.mu.Lock()
	g, ok := r.byID[genID]
	r.mu.Unlock()
	if !ok || g.UserID != userID {
		return false
	}
	g.cancel()
	return true
}

func (r *genRegistry) CountForUser(userID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, g := range r.byID {
		if g.UserID == userID {
			n++
		}
	}
	return n
}

func (r *genRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// waitIdle blocks until no generations are running or ctx expires — used at
// shutdown so streams can persist their final state before the DB closes.
func (r *genRegistry) waitIdle(ctx context.Context) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for r.Count() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
