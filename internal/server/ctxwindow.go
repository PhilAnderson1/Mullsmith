// Context-window resolution (spec §2.2 llm.max_context_tokens, §6.1). The
// window answers two questions: what denominator the SPA's meter divides
// by, and whether the next send would overflow before we pay for it.
//
// Resolution order: a spelled-out config number wins outright (discovery
// never runs); -1 (the default) means auto — the endpoint's GET /models
// listing is probed at startup, at the start of each new conversation and
// (briefly, once) by the first meter read. A window we have never resolved
// is simply not advertised: the UI degrades to "K tokens used" and sends
// are not gated — the endpoint's own error stands (agreed doctrine: never
// guess a number).
package server

import (
	"context"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/llm"
)

// windowGrace bounds how long a caller may wait for a first-ever probe.
// Once a value (or a failure verdict) exists, refreshes are always async:
// no request path ever blocks behind a slow endpoint.
const windowGrace = 1500 * time.Millisecond

// StartContextWindowProbe warms the auto-detected window once in the
// background at boot; main calls it. A pinned config ignores it.
func (s *Server) StartContextWindowProbe() {
	if s.cfg.LLM.MaxContextTokens > 0 {
		return
	}
	go s.refreshContextWindow()
}

// windowProbeTimeout bounds one GET /models. Kept well under typical send
// patience so even the grace-waited first probe stays a footnote.
const windowProbeTimeout = 3 * time.Second

// probeEndpointWindow is the default Server.windowProbe: the configured
// model's endpoint-reported window, or (0, false) when the listing is
// unreachable or says nothing. Absence is normal — context_length is not
// part of the OpenAI spec.
func (s *Server) probeEndpointWindow(ctx context.Context) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, windowProbeTimeout)
	defer cancel()
	models, err := llm.ProbeModels(ctx, s.cfg)
	if err != nil {
		s.logger.Info("context-window probe failed", "err", err)
		return 0, false
	}
	window, ok := llm.ContextLengthFor(models, s.cfg.LLM.DefaultModelID())
	if !ok {
		s.logger.Info("endpoint reports no context window", "model", s.cfg.LLM.DefaultModelID())
		return 0, false
	}
	return window, true
}

// contextWindow is the window in force: the pinned config number when > 0,
// else the discovered one; 0 means unknown. Struct-literal test configs
// leave MaxContextTokens at 0, which lands in the auto branch — harmless,
// their fake endpoints report nothing.
func (s *Server) contextWindow() int64 {
	if s.cfg.LLM.MaxContextTokens > 0 {
		return int64(s.cfg.LLM.MaxContextTokens)
	}
	return s.ctxWindow.Load()
}

// contextWindowMeta projects the window for /api/v1/meta: a number when
// known, nil (JSON null) while auto-detection has nothing — the SPA then
// shows "K tokens used" instead of a percentage. Unknown, it waits once
// for the first probe (grace-capped) so a page load that races startup
// still gets a real denominator.
func (s *Server) contextWindowMeta() any {
	if w := s.contextWindow(); w > 0 {
		return w
	}
	if s.cfg.LLM.MaxContextTokens > 0 {
		return nil // pinned-but-odd: never advertised as auto
	}
	s.refreshContextWindowGraceful()
	if w := s.contextWindow(); w > 0 {
		return w
	}
	return nil
}

// refreshContextWindow updates the cache in the background whenever a
// value already exists, and synchronously — grace-capped — when none does.
// A pinned config answers with the number: discovery never runs, not even
// as a stale-value refresh.
func (s *Server) refreshContextWindow() {
	if s.cfg.LLM.MaxContextTokens > 0 {
		return
	}
	if s.ctxWindow.Load() > 0 {
		go s.doProbe()
		return
	}
	s.refreshContextWindowGraceful()
}

// refreshContextWindowGraceful waits for a probe, but never longer than the
// grace. Overlapping waits may each start a probe; that is cheap and the
// last write wins with the same number.
func (s *Server) refreshContextWindowGraceful() {
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

// doProbe runs one probe and caches a success. A failure leaves the last
// good value in place — stale beats none.
func (s *Server) doProbe() {
	if s.windowProbe == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), windowProbeTimeout)
	defer cancel()
	if window, ok := s.windowProbe(ctx); ok && window > 0 {
		if prev := s.ctxWindow.Swap(int64(window)); prev != int64(window) {
			s.logger.Info("context window discovered", "tokens", window, "model", s.cfg.LLM.DefaultModelID())
		}
	}
}

// conversationFull answers "would the next request overflow?" for a
// conversation, given the content about to be appended (the user's
// message, or an attachment manifest line). It is honest-by-estimate:
// agent.EstimateTokens is runes/4, so a send may cross the real line a
// little early or late. When the window is unknown the gate is open —
// the endpoint answers for us.
func (s *Server) conversationFull(convID, extra string) bool {
	window := s.contextWindow()
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
