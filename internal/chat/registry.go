package chat

import (
	"context"
	"errors"
	"sync"
	"time"

	"mullsmith/internal/id"
)

// ErrGenerating enforces "one in-flight Generation per conversation"
// (spec §6.2): the second POST returns 409.
var ErrGenerating = errors.New("Generation already running for this conversation")

// Capacity refusals from the atomic reservation in Start (§9.11).
var (
	ErrUserStreams   = errors.New("per-user concurrent Generation limit reached")
	ErrGlobalStreams = errors.New("server Generation capacity reached")
	ErrShuttingDown  = errors.New("server is shutting down")
)

type Generation struct {
	ID     string
	ConvID string
	UserID string
	cancel context.CancelFunc

	// approvals tracks outstanding awaiting_approval requests (§6.1 step 5):
	// approval id → decision channel the tool loop waits on.
	approvals map[string]chan ApprovalDecision
}

// ApprovalDecision is the user's answer to one tool approval; Remember
// lets "always allow in this conversation" persist for the rest of the
// conversation (§6.1 step 5, §9.9 layer 6 — code_run).
type ApprovalDecision struct {
	Approve  bool
	Remember bool
}

// AddApproval registers a pending approval for a live Generation.
func (r *Registry) AddApproval(genID string) (string, <-chan ApprovalDecision, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[genID]
	if !ok {
		return "", nil, false
	}
	if g.approvals == nil {
		g.approvals = map[string]chan ApprovalDecision{}
	}
	id := id.New()
	ch := make(chan ApprovalDecision, 1)
	g.approvals[id] = ch
	return id, ch, true
}

// ResolveApproval delivers a user decision; the caller must own the
// Generation. Unknown ids (already answered, timed out, gone) → false.
func (r *Registry) ResolveApproval(genID, approvalID, userID string, approve, remember bool) bool {
	r.mu.Lock()
	g, ok := r.byID[genID]
	if !ok || g.UserID != userID {
		r.mu.Unlock()
		return false
	}
	ch, open := g.approvals[approvalID]
	if open {
		delete(g.approvals, approvalID)
	}
	r.mu.Unlock()
	if !open {
		return false
	}
	ch <- ApprovalDecision{Approve: approve, Remember: remember} // buffered: never blocks
	return true
}

// RemoveApproval drops an unresolved approval when the loop gives up
// (timeout or abort) so late POSTs cannot fill a dead channel.
func (r *Registry) RemoveApproval(genID, approvalID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.byID[genID]; ok {
		delete(g.approvals, approvalID)
	}
}

// Registry tracks live generations. It doubles as the concurrency
// counter for app_limits.concurrent_streams_per_user.
type Registry struct {
	mu     sync.Mutex
	closed bool
	byConv map[string]*Generation
	byID   map[string]*Generation
}

func NewRegistry() *Registry {
	return &Registry{byConv: map[string]*Generation{}, byID: map[string]*Generation{}}
}

func (r *Registry) Start(convID, userID string, maxUser, maxGlobal int, cancel context.CancelFunc) (*Generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrShuttingDown
	}
	if _, busy := r.byConv[convID]; busy {
		return nil, ErrGenerating
	}
	// The per-user and global caps are evaluated and reserved in THIS lock
	// acquisition (audit finding 3): the previous check-then-Start split let
	// parallel requests for different conversations sail past the user cap.
	if maxGlobal > 0 && len(r.byID) >= maxGlobal {
		return nil, ErrGlobalStreams
	}
	if maxUser > 0 {
		running := 0
		for _, g := range r.byID {
			if g.UserID == userID {
				running++
			}
		}
		if running >= maxUser {
			return nil, ErrUserStreams
		}
	}
	g := &Generation{ID: id.New(), ConvID: convID, UserID: userID, cancel: cancel}
	r.byConv[convID] = g
	r.byID[g.ID] = g
	return g, nil
}

// Finish removes the Generation (idempotent, only if it is still the same
// one for the conversation).
func (r *Registry) Finish(g *Generation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.byConv[g.ConvID]; ok && cur == g {
		delete(r.byConv, g.ConvID)
	}
	delete(r.byID, g.ID)
}

// Abort cancels the Generation when id matches and the caller owns it.
func (r *Registry) Abort(genID, userID string) bool {
	r.mu.Lock()
	g, ok := r.byID[genID]
	r.mu.Unlock()
	if !ok || g.UserID != userID {
		return false
	}
	g.cancel()
	return true
}

// AbortConv cancels the live Generation of one conversation whose rows are
// being deleted (C6) — but ONLY for the conversation's owner (review3
// issue 1): the delete handler authorizes before it gets here, and the
// registry refuses to be a second way to touch another account's work.
// The cancel is issued with the registry lock released and nothing is
// awaited: the Generation notices at its next context check and its final
// writes then land as ErrRowGone/ErrConversationNotFound — which the
// orchestrator reads as cancellation, not storage failure.
func (r *Registry) AbortConv(convID, userID string) bool {
	r.mu.Lock()
	g, ok := r.byConv[convID]
	if ok && g.UserID != userID {
		r.mu.Unlock()
		return false // a foreign conversation: the registry stays untouched
	}
	r.mu.Unlock()
	if ok {
		g.cancel()
	}
	return ok
}

// AbortUser cancels every live Generation of one account — the
// deletion counterpart of AbortConv (C6): after the owner's rows are gone
// there is no work worth continuing, and no slot held while anyone waits.
func (r *Registry) AbortUser(userID string) {
	r.mu.Lock()
	var running []*Generation
	for _, g := range r.byID {
		if g.UserID == userID {
			running = append(running, g)
		}
	}
	r.mu.Unlock()
	for _, g := range running {
		g.cancel()
	}
}

func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// CountUser reports the running generations of one account (the account-
// delete sweep's idle check, C6).
func (r *Registry) CountUser(userID string) int {
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

// Shutdown atomically closes admission and cancels every reserved generation.
func (r *Registry) Shutdown() int {
	r.mu.Lock()
	r.closed = true
	gens := make([]*Generation, 0, len(r.byID))
	for _, g := range r.byID {
		gens = append(gens, g)
	}
	r.mu.Unlock()
	for _, g := range gens {
		g.cancel()
	}
	return len(gens)
}

// WaitIdle blocks until no generations are running or ctx expires — used at
// shutdown so streams can persist their final state before the DB closes.
func (r *Registry) WaitIdle(ctx context.Context) {
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
