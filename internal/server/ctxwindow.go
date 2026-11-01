// Context-window resolution (spec §2.2 llm.models[].context_size, §6.1).
// The window is a property of the MODEL: an entry pinning a positive
// number states it outright; an entry omitting it (or -1) takes it from
// the endpoint's GET /models listing and falls back to unlimited — the
// meter shows a plain count, the send gate stays open — when the endpoint
// says nothing (agreed doctrine: never guess a number).
//
// A window answers two questions per conversation: what denominator the
// SPA's meter divides by (carried on the conversation payload, not meta),
// and whether the next send would overflow before we pay for it.
//
// Discovery probes once and caches per model id (lower-cased, the way
// endpoints list them). Pinned entries never probe; the refresh runs at
// boot (if any entry is auto), when a conversation is opened, when its
// model changes, and at the start of each new conversation.
package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"mullsmith/internal/llm"
)

// windowGrace bounds how long a caller may wait for a first-ever probe of
// a model. Once a value (or a failure verdict) exists, refreshes are
// always async: no request path ever blocks behind a slow endpoint.
const windowGrace = 1500 * time.Millisecond

// StartContextWindowProbe warms the auto-detected windows once in the
// background at boot; main calls it. An all-pinned config ignores it.
func (s *Server) StartContextWindowProbe() {
	if !s.cfg.LLM.AnyContextAuto() {
		return
	}
	go s.refreshContextWindows()
}

// windowProbeTimeout bounds one GET /models. Kept well under typical send
// patience so even the grace-waited first probe stays a footnote.
const windowProbeTimeout = 3 * time.Second

// probeEndpointWindows is the default Server.windowProbe: every window
// the /models listing reports, keyed by lower-cased model id. Absence is
// normal — context_length is not part of the OpenAI spec.
func (s *Server) probeEndpointWindows(ctx context.Context) (map[string]int, bool) {
	ctx, cancel := context.WithTimeout(ctx, windowProbeTimeout)
	defer cancel()
	models, err := llm.ProbeModels(ctx, s.cfg)
	if err != nil {
		s.logger.Info("context-window probe failed", "err", err)
		return nil, false
	}
	out := map[string]int{}
	for _, m := range models {
		if m.ContextLength > 0 {
			out[strings.ToLower(m.ID)] = m.ContextLength
		}
	}
	if len(out) == 0 {
		s.logger.Info("endpoint reports no context windows", "models", len(models))
	}
	return out, len(out) > 0
}

// contextWindow is the window in force for a model: its entry's pinned
// number, else the discovered one; 0 means unknown (unlimited).
func (s *Server) contextWindow(model string) int64 {
	if w := s.cfg.LLM.ContextSizeFor(model); w > 0 {
		return int64(w)
	}
	if w, ok := s.discoveredWindows.Load(strings.ToLower(model)); ok {
		return int64(w)
	}
	return 0
}

// refreshContextWindows refreshes every auto model's cached window: in
// the background when all are known, synchronously — grace-capped — for
// any that is not. Pinned models are never probed.
func (s *Server) refreshContextWindows() {
	if s.pinnedOnly() {
		return
	}
	if s.autoWindowsKnown() {
		s.ensureProbe()
		return
	}
	s.awaitProbe()
}

// ensureProbe starts a discovery probe UNLESS one is already running (R5):
// a dozen concurrent opens used to mean a dozen GET /models against the
// same endpoint. The returned channel closes when the probe in flight —
// this caller's or the one it joined — finishes.
func (s *Server) ensureProbe() <-chan struct{} {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probeDone != nil {
		return s.probeDone
	}
	done := make(chan struct{})
	s.probeDone = done
	go func() {
		s.doProbe()
		s.probeMu.Lock()
		s.probeDone = nil
		s.probeMu.Unlock()
		close(done)
	}()
	return done
}

