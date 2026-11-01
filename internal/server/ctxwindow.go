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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
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

// requestTokens estimates the ACTUAL built request (finding 7): every
// message's text, the tool-call arguments the protocol replays, and the
// advertised tool schemas — the material the previous gate simply did not
// count (system prompt and guidance ride in msgs[0]; schemas are what the
// endpoint tokenises alongside the conversation). Data URLs are NOT
// estimated: a picture's real cost is the endpoint's own resolution
// formula, and runes/4 over base64 would refuse sends wildly early; the
// manifest line a picture arrives with is counted.
func requestTokens(msgs []llm.ChatMessage, specs []llm.ToolSpec) int64 {
	var used int64
	for _, m := range msgs {
		used += int64(agent.EstimateTokens(m.Content))
		for _, tc := range m.ToolCalls {
			used += int64(agent.EstimateTokens(tc.Function.Name + tc.Function.Arguments))
		}
	}
	if len(specs) > 0 {
		if b, err := json.Marshal(specs); err == nil {
			used += int64(agent.EstimateTokens(string(b)))
		}
	}
	return used
}

// conversationFull answers "would the next request overflow?" — measured,
// not approximated (finding 7): the gate builds the SAME request the send
// would make (buildRequest: system prompt, guidance, tool schemas, manifests)
// plus the turn about to land (extra/attPaths; empty on regenerate), and
// compares the estimate with the window minus the output reserve.
// excludeMsgID drops the reply a regeneration replaces — counting it would
// refuse a replacement whose request excludes it.
//
// Still honest-by-estimate: agent.EstimateTokens is runes/4, so a send may
// cross the real line a little early or late. The second result is the
// refusal code — "context_full", or "history_limit" when the conversation
// has outgrown context replay itself (finding 2: a past-cap history is an
// explicit refusal, never a silently-truncated success; the row cap binds
// even when the window is unknown, and the incoming turn counts toward it
// — review2 finding 1). The third result is the history the verdict was
// based on (incoming turn appended), so the generation that proceeds builds
// its prompt from the SAME read — no second look can flip the decision
// after the turn was committed. Accept-with-store-trouble: (false, "", nil)
// — the caller re-reads before persisting, and that read's refusal stands.
func (s *Server) conversationFull(conv *store.Conversation, userID, extra string, attPaths []string, excludeMsgID string) (bool, string, []store.Message) {
	_, known := s.overflowLine(s.thinkingModel(conv.Model))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hist, err := s.store.ContextHistory(ctx, conv.ID, excludeMsgID)
	if errors.Is(err, store.ErrHistoryLimit) {
		return true, "history_limit", nil
	}
	if err != nil {
		return false, "", nil
	}
	// The row cap binds EVEN WITHOUT a discovered window (review2
	// finding 1): it limits context REPLAY, not token cost, so the
	// unknown-window case cannot skip it. The refusal point is where the
	// NEXT ContextHistory read would fail — persisted rows (hist, already
	// excluding the regenerated reply) PLUS the turn about to land
	// (send: one row; regenerate: none, the placeholder replaces the
	// retired one) past the cap. len+incoming > cap, not >=: a history
	// AT the cap is still readable, and a send that lands exactly ON it
	// is the last one the cap allows rather than one refused early.
	incoming := 0
	if extra != "" || len(attPaths) > 0 {
		incoming = 1
	}
	if len(hist)+incoming > store.ContextHistoryRows() {
		return true, "history_limit", nil
	}
	// The turn about to land rides the history the generation will build
	// its prompt from — one read for decision AND request, so the caller
	// never re-reads (and never 409s after committing).
	if extra != "" || len(attPaths) > 0 {
		turn := store.Message{Role: "user", Content: extra}
		for _, p := range attPaths {
			turn.Attachments = append(turn.Attachments, store.Attachment{Path: p})
		}
		hist = append(hist, turn)
	}
	if !known {
		// Open token gate (window unknown: the endpoint arbitrates),
		// but the row-cap verdict above stands.
		return false, "", hist
	}
	msgs, specs := s.buildRequest(conv, userID, hist)
	line, _ := s.overflowLine(s.thinkingModel(conv.Model))
	return requestTokens(msgs, specs) >= line, "context_full", hist
}

// continuationFull re-measures a growing tool continuation against the same
// overflow line admission applied (finding 7): by round N the transcript
// carries every result so far, and no further check had ever run. roundTools
// is what THIS round would advertise — nil once maxRounds forces the final
// answer, whose schemas then leave the request.
//
// The line already subtracts the FULL output reserve, so a continuation
// under it always leaves the request's max_tokens affordable — one figure,
// gate and wire agree (review2 finding 6's consistency requirement). When
// the reserve no longer fits the window at all, the line is at or below
// zero and every send — first or continuation — is refused with
// context_full instead of the reserve being silently dropped.
func (s *Server) continuationFull(conv *store.Conversation, msgs []llm.ChatMessage, roundTools []llm.ToolSpec) bool {
	line, known := s.overflowLine(s.thinkingModel(conv.Model))
	return known && requestTokens(msgs, roundTools) >= line
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
