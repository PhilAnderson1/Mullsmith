package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mullsmith/internal/chat"
	"mullsmith/internal/scratch"
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
		Content         string   `json:"content"`
		AttachmentPaths []string `json:"attachment_paths"`
		Thinking        *string  `json:"thinking"`
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
	attPaths, ok := s.validateAttachmentPaths(w, r, user.ID, body.AttachmentPaths)
	if !ok {
		return
	}
	// Everything beyond request-shape validation runs inside ONE operation
	// boundary (C1): runGeneration reserves the conversation, reads the
	// authoritative snapshot under that reservation, admits the request and
	// commits it — in that order, with no second history read. The hourly
	// bucket, the busy refusals and the overflow gate all answer BEFORE
	// anything is persisted; a thinking choice shipped with the body is
	// validated and stored there too, so a capacity refusal can never
	// leave it applied (C1).
	s.runGeneration(w, r, chat.Command{
		ConversationID:  conv.ID,
		AutoTitle:       content, // may name a still-unnamed conversation (§12)
		Content:         content,
		AttachmentPaths: attPaths,
		Thinking:        body.Thinking,
	})
}

// validateAttachmentPaths checks count, shape and existence up front so a
// bad path fails with 4xx BEFORE the user message is persisted. A path is
// accepted only when it is canonical (ParseRel rejoined), at least two
// segments deep, and inside the session user's uploads/ — ownership is
// STRUCTURAL: scratch.Stat resolves rel only under this user's tree
// (§9.7), so another user's file cannot even be addressed, and every
// rejection answers the same attachment_not_found (§9.2).
func (s *Server) validateAttachmentPaths(w http.ResponseWriter, r *http.Request, userID string, raws []string) ([]string, bool) {
	if len(raws) == 0 {
		return nil, true
	}
	max := s.cfg.Limits.MaxAttachments
	if len(raws) > max {
		writeAPIError(w, r, http.StatusBadRequest, "too_many_attachments",
			"at most "+strconv.Itoa(max)+" attachments per message")
		return nil, false
	}
	seen := make(map[string]bool, len(raws))
	out := make([]string, 0, len(raws))
	for _, raw := range raws {
		segs, perr := scratch.ParseRel(strings.TrimSpace(raw))
		// "../x" never survives ParseRel; "uploads" itself (one segment)
		// is not a file; anything outside uploads/ is not an attachment.
		if perr != nil || len(segs) < 2 || segs[0] != "uploads" {
			writeAPIError(w, r, http.StatusBadRequest, "attachment_not_found",
				"attachment does not exist")
			return nil, false
		}
		rel := strings.Join(segs, "/") // the canonical spelling enters the DB
		if seen[rel] {
			continue
		}
		seen[rel] = true
		fi, serr := scratch.Stat(s.cfg.Scratchpad.Root, userID, rel)
		if serr != nil {
			if errors.Is(serr, scratch.ErrNotFound) || errors.Is(serr, scratch.ErrPathUnsafe) {
				writeAPIError(w, r, http.StatusBadRequest, "attachment_not_found",
					"attachment does not exist: "+rel)
			} else {
				s.logger.Error("attachment stat", "error", serr, "rid", RequestIDFrom(r.Context()))
				writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
			}
			return nil, false
		}
		if !fi.Mode().IsRegular() { // dirs, FIFOs, symlinks: not attachments
			writeAPIError(w, r, http.StatusBadRequest, "attachment_not_found",
				"attachment does not exist: "+rel)
			return nil, false
		}
		out = append(out, rel)
	}
	return out, true
}

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
	// runGeneration secures the per-conversation slot BEFORE admission or
	// acceptance, so a refusal leaves the existing reply untouched.
	// Regeneration never retitles: the conversation already has its name.
	// The overflow gate applies here too — retrying a reply is still a
	// send — and it measures the request the REPLACEMENT would make: the
	// retired turn is excluded, because it never rides the regenerated
	// prompt (finding 7's other half — refusing over content that will not
	// be sent). "Newest" is deliberately NOT pre-checked here:
	// BeginTurnRegeneration re-checks it INSIDE the swap and answers
	// not_last_message via writeConvErr.
	s.runGeneration(w, r, chat.Command{
		ConversationID: conv.ID,
		RegenerateID:   m.ID,
	})
}

// --- POST /api/v1/generations/{id}/abort (spec §6.2) -----------------------------------

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if !s.chat.Generations.Abort(r.PathValue("id"), user.ID) {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "no such generation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /api/v1/generations/{id}/approvals (spec §6.1 step 5) -----------------------

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		ApprovalID string `json:"approval_id"`
		Approve    bool   `json:"approve"`
		Remember   bool   `json:"remember"` // "always allow in this conversation" (§6.1)
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.ApprovalID == "" {
		writeAPIError(w, r, http.StatusBadRequest, "missing_approval_id", "approval_id is required")
		return
	}
	if !s.chat.Generations.ResolveApproval(r.PathValue("id"), body.ApprovalID, user.ID, body.Approve, body.Remember) {
		// Already answered, timed out, or not ours: indistinguishable 404
		// (§9.2: never probe-able).
		writeAPIError(w, r, http.StatusNotFound, "not_found", "no pending approval")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared streaming machinery ----------------------------------------------------------

func (s *Server) runGeneration(w http.ResponseWriter, r *http.Request, cmd chat.Command) {
	cmd.RequestID = RequestIDFrom(r.Context())
	op, err := s.chat.Begin(r.Context(), UserFrom(r.Context()).ID, cmd)
	if err != nil {
		var refusal *chat.Refusal
		switch {
		case errors.As(err, &refusal):
			if refusal.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(refusal.RetryAfter))
			}
			if refusal.Code == "context_full" || refusal.Code == "history_limit" {
				s.fullRefusal(w, r, refusal.Code)
				return
			}
			status := http.StatusBadRequest
			if refusal.Code == "chat_limited" {
				status = http.StatusTooManyRequests
			}
			writeAPIError(w, r, status, refusal.Code, refusal.Message)
		case errors.Is(err, chat.ErrUserStreams):
			writeAPIError(w, r, http.StatusTooManyRequests, "too_many_streams", "you already have the maximum number of generations running")
		case errors.Is(err, chat.ErrGlobalStreams):
			writeAPIError(w, r, http.StatusServiceUnavailable, "server_busy", "the server is at its generation capacity, try again shortly")
		case errors.Is(err, chat.ErrGenerating):
			writeAPIError(w, r, http.StatusConflict, "generation_in_progress", "a reply is already being generated in this conversation")
		default:
			s.writeConvErr(w, r, err)
		}
		return
	}
	defer op.Close()
	conn, err := newSSE(w)
	if err != nil {
		op.AbortBeforeStart()
		return
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if conn.Comment("ping") != nil {
					op.Cancel()
					return
				}
			}
		}
	}()
	op.Run(func(event chat.Event) error { return conn.Event(event.Kind(), event) })
}
