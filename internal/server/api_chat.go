package server

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gabkeep/internal/agent"
	"gabkeep/internal/llm"
	"gabkeep/internal/store"
)

// --- POST /api/v1/conversations/{id}/messages — the core endpoint (spec §5.2) ----

func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	conv, err := s.store.ConversationForUser(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	var body struct {
		Content       string   `json:"content"`
		AttachmentIDs []string `json:"attachment_ids"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	content := strings.TrimRight(body.Content, " \t\r\n")
	if content == "" {
		writeAPIError(w, r, http.StatusBadRequest, "empty_message", "message is empty")
		return
	}
	if utf8.RuneCountInString(content) > s.cfg.Limits.MaxMessageChars {
		writeAPIError(w, r, http.StatusBadRequest, "message_too_long", "message exceeds the size limit")
		return
	}
	if len(body.AttachmentIDs) > 0 {
		writeAPIError(w, r, http.StatusBadRequest, "attachments_unavailable", "attachments land in milestone M5")
		return
	}

	// Persist the user turn first (spec §6.1 step 1), then stream.
	s.streamAssistant(w, r, conv, func(bg context.Context) error {
		return s.store.AddMessage(bg, user.ID, &store.Message{
			ConversationID: conv.ID, Role: "user", Content: content,
			TokenCount: int64ptr(int64(agent.EstimateTokens(content))),
		})
	})
}

// --- POST /api/v1/messages/{id}/regenerate (spec §5.2) ------------------------------

func (s *Server) handleRegenerate(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	m, err := s.store.GetMessageForUser(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	if m.Role != "assistant" {
		writeAPIError(w, r, http.StatusBadRequest, "not_regeneratable", "only assistant messages can be regenerated")
		return
	}
	conv, err := s.store.ConversationForUser(r.Context(), user.ID, m.ConversationID)
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	last, err := s.store.IsLastMessageInConversation(r.Context(), conv.ID, m.ID)
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	if !last {
		writeAPIError(w, r, http.StatusBadRequest, "not_last_message", "only the newest reply can be regenerated")
		return
	}
	// streamAssistant secures the per-conversation slot BEFORE prepare runs,
	// so a 409 leaves the existing reply untouched.
	s.streamAssistant(w, r, conv, func(bg context.Context) error {
		return s.store.DeleteMessage(bg, m.ID)
	})
}

// --- POST /api/v1/generations/{id}/abort (spec §6.2) -----------------------------------

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if !s.gens.Abort(r.PathValue("id"), user.ID) {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "no such generation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared streaming machinery ----------------------------------------------------------

// streamAssistant runs one assistant turn: it reserves the conversation
// slot (409 when busy), applies the prepare mutation (user message insert
// or regenerate delete), creates the streaming placeholder, then pumps SSE
// until completion, abort or upstream failure.
//
// All persistence happens on a context detached from the browser request so
// that disconnect-triggered aborts can still save the partial answer
// (spec §6.2).
func (s *Server) streamAssistant(w http.ResponseWriter, r *http.Request,
	conv *store.Conversation, prepare func(bg context.Context) error,
) {
	user := UserFrom(r.Context())

	if max := s.cfg.Limits.ConcurrentStreamsPerUser; max > 0 && s.gens.CountForUser(user.ID) >= max {
		writeAPIError(w, r, http.StatusTooManyRequests, "too_many_streams",
			"you already have the maximum number of generations running")
		return
	}

	// genCtx lives independent of r.Context(); a watcher cancels it when the
	// browser goes away.
	genCtx, cancel := context.WithCancel(context.Background())
	g, err := s.gens.Start(conv.ID, user.ID, cancel)
	if err != nil {
		cancel()
		writeAPIError(w, r, http.StatusConflict, "generation_in_progress",
			"a reply is already being generated in this conversation")
		return
	}

	handlerDone := make(chan struct{})
	defer close(handlerDone)
	defer cancel()
	defer s.gens.Finish(g)

	go func() {
		select {
		case <-r.Context().Done():
			cancel()
		case <-handlerDone:
		}
	}()

	if err := prepare(genCtx); err != nil {
		s.writeConvErr(w, r, err)
		return
	}

	am := &store.Message{ConversationID: conv.ID, Role: "assistant", Status: "streaming"}
	if err := s.store.AddMessage(genCtx, user.ID, am); err != nil {
		s.writeConvErr(w, r, err)
		return
	}

	// Context assembly (spec §6.1 step 2).
	hist, err := s.store.ContextHistory(genCtx, conv.ID, am.ID)
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	msgs := make([]llm.ChatMessage, 0, len(hist))
	for i := range hist {
		msgs = append(msgs, llm.ChatMessage{Role: hist[i].Role, Content: hist[i].Content})
	}
	msgs = agent.BuildContext(conv.SystemPrompt, msgs,
		s.cfg.LLM.MaxContextTokens, s.cfg.LLM.MaxTokensResponse)

	// From here on, errors travel as SSE events.
	conn, err := newSSE(w)
	if err != nil {
		s.finishMessage(genCtx, am.ID, "", "aborted", 0)
		return
	}
	if err := conn.Event("start", map[string]any{
		"generation_id":   g.ID,
		"message_id":      am.ID,
		"conversation_id": conv.ID,
	}); err != nil {
		s.finishMessage(genCtx, am.ID, "", "aborted", 0)
		return
	}

	// Heartbeat every 15 s: keeps proxies warm and surfaces dead clients via
	// a write error, which cancels generation.
	hbStop := make(chan struct{})
	defer close(hbStop)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				if err := conn.Comment("ping"); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	res := s.generator.Generate(genCtx, conv.Model, msgs, func(kind, text string) error {
		event := "token"
		if kind == agent.EventReasoning {
			if !s.cfg.LLM.ShowReasoning {
				return nil // accumulated server-side, not shown (spec §6.2)
			}
			event = "reasoning"
		}
		if err := conn.Event(event, map[string]any{"t": text}); err != nil {
			return agent.ErrClientGone
		}
		return nil
	})

	s.finishMessage(genCtx, am.ID, res.Content, res.Status, res.Usage)
	if res.Err != nil {
		s.logger.Warn("generation failed", "error", res.Err, "user", user.ID,
			"conversation", conv.ID, "rid", RequestIDFrom(r.Context()))
		_ = conn.Event("error", map[string]any{
			"code":       "upstream_error",
			"message":    "the model endpoint failed; try again",
			"request_id": RequestIDFrom(r.Context()),
		})
	}
	_ = conn.Event("done", map[string]any{
		"message_id":   am.ID,
		"status":       res.Status,
		"usage_tokens": res.Usage,
	})
}

// finishMessage persists the final state on a short background context: the
// browser may already be gone (and genCtx already cancelled) when we save.
func (s *Server) finishMessage(_ context.Context, msgID, content, status string, usage int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FinalizeMessage(ctx, msgID, content, status, usage); err != nil {
		s.logger.Error("failed to persist message final state", "error", err, "message", msgID)
	}
}

func int64ptr(v int64) *int64 { return &v }
