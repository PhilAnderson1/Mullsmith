package server

import (
	"gabkeep/internal/store"
	"gabkeep/internal/thinking"
)

// Server-side thinking-control plumbing (thinking-control-specification.md
// §5/§6). The Runtime in s.cfg.Thinking holds the validated catalogues and
// the effective choices; nothing here ever trusts a client-provided effort
// string or JSON fragment — a choice arrives as an ID and is resolved
// against the backend's own list.

// thinkingModel is the model ID a conversation's requests will carry —
// its own pick, or the server default — and thus part of the
// applicability fingerprint. It takes the conversation's raw model
// value so callers mid-patch can pass the value that is about to apply.
func (s *Server) thinkingModel(convModel string) string {
	if convModel != "" {
		return convModel
	}
	return s.cfg.LLM.Model
}

// thinkingMeta is the meta endpoint's slice of the feature: the effective
// choice list when the selector is visible, null otherwise.
func (s *Server) thinkingMeta() any {
	rt := s.cfg.Thinking
	if !rt.SelectorVisible() {
		return nil
	}
	return map[string]any{"choices": rt.Choices()}
}

// thinkingChoice projects a conversation's thinking selection for the API:
// the saved choice when it is still available under a matching
// endpoint/model/profile context, otherwise the built-in Default — the
// visible reset of §5, rendered rather than hidden.
func (s *Server) thinkingChoice(c *store.Conversation) string {
	rt := s.cfg.Thinking
	if !rt.SelectorVisible() {
		return thinking.DefaultID
	}
	if id, ok := rt.ResolveSaved(c.Thinking, c.ThinkingCtx, s.thinkingModel(c.Model)); ok {
		return id
	}
	return thinking.DefaultID
}

// validateThinkingChoice checks a client-submitted choice ID for the
// conversation (model = the model it will run on after any same-request
// model patch). Default ("", "default") stores as nothing; anything else
// must be among the effective choices of a visible selector. The bool is
// false for stale/invalid input — the API answers a validation error and
// never silently translates (§5).
func (s *Server) validateThinkingChoice(model, id string) (choice, fingerprint string, ok bool) {
	rt := s.cfg.Thinking
	if id == "" || id == thinking.DefaultID {
		return "", "", true
	}
	if !rt.SelectorVisible() {
		return "", "", false
	}
	if _, found := rt.Resolve(id); !found {
		return "", "", false
	}
	return id, rt.Fingerprint(model), true
}

// thinkingSnapshot renders the conversation's thinking choice into the
// request fragment ONCE, at generation start. Tool continuations of that
// generation reuse the snapshot: changing the selector afterwards affects
// the next request, never an in-flight stream (§5).
func (s *Server) thinkingSnapshot(conv *store.Conversation) map[string]any {
	rt := s.cfg.Thinking
	if !rt.SelectorVisible() {
		return nil
	}
	id, ok := rt.ResolveSaved(conv.Thinking, conv.ThinkingCtx, s.thinkingModel(conv.Model))
	if !ok {
		id = thinking.DefaultID // stale context: this generation runs Default
	}
	ch, ok := rt.Resolve(id)
	if !ok {
		return nil
	}
	frag, err := rt.Fragment(ch)
	if err != nil {
		// Load-time validation makes this unreachable for configured
		// choices; should it ever fire, the safe send is no override.
		s.logger.Warn("thinking fragment render failed; sending without override",
			"choice", id, "endpoint_type", s.cfg.LLM.EndpointType,
			"profile", s.cfg.LLM.ThinkingProfile, "error", err)
		return nil
	}
	return frag
}
