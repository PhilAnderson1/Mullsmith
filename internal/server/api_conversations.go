package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mullsmith/internal/id"
	"mullsmith/internal/scratch"
	"mullsmith/internal/store"
)

// Field caps for conversation metadata (spec §9.11).
const (
	maxTitleLen        = 200
	maxSystemPromptLen = 8000
	maxTitleSearchLen  = 100
)

// conversationJSON projects a row for the API. thinking is the restored
// effective choice (saved-and-valid, else Default) so a fresh page load
// shows exactly what the next request will send.
func (s *Server) conversationJSON(c *store.Conversation) map[string]any {
	j := map[string]any{
		"id":    c.ID,
		"title": c.Title,
		// The live model, not the raw column: a stored name the config
		// no longer offers would strand the picker on a ghost (§10).
		"model":         s.thinkingModel(c.Model),
		"system_prompt": c.SystemPrompt,
		"thinking":      s.thinkingChoice(c),
		"created_at":    c.CreatedAt.UTC().Format(timeRFC3339),
		"updated_at":    c.UpdatedAt.UTC().Format(timeRFC3339),
	}
	// The context window of the model this conversation runs on — the
	// meter's denominator travels with the conversation, because the
	// window is a model property (§6.1). Unknown (auto, endpoint silent)
	// is null: the SPA then shows a bare token count.
	if w := s.contextWindow(s.thinkingModel(c.Model)); w > 0 {
		j["max_context_tokens"] = w
	}
	return j
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

// messageJSON projects a stored message for the API. userID owns the
// conversation and is the sandbox owner for the attachment stats.
func (s *Server) messageJSON(m *store.Message, userID string) map[string]any {
	j := map[string]any{
		"id":         m.ID,
		"role":       m.Role,
		"content":    m.Content,
		"status":     m.Status,
		"created_at": m.CreatedAt.UTC().Format(timeRFC3339),
	}
	if m.TokenCount != nil {
		j["token_count"] = *m.TokenCount
	}
	if m.CompletionTokens != nil && *m.CompletionTokens > 0 {
		j["completion_tokens"] = *m.CompletionTokens
	}
	if m.DurationMS != nil && *m.DurationMS > 0 {
		j["duration_ms"] = *m.DurationMS
	}
	if m.ToolCallID != "" {
		j["tool_call_id"] = m.ToolCallID
	}
	if m.Synthetic {
		// Server-inserted turn (view_image, §7.2): the client renders no
		// bubble for it. Absent means a genuine message.
		j["synthetic"] = true
	}
	if len(m.Calls) > 0 {
		calls := make([]map[string]any, 0, len(m.Calls))
		for _, tc := range m.Calls {
			calls = append(calls, map[string]any{
				"id":        tc.LLMCallID,
				"tool":      tc.Tool,
				"arguments": tc.Arguments,
				"status":    tc.Status,
				"result":    tc.Result,
			})
		}
		j["tool_calls"] = calls
	}
	if len(m.Attachments) > 0 {
		// Attachment chips are path pointers: name = disk basename, size
		// from a stat. A file deleted since the message was sent still
		// renders (a historical chip whose download 404s — by design, the
		// row records what WAS attached); it simply carries no size.
		atts := make([]map[string]any, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			item := map[string]any{
				"name":         a.Name(),
				"path":         a.Path,
				"download_url": fileDownloadURL(a.Path),
			}
			if fi, err := scratch.Stat(s.cfg.Scratchpad.Root, userID, a.Path); err == nil &&
				fi.Mode().IsRegular() {
				item["size"] = fi.Size()
			}
			atts = append(atts, item)
		}
		j["files"] = atts
	}
	return j
}

// writeConvErr maps store errors to the API envelope. Ownership failures
// always surface as 404 — never 403 — so other users' ids cannot be probed.
func (s *Server) writeConvErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrConversationNotFound):
		writeAPIError(w, r, http.StatusNotFound, "not_found", "conversation not found")
	case errors.Is(err, errInvalidCursor):
		writeAPIError(w, r, http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
	case errors.Is(err, store.ErrHistoryLimit):
		// A past-cap history is refused explicitly, never replayed
		// truncated (finding 2).
		writeAPIError(w, r, http.StatusConflict, "history_limit",
			"this conversation has outgrown the message limit for context replay — start a new chat")
	case errors.Is(err, store.ErrNotLastMessage):
		// BeginRegeneration found a newer row behind the one being
		// replaced (finding 4): the swap rolled back, nothing was lost.
		writeAPIError(w, r, http.StatusConflict, "not_last_message", "only the newest reply can be regenerated")
	default:
		s.logger.Error("conversation store error", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
	}
}

