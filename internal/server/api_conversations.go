package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"gabkeep/internal/id"
	"gabkeep/internal/scratch"
	"gabkeep/internal/store"
)

// Field caps for conversation metadata (spec §9.11).
const (
	maxTitleLen        = 200
	maxSystemPromptLen = 8000
	maxTitleSearchLen  = 100
)

func conversationJSON(c *store.Conversation) map[string]any {
	return map[string]any{
		"id":            c.ID,
		"title":         c.Title,
		"model":         c.Model,
		"system_prompt": c.SystemPrompt,
		"created_at":    c.CreatedAt.UTC().Format(timeRFC3339),
		"updated_at":    c.UpdatedAt.UTC().Format(timeRFC3339),
	}
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
	if m.ToolCallID != "" {
		j["tool_call_id"] = m.ToolCallID
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
		out = append(out, conversationJSON(&items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// --- POST /api/v1/conversations ----------------------------------------------------

func (s *Server) validateModel(model string) bool {
	if model == "" {
		return true // server default
	}
	for _, m := range s.cfg.LLM.Models {
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
	writeJSON(w, http.StatusCreated, conversationJSON(c))
}

// --- GET / PATCH / DELETE /api/v1/conversations/{id} ----------------------------------

func (s *Server) handleConversationGet(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	c, err := s.store.ConversationForUser(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, conversationJSON(c))
}

func (s *Server) handleConversationPatch(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		Title        *string `json:"title"`
		Model        *string `json:"model"`
		SystemPrompt *string `json:"system_prompt"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Title == nil && body.Model == nil && body.SystemPrompt == nil {
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
	c, err := s.store.UpdateConversation(r.Context(), user.ID, r.PathValue("id"), p)
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, conversationJSON(c))
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
