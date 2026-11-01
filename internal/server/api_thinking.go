package server

import (
	"mullsmith/internal/store"
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
// thinkingModel is the model a conversation actually RUNS on: its own
// selection when that name is still in the deployment's model list,
// else the default. A conversation pinned to a model the config no
// longer offers (endpoint switched, entry removed) must not drag that
// dead name onto the wire or into the picker — it silently rides the
// default instead. The stored choice survives, so a config that later
// re-adds the model adopts it back.
func (s *Server) thinkingModel(convModel string) string { return s.chat.Model(convModel) }

// thinkingMeta is the meta endpoint's slice of the feature: the choice
// list PER MODEL (a picker entry's dropdown describes its model), or
// null when the selector is off. The SPA picks the list matching the
// conversation's active model (its own, else default_model).
func (s *Server) thinkingMeta() any {
	rt := s.cfg.Thinking
	if !rt.SelectorVisible() {
		return nil
	}
	byModel := map[string]any{}
	for _, m := range s.cfg.LLM.ModelNames() {
		byModel[m] = rt.Choices(m)
	}
	return map[string]any{"choices_by_model": byModel}
}

// thinkingChoice projects a conversation's thinking selection for the API:
// the saved choice when it is still available under a matching
// endpoint/model/profile context, otherwise the built-in Default — the
// visible reset of §5, rendered rather than hidden.
func (s *Server) thinkingChoice(c *store.Conversation) string { return s.chat.ThinkingChoice(c) }

// validateThinkingChoice checks a client-submitted choice ID for the
// conversation (model = the model it will run on after any same-request
// model patch). Default ("", "default") stores as nothing; anything else
// must be among the effective choices of a visible selector. The bool is
// false for stale/invalid input — the API answers a validation error and
// never silently translates (§5).
func (s *Server) validateThinkingChoice(model, id string) (choice, fingerprint string, ok bool) {
	return s.chat.ValidateThinkingChoice(model, id)
}