// errInvalidCursor distinguishes bad client cursors from store faults.
var errInvalidCursor = errors.New("invalid cursor")

func mapCursorErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "invalid cursor") {
		return errInvalidCursor
	}
	return err
}

// listParams parses ?cursor=&limit= (limit is advisory; the store clamps).
func listParams(r *http.Request) (cursor string, limit int) {
	cursor = r.URL.Query().Get("cursor")
	limit = 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	return cursor, limit
}

// --- GET /api/v1/conversations ---------------------------------------------------

func (s *Server) handleConversationsList(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > maxTitleSearchLen {
		q = q[:maxTitleSearchLen]
	}
	cursor, limit := listParams(r)
	items, next, err := s.store.ListConversations(r.Context(), user.ID, q, cursor, limit)
	if err != nil {
		s.writeConvErr(w, r, mapCursorErr(err))
		return
	}
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		out = append(out, s.conversationJSON(&items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// --- POST /api/v1/conversations ----------------------------------------------------

func (s *Server) validateModel(model string) bool {
	if model == "" {
		return true // server default
	}
	for _, m := range s.cfg.LLM.ModelNames() {
		if m == model {
			return true
		}
	}
	return false
}

func (s *Server) handleConversationCreate(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		Title        string `json:"title"`
		Model        string `json:"model"`
		SystemPrompt string `json:"system_prompt"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	title := strings.TrimSpace(body.Title)
	if utf8.RuneCountInString(title) > maxTitleLen {
		writeAPIError(w, r, http.StatusBadRequest, "title_too_long", "title too long")
		return
	}
	if !s.validateModel(body.Model) {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_model", "unknown model")
		return
	}
	if utf8.RuneCountInString(body.SystemPrompt) > maxSystemPromptLen {
		writeAPIError(w, r, http.StatusBadRequest, "system_prompt_too_long", "system prompt too long")
		return
	}
	c := &store.Conversation{
		ID:           id.New(),
		UserID:       user.ID,
		Title:        title,
		Model:        body.Model,
		SystemPrompt: body.SystemPrompt,
	}
	// Settings page: a conversation created without an explicit prompt
	// inherits the user's default. The row stores the copy, so later
	// edits of the default never rewrite an existing chat.
	if c.SystemPrompt == "" {
		c.SystemPrompt = user.DefaultSystemPrompt
	}
	if err := s.store.CreateConversation(r.Context(), c); err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	// A new chat is the refresh point for auto-detected context windows
	// (§2.2): async when one is already cached, grace-capped when not.
	if s.cfg.LLM.AnyContextAuto() {
		s.refreshContextWindows()
	}
	writeJSON(w, http.StatusCreated, s.conversationJSON(c))
}

// --- GET / PATCH / DELETE /api/v1/conversations/{id} ----------------------------------

func (s *Server) handleConversationGet(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	c, err := s.store.ConversationForUser(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	// Opening a conversation is its denominator's refresh point: async
	// once known, grace-capped on the first-ever look at this model.
	s.refreshContextWindowFor(s.thinkingModel(c.Model))
	writeJSON(w, http.StatusOK, s.conversationJSON(c))
}

func (s *Server) handleConversationPatch(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		Title        *string `json:"title"`
		Model        *string `json:"model"`
		SystemPrompt *string `json:"system_prompt"`
		Thinking     *string `json:"thinking"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Title == nil && body.Model == nil && body.SystemPrompt == nil && body.Thinking == nil {
		writeAPIError(w, r, http.StatusBadRequest, "no_fields", "nothing to update")
		return
	}
	p := store.ConversationPatch{}
	if body.Title != nil {
		t := strings.TrimSpace(*body.Title)
		if t == "" || utf8.RuneCountInString(t) > maxTitleLen {
			writeAPIError(w, r, http.StatusBadRequest, "title_too_long", "title must be 1-200 characters")
			return
		}
		p.Title = &t
	}
	if body.Model != nil {
		if !s.validateModel(*body.Model) {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_model", "unknown model")
			return
		}
		p.Model = body.Model
	}
	if body.SystemPrompt != nil {
		if utf8.RuneCountInString(*body.SystemPrompt) > maxSystemPromptLen {
			writeAPIError(w, r, http.StatusBadRequest, "system_prompt_too_long", "system prompt too long")
			return
		}
		p.SystemPrompt = body.SystemPrompt
	}
	if body.Thinking != nil {
		// The fingerprint must name the model this conversation runs on
		// AFTER this patch — a same-call model change is part of the new
		// choice's context, not a mismatch with it.
		model := ""
		switch {
		case body.Model != nil:
			model = *body.Model
		default:
			c, err := s.store.ConversationForUser(r.Context(), user.ID, r.PathValue("id"))
			if err != nil {
				s.writeConvErr(w, r, err)
				return
			}
			model = c.Model
		}
		choice, fp, ok := s.validateThinkingChoice(s.thinkingModel(model), *body.Thinking)
		if !ok {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_thinking_choice",
				"unknown thinking choice for this endpoint and profile")
			return
		}
		p.Thinking, p.ThinkingCtx = &choice, &fp
	}
	c, err := s.store.UpdateConversation(r.Context(), user.ID, r.PathValue("id"), p)
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	// A model switch may have just moved the conversation to a model
	// whose window was never looked at: refresh before projecting.
	if body.Model != nil {
		s.refreshContextWindowFor(s.thinkingModel(c.Model))
	}
	writeJSON(w, http.StatusOK, s.conversationJSON(c))
}

func (s *Server) handleConversationDelete(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if err := s.store.DeleteConversation(r.Context(), user.ID, r.PathValue("id")); err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	s.remembered.forget(r.PathValue("id")) // consent dies with the conversation (§9.9)
	w.WriteHeader(http.StatusNoContent)
}

// --- DELETE /api/v1/conversations?older_than= ---------------------------------------

// pruneAges are the sidebar's "Delete older than" choices and the tokens
// that carry them. Months and years are deliberate calendar approximations
// (30/365 days): a housekeeping threshold, not a billing period. "all"
// maps to zero so the store applies no age condition at all.
var pruneAges = map[string]time.Duration{
	"1d":  24 * time.Hour,
	"1w":  7 * 24 * time.Hour,
	"1mo": 30 * 24 * time.Hour,
	"1y":  365 * 24 * time.Hour,
	"all": 0,
}

func (s *Server) handleConversationsPrune(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	age, ok := pruneAges[r.URL.Query().Get("older_than")]
	if !ok {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_older_than",
			"older_than must be one of 1d, 1w, 1mo, 1y, all")
		return
	}
	var cutoff int64
	if age > 0 {
		cutoff = time.Now().Add(-age).Unix()
	}
	ids, err := s.store.PruneConversations(r.Context(), user.ID, cutoff)
	if err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "prune failed")
		return
	}
	for _, convID := range ids {
		s.remembered.forget(convID) // consent dies with the conversation (§9.9)
	}
	if ids == nil {
		ids = []string{} // a JSON contract: ids is always an array, never null
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(ids), "ids": ids})
}

// --- GET /api/v1/conversations/{id}/messages --------------------------------------------

func (s *Server) handleConversationMessages(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	cursor, limit := listParams(r)
	items, next, err := s.store.ListMessages(r.Context(), user.ID, r.PathValue("id"), cursor, limit)
	if err != nil {
		s.writeConvErr(w, r, mapCursorErr(err))
		return
	}
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		out = append(out, s.messageJSON(&items[i], user.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}
