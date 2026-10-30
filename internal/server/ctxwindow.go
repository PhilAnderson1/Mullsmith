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
	"strings"
	"sync"
	"time"

	"mullsmith/internal/agent"
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
		go s.doProbe()
		return
	}
	s.refreshContextWindowsGraceful()
}

// refreshContextWindowsGraceful waits for a probe, but never longer than
// the grace. Overlapping waits may each start a probe; that is cheap and
// the last write wins with the same number.
func (s *Server) refreshContextWindowsGraceful() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.doProbe()
	}()
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
		go s.doProbe()
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.doProbe()
	}()
	select {
	case <-done:
	case <-time.After(windowGrace):
	}
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

// conversationFull answers "would the next request overflow?" for a
// conversation, given the content about to be appended (the user's
// message, or an attachment manifest line) and the model its next request
// will carry. It is honest-by-estimate: agent.EstimateTokens is runes/4,
// so a send may cross the real line a little early or late. When the
// window is unknown the gate is open — the endpoint answers for us.
func (s *Server) conversationFull(convID, model, extra string) bool {
	window := s.contextWindow(model)
	if window <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hist, err := s.store.ContextHistory(ctx, convID, "")
	if err != nil {
		return false // store trouble is not this check's verdict to give
	}
	used := int64(agent.EstimateTokens(extra))
	for _, m := range hist {
		used += int64(agent.EstimateTokens(m.Content))
		for _, tc := range m.Calls {
			used += int64(agent.EstimateTokens(tc.Arguments))
		}
	}
	return used >= window
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