// awaitProbe joins the running probe, but never longer than the grace.
// Overlapping waiters share ONE probe; the last write wins with the same
// number.
func (s *Server) awaitProbe() {
	done := s.ensureProbe()
	select {
	case <-done:
	case <-time.After(windowGrace):
	}
}

// refreshContextWindowFor refreshes one model's window (a conversation
// opening or switching to it). A pinned model returns at once; an unknown
// auto one is grace-capped so the first open still gets a denominator.
func (s *Server) refreshContextWindowFor(model string) {
	if s.cfg.LLM.ContextSizeFor(model) > 0 {
		return
	}
	if s.contextWindow(model) > 0 {
		s.ensureProbe()
		return
	}
	s.awaitProbe()
}

// pinnedOnly reports whether discovery has nothing to contribute.
func (s *Server) pinnedOnly() bool { return !s.cfg.LLM.AnyContextAuto() }

// autoWindowsKnown reports whether every auto picker model already has a
// cached window — the difference between an async refresh and a wait.
func (s *Server) autoWindowsKnown() bool {
	for _, m := range s.cfg.LLM.Models {
		if m.ContextSize > 0 {
			continue
		}
		if _, ok := s.discoveredWindows.Load(strings.ToLower(m.ID)); !ok {
			return false
		}
	}
	return true
}

// doProbe runs one probe and caches every window it reports. A failure
// leaves the last good values in place — stale beats none.
func (s *Server) doProbe() {
	if s.windowProbe == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), windowProbeTimeout)
	defer cancel()
	windows, ok := s.windowProbe(ctx)
	if !ok {
		return
	}
	for id, w := range windows {
		prev, existed := s.discoveredWindows.Swap(id, w)
		if !existed || prev != w {
			s.logger.Info("context window discovered", "tokens", w, "model", id)
		}
	}
}

// overflowLine is the estimate a request must stay under: the context window
// MINUS the output reserve (llm.max_tokens_response). A window covers prompt
// and completion together; counting only the prompt against the full window
// lets every send overrun by the length of its own reply. A reserve that no
// longer fits (a DISCOVERED window smaller than the configured reserve)
// still subtracts: the line floors at zero and every send is refused with
// context_full — the operator sees "start a new chat" on a model that is
// really mis-provisioned, which is honest; the reserve silently vanishing
// (review2 finding 6) was not. The request side clamps max_tokens with the
// same arithmetic (see runModelRound). Unknown window: not known, and the
// gate stays open — the endpoint answers for us.
func (s *Server) overflowLine(model string) (int64, bool) {
	window := s.contextWindow(model)
	if window <= 0 {
		return 0, false
	}
	reserve := int64(s.cfg.LLM.MaxTokensResponse)
	if reserve > 0 {
		if reserve > window {
			reserve = window
		}
		return window - reserve, true
	}
	return window, true
}

// fullRefusal writes the gate's two 409s — identical wording wherever a
// send, regenerate or replay meets them.
func (s *Server) fullRefusal(w http.ResponseWriter, r *http.Request, reason string) {
	if reason == "history_limit" {
		writeAPIError(w, r, http.StatusConflict, "history_limit",
			"this conversation has outgrown the message limit for context replay — start a new chat")
		return
	}
	writeAPIError(w, r, http.StatusConflict, "context_full",
		"this conversation has reached its context window — start a new chat")
}

// windowsCache is the per-model discovered-window store: lower-cased
// endpoint model id → tokens. Written by the probe, read by every
// conversation path; the lock is a plain RWMutex because the values are
// few and reads are short.
type windowsCache struct {
	mu sync.RWMutex
	m  map[string]int
}

func newWindowsCache() *windowsCache { return &windowsCache{m: map[string]int{}} }

func (w *windowsCache) Load(id string) (int, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	v, ok := w.m[id]
	return v, ok
}

// Swap stores the new window and reports the previous value plus whether
// the id was seen before, so the caller logs only real changes.
func (w *windowsCache) Swap(id string, v int) (int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev, existed := w.m[id]
	w.m[id] = v
	return prev, existed
}
