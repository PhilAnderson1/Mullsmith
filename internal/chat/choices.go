package chat

import (
	"slices"

	"mullsmith/internal/store"
	"mullsmith/internal/thinking"
)

func (s *Service) Model(convModel string) string {
	if convModel != "" && slices.Contains(s.cfg.LLM.ModelNames(), convModel) {
		return convModel
	}
	return s.cfg.LLM.DefaultModelID()
}

func (s *Service) ThinkingChoice(c *store.Conversation) string {
	rt := s.cfg.Thinking
	if !rt.SelectorVisible() {
		return thinking.DefaultID
	}
	if id, ok := rt.ResolveSaved(c.Thinking, c.ThinkingCtx, s.Model(c.Model)); ok {
		return id
	}
	return thinking.DefaultID
}

func (s *Service) ValidateThinkingChoice(model, id string) (choice, fingerprint string, ok bool) {
	rt := s.cfg.Thinking
	if id == "" || id == thinking.DefaultID {
		return "", "", true
	}
	if !rt.SelectorVisible() {
		return "", "", false
	}
	if _, found := rt.ResolveFor(model, id); !found {
		return "", "", false
	}
	return id, rt.Fingerprint(model), true
}

func (s *Service) ThinkingSnapshot(conv *store.Conversation) map[string]any {
	rt := s.cfg.Thinking
	if !rt.SelectorVisible() {
		return nil
	}
	model := s.Model(conv.Model)
	id, ok := rt.ResolveSaved(conv.Thinking, conv.ThinkingCtx, model)
	if !ok {
		id = thinking.DefaultID // stale context: this generation runs Default
	}
	ch, ok := rt.ResolveFor(model, id)
	if !ok {
		return nil
	}
	frag, err := rt.Fragment(model, ch)
	if err != nil {
		// Load-time validation makes this unreachable for configured
		// choices; should it ever fire, the safe send is no override.
		s.logger.Warn("thinking fragment render failed; sending without override",
			"choice", id, "endpoint_type", s.cfg.LLM.EndpointType,
			"profile", s.cfg.LLM.ThinkingProfileFor(model), "error", err)
		return nil
	}
	return frag
}
