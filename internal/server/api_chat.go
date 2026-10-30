package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mullsmith/internal/agent"
	"mullsmith/internal/id"
	"mullsmith/internal/llm"
	"mullsmith/internal/scratch"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
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
	// Overflow gate (spec §6.1): with a known window, a send that would
	// cross it is refused before anything is persisted or paid for — the
	// conversation is append-only until the end. Unknown window: the gate
	// is open and the endpoint's own error stands.
	if s.conversationFull(conv.ID, content) {
		writeAPIError(w, r, http.StatusConflict, "context_full",
			"this conversation has reached its context window — start a new chat")
		return
	}
	// A thinking choice shipped with the message is validated against the
	// backend's effective list and persisted BEFORE anything is streamed:
	// an invalid one is a plain 400 that never reaches the endpoint, and
	// a valid one is this generation's snapshot — later tool rounds of the
	// same reply speak it even if the selector moves mid-flight.
	if body.Thinking != nil {
		choice, fp, ok := s.validateThinkingChoice(s.thinkingModel(conv.Model), *body.Thinking)
		if !ok {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_thinking_choice",
				"unknown thinking choice for this endpoint and profile")
			return
		}
		if choice != conv.Thinking || fp != conv.ThinkingCtx {
			updated, err := s.store.UpdateConversation(r.Context(), user.ID, conv.ID,
				store.ConversationPatch{Thinking: &choice, ThinkingCtx: &fp})
			if err != nil {
				s.writeConvErr(w, r, err)
				return
			}
			conv = updated
		}
	}

	// Persist the user turn first (spec §6.1 step 1), then stream. The user
	// text also seeds auto-titling when the conversation is still unnamed.
	s.streamAssistant(w, r, conv, content, func(bg context.Context) error {
		msg := &store.Message{
			ConversationID: conv.ID, Role: "user", Content: content,
			TokenCount: int64ptr(int64(agent.EstimateTokens(content))),
		}
		if err := s.store.AddMessage(bg, user.ID, msg); err != nil {
			return err
		}
		if len(attPaths) > 0 {
			return s.store.AttachFilesToMessage(bg, msg.ID, user.ID, attPaths)
		}
		return nil
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
	if max <= 0 { // defensive: configs built outside config.Load
		max = 8
	}
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

// manifestOpts shapes one attachmentManifest build. visionOn advertises
// view_image in the header (only when the tool can actually be offered);
// embed carries the bytes as data: URLs (view turns only); maxBytes is
// the per-picture cap.
type manifestOpts struct {
	visionOn bool
	embed    bool
	maxBytes int64
}

// attachmentManifest tells the model where a turn's attachments live so
// it can fs_read them (§7.3). The manifest is generated server-side;
// upload CONTENT remains untrusted data. Size and existence come from
// the disk — the single source of truth: an attachment that vanished
// since (the file, or a work/ TTL sweep) is skipped in silence (the
// historical chat chip stays, the model is simply not told about bytes
// that are not there).
//
// Uploads NEVER travel as pictures: since view_image (§7.2) the model
// asks for the ones it wants, so image bytes cross the wire only when
// they matter to the answer. o.embed is that asking made flesh — used
// for the synthetic view_image turn, whose persisted marker makes
// historyToLLM re-embed it on every replay. A missing or oversized file
// simply drops out of urls (and the inline=yes annotation), so the
// message degrades to manifest-only, a plain string on the wire.
func attachmentManifest(root, userID string, atts []store.Attachment,
	o manifestOpts,
) (string, []string) {
	var b strings.Builder
	var urls []string
	head := "\n\n[Attached files — stored in your scratchpad; open them with fs_read"
	if o.visionOn {
		head += "; to see an image, call view_image with its path"
	}
	b.WriteString(head + ":]")
	for _, a := range atts {
		fi, err := scratch.Stat(root, userID, a.Path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		note := ""
		if o.embed {
			if url, ok := imageDataURL(root, userID, a.Path, o.maxBytes); ok {
				urls = append(urls, url)
				note = " inline=yes"
			}
		}
		b.WriteString("\n- name=" + jsonQuote(a.Name()) +
			" path=" + jsonQuote(a.Path) +
			" media=" + scratch.MediaTypeForPath(a.Path) +
			" bytes=" + strconv.FormatInt(fi.Size(), 10) + note)
	}
	return b.String(), urls
}

// imageDataURL reads a sandboxed file (§9.7 open chain) and encodes it as
// a data: URL when its SNIFFED content type is a raster image endpoints
// consume. The type comes from the bytes (http.DetectContentType, spec
// §9.4), never the extension: an SVG or HTML file wearing a .png name is
// not a picture, and active content must never reach the model's context
// as one. Reads are capped: a file that keeps reading past maxBytes is
// refused whole, not truncated into a corrupt image.
func imageDataURL(root, userID, rel string, maxBytes int64) (string, bool) {
	f, err := scratch.OpenFile(root, userID, rel)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes || len(data) == 0 {
		return "", false
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return "", false
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
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
	// so a 409 leaves the existing reply untouched. Regeneration never
	// retitles: the conversation already has its name. The overflow gate
	// applies here too — retrying a reply is still a send.
	if s.conversationFull(conv.ID, "") {
		writeAPIError(w, r, http.StatusConflict, "context_full",
			"this conversation has reached its context window — start a new chat")
		return
	}
	s.streamAssistant(w, r, conv, "", func(bg context.Context) error {
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

// --- POST /api/v1/generations/{id}/approvals (spec §6.1 step 5) -----------------------

// approvalTimeout bounds how long a tool call waits on the user; past it
// the call is denied. A var so tests can shrink the wait.
var approvalTimeout = 10 * time.Minute

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
	if !s.gens.ResolveApproval(r.PathValue("id"), body.ApprovalID, user.ID, body.Approve, body.Remember) {
		// Already answered, timed out, or not ours: indistinguishable 404
		// (§9.2: never probe-able).
		writeAPIError(w, r, http.StatusNotFound, "not_found", "no pending approval")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared streaming machinery ----------------------------------------------------------

// toolResultBudgetRunes bounds one tool result as replayed to the model
// (spec §6.1 step 5, middle-elided).
const toolResultBudgetRunes = 8 * 1024

// streamAssistant runs the §6.1 flow end to end: reserve the slot, apply
// the prepare mutation, then loop model-round → tool-calls → tool-results
// until the model answers (or the round/approval rules cut it short),
// pumping SSE the whole way.
//
// autoTitle is the user text that may name an still-unnamed conversation
// (empty from regenerate; see nameConversation).
//
// All persistence happens on a context detached from the browser request so
// that disconnect-triggered aborts can still save the partial answer
// (spec §6.2).
func (s *Server) streamAssistant(w http.ResponseWriter, r *http.Request,
	conv *store.Conversation, autoTitle string, prepare func(bg context.Context) error,
) {
	user := UserFrom(r.Context())

	// Hourly chat bucket (§9.11 limits.chat_messages_per_hour — was
	// configured but never enforced, audit finding 3): refused before any
	// persistence, slot reservation or LLM work.
	if max := s.cfg.Limits.ChatMessagesPerHour; max > 0 &&
		!s.limiter.Allow("chat:"+user.ID, max, time.Hour) {
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Hour.Seconds())))
		writeAPIError(w, r, http.StatusTooManyRequests, "chat_limited",
			"hourly message limit reached, please try again later")
		return
	}

	// genCtx lives independent of r.Context(); a watcher cancels it when the
	// browser goes away.
	genCtx, cancel := context.WithCancel(context.Background())
	g, err := s.gens.Start(conv.ID, user.ID,
		s.cfg.Limits.ConcurrentStreamsPerUser,
		s.cfg.Limits.ConcurrentStreamsTotal, cancel)
	if err != nil {
		cancel()
		switch {
		case errors.Is(err, errUserStreams):
			writeAPIError(w, r, http.StatusTooManyRequests, "too_many_streams",
				"you already have the maximum number of generations running")
		case errors.Is(err, errGlobalStreams):
			writeAPIError(w, r, http.StatusServiceUnavailable, "server_busy",
				"the server is at its generation capacity, try again shortly")
		default:
			writeAPIError(w, r, http.StatusConflict, "generation_in_progress",
				"a reply is already being generated in this conversation")
		}
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

	// From here on, errors travel as SSE events.
	hist, err := s.store.ContextHistory(genCtx, conv.ID, "")
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}

	toolSpecs := s.tools.Specs()
	sysPrompt := conv.SystemPrompt
	if len(toolSpecs) > 0 {
		sysPrompt += agent.ToolGuidance
		if _, hasCode := s.tools.Get("code_run"); hasCode {
			sysPrompt += agent.CodeRunGuidance
		}
	}
	msgs := agent.BuildContext(sysPrompt, s.historyToLLM(hist, s.tools.Enabled(), user.ID))

	am := &store.Message{ConversationID: conv.ID, Role: "assistant", Status: "streaming"}
	if err := s.store.AddMessage(genCtx, user.ID, am); err != nil {
		s.writeConvErr(w, r, err)
		return
	}

	conn, err := newSSE(w)
	if err != nil {
		s.finishMessage(genCtx, am.ID, "", "aborted", 0, 0, 0)
		return
	}
	if err := conn.Event("start", map[string]any{
		"generation_id":   g.ID,
		"message_id":      am.ID,
		"conversation_id": conv.ID,
	}); err != nil {
		conn.Close()
		s.finishMessage(genCtx, am.ID, "", "aborted", 0, 0, 0)
		return
	}

	// Auto-title quest (spec §12): name the conversation from this first
	// message while the reply streams. Runs on its own context so a browser
	// disconnect cannot strand the rename; the store write is guarded so a
	// user's manual rename always wins.
	titleDone := make(chan struct{})
	if autoTitle != "" && (conv.Title == "" || conv.Title == store.DefaultTitle) {
		go func() {
			defer close(titleDone)
			s.nameConversation(conv, autoTitle, conn)
		}()
	} else {
		close(titleDone)
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

	// --- the agentic loop (spec §6.1 steps 3–5) --------------------------------
	// The thinking choice is snapshotted here, once: every round below —
	// tool continuations included — sends this same fragment, and a
	// selector change during the stream only reaches the NEXT generation
	// (thinking-control-specification.md §5).
	thinkFrag := s.thinkingSnapshot(conv)
	maxRounds := s.cfg.Tools.Agent.MaxToolRounds
	if maxRounds < 0 {
		maxRounds = 0
	}
	var res agent.Result
	var lastMS int64 // model-round wall time behind the final message row
	round := 0
	for {
		roundTools := toolSpecs
		if round >= maxRounds {
			roundTools = nil // force a final, tool-less answer
		}
		roundStart := time.Now() // model time only: tool runs land on tool cards
		res = s.runModelRound(genCtx, conn, conv.Model, msgs, roundTools, thinkFrag)
		roundMS := time.Since(roundStart).Milliseconds()

		if res.Err != nil || res.Status == "aborted" {
			lastMS = roundMS
			s.finishMessage(genCtx, am.ID, res.Content, res.Status, res.Usage, res.Completion, roundMS)
			break
		}
		if len(res.ToolCalls) == 0 || round >= maxRounds {
			lastMS = roundMS
			s.finishMessage(genCtx, am.ID, res.Content, "complete", res.Usage, res.Completion, roundMS)
			break
		}

		// Persist the answered assistant turn, then execute every call
		// (the protocol requires one answer per call) and continue with a
		// fresh placeholder row. runToolCall owns the audit rows.
		s.finishMessage(genCtx, am.ID, res.Content, "complete", res.Usage, res.Completion, roundMS)

		msgs = append(msgs, llm.ChatMessage{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
		for _, call := range res.ToolCalls {
			result, attach := s.runToolCall(genCtx, g, conn, user.ID, conv.ID, am.ID, call)
			msgs = append(msgs, llm.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result})
			if attach != "" {
				if im, ok := s.attachImageView(user.ID, conv.ID, attach); ok {
					msgs = append(msgs, im)
				}
			}
		}

		round++
		am = &store.Message{ConversationID: conv.ID, Role: "assistant", Status: "streaming"}
		if err := s.store.AddMessage(genCtx, user.ID, am); err != nil {
			if genCtx.Err() != nil {
				break // abort/shutdown: not a storage failure, finalize below
			}
			s.logger.Error("placeholder for tool round failed", "error", err)
			break
		}
	}

	if res.Err != nil {
		// The sanitized client-facing error stays upstream_error (§9.12);
		// the log carries the deployment context a thinking failure needs:
		// endpoint, model and the choice this generation sent (§6).
		s.logger.Warn("generation failed", "error", res.Err, "user", user.ID,
			"conversation", conv.ID, "endpoint", s.cfg.LLM.Endpoint,
			"model", s.thinkingModel(conv.Model), "thinking", s.thinkingChoice(conv),
			"rid", RequestIDFrom(r.Context()))
		_ = conn.Event("error", map[string]any{
			"code":       "upstream_error",
			"message":    "the model endpoint failed; try again",
			"request_id": RequestIDFrom(r.Context()),
		})
	}
	// Give an in-flight title quest a short grace period so its event
	// arrives before EOF (the client stops reading at EOF). Beyond that the
	// rename still lands in the database; the browser sees it on next load.
	select {
	case <-titleDone:
	case <-time.After(3 * time.Second):
	}
	_ = conn.Event("done", map[string]any{
		"message_id":   am.ID,
		"status":       res.Status,
		"usage_tokens": res.Usage,
		// The numbers behind the UI's per-message tok/s readout: what the
		// model produced and the model-round wall time it took (tool runs
		// excluded — they show on their cards).
		"completion_tokens": res.Completion,
		"duration_ms":       lastMS,
	})
	conn.Close()
}

// runModelRound streams one model round; tool deltas surface as
// tool_start/tool_args SSE events with stable call ids (§6.1 taxonomy).
// thinking is the generation's snapshotted request fragment (nil = no
// thinking override).
func (s *Server) runModelRound(ctx context.Context, conn *sseConn, model string,
	msgs []llm.ChatMessage, toolSpecs []llm.ToolSpec, thinking map[string]any,
) agent.Result {
	type card struct {
		callID  string
		started bool
	}
	cards := map[int]*card{}

	toolDelta := func(idx int, d llm.ToolCallDelta) {
		c := cards[idx]
		if c == nil {
			c = &card{}
			cards[idx] = c
		}
		if d.ID != "" {
			c.callID = d.ID
		}
		if !c.started && (d.ID != "" || d.Name != "") {
			c.started = true
			if c.callID == "" {
				c.callID = id.New() // endpoints may omit ids; the UI needs one
			}
			_ = conn.Event("tool_start", map[string]any{"call_id": c.callID, "tool": d.Name})
		}
		if c.started && d.Arguments != "" {
			_ = conn.Event("tool_args", map[string]any{"call_id": c.callID, "t": d.Arguments})
		}
	}

	res := s.generator.Round(ctx, model, msgs, toolSpecs, thinking, func(kind, text string) error {
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
	}, toolDelta)

	// Endpoints occasionally omit ids; the protocol, the audit rows and the
	// UI cards all pair on one id, so mint the missing ones here — before
	// the fallback-card pass so the two never disagree.
	for i := range res.ToolCalls {
		if res.ToolCalls[i].ID == "" {
			res.ToolCalls[i].ID = id.New()
		}
	}
	// A model-produced call may outlive a partial start (e.g. name arrived
	// without any delta we saw); make sure a card exists either way.
	for _, call := range res.ToolCalls {
		found := false
		for _, c := range cards {
			if c.callID == call.ID {
				found = true
				break
			}
		}
		if !found {
			_ = conn.Event("tool_start", map[string]any{"call_id": call.ID, "tool": call.Function.Name})
		}
	}
	return res
}

// execTimeout bounds one tool execution. code_run gets the configured run
// budget plus a margin so the runner's own process-group kill (§9.9) fires
// first; this is the outer backstop.
func (s *Server) execTimeout(tool string) time.Duration {
	if tool == "code_run" && s.cfg.Scratchpad.CodeExec.Enabled {
		if d := s.cfg.Scratchpad.CodeExec.Timeout.D(); d > 0 {
			return d + 15*time.Second
		}
	}
	return 30 * time.Second
}

// runToolCall executes one proposed call: audit row, optional approval,
// execution, truncated result message, tool_end event. Returns the content
// to hand back to the model (always a valid tool message — denied and
// failed calls are reported as such, keeping the protocol intact) plus any
// image path the result asks the loop to attach (view_image; "" otherwise).
// call.ID is guaranteed non-empty by runModelRound.
func (s *Server) runToolCall(ctx context.Context, g *generation, conn *sseConn,
	userID, convID, assistantMsgID string, call llm.ChatToolCall,
) (string, string) {
	callID := call.ID
	name := call.Function.Name

	emitEnd := func(status, summary string, data any) {
		_ = conn.Event("tool_end", map[string]any{
			"call_id": callID, "tool": name, "status": status,
			"summary": summary, "data": data,
		})
	}

	tool, known := s.tools.Get(name)
	if !known {
		emitEnd("error", "unknown tool", nil)
		return `{"error":"unknown tool ` + jsonQuote(name) + `"}`, ""
	}

	status := "running"
	// code_run defaults to confirm; a remembered "always allow in this
	// conversation" (§9.9) skips the prompt for the rest of the chat.
	needApproval := s.tools.Policy(name) == tools.ApprovalConfirm &&
		!s.remembered.remembered(convID, name)
	if needApproval {
		status = "pending_approval"
	}
	tc := &store.ToolCall{
		MessageID: assistantMsgID, LLMCallID: callID, Tool: name,
		Arguments: call.Function.Arguments, Status: status,
	}
	if err := s.store.AddToolCall(ctx, tc); err != nil {
		// Audit is required (§7.1) but must not strand the conversation:
		// log and continue with a blank audit id; the model still gets an
		// answer for this call.
		s.logger.Error("tool audit insert failed", "error", err, "tool", name)
	}

	if status == "pending_approval" {
		decision := s.awaitApproval(ctx, g, conn, callID, name, call.Function.Arguments)
		if tc.ID != "" {
			newStatus, uctx := "approved", ctx
			if !decision.Approve {
				newStatus, uctx = "denied", context.Background() // abort may have killed ctx
			}
			_ = s.store.UpdateToolCallStatus(uctx, tc.ID, newStatus)
		}
		if decision.Approve && decision.Remember && name == "code_run" {
			s.remembered.remember(convID, name) // "always allow" for this chat only (§9.9)
		}
		if !decision.Approve {
			emitEnd("denied", "user denied this action", nil)
			// Deliberately no tool-result row: historyToLLM only replays
			// answered calls, so a denied call simply vanishes from later
			// prompts; the in-session msgs get the denial answer below.
			return `{"error":"the user denied this tool call"}`, ""
		}
	}

	started := time.Now()
	execCtx, execCancel := context.WithTimeout(ctx, s.execTimeout(name))
	tres, err := tool.Execute(execCtx, tools.Invocation{
		UserID: userID, Conversation: convID, LLMCallID: callID,
		Arguments: call.Function.Arguments, Scratchpad: s.cfg.Scratchpad.Root,
	})
	execCancel()
	dur := time.Since(started).Milliseconds()

	// The browser may have vanished mid-execution (genCtx cancelled): final
	// writes still happen on a detached context so history stays coherent.
	if err != nil {
		msg := tools.Elide(err.Error(), 200) // tool errors are pre-sanitized
		content := `{"error":` + jsonQuote(msg) + `}`
		if tc.ID != "" {
			_ = s.store.FinishToolCall(detachedCtx(), tc.ID, "error", content, dur)
		}
		s.appendToolResult(detachedCtx(), userID, convID, callID, content)
		emitEnd("error", msg, nil)
		return content, ""
	}

	content := tools.TruncateMiddle(tres.Content, toolResultBudgetRunes)
	if tc.ID != "" {
		_ = s.store.FinishToolCall(detachedCtx(), tc.ID, "ok", content, dur)
	}
	s.appendToolResult(detachedCtx(), userID, convID, callID, content)
	emitEnd("ok", tres.Summary, tres.Data)
	return content, tres.AttachPath
}

// detachedCtx gives a final persistence write 5 s of independence from the
// browser request.
func detachedCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() { // release once the write completes or times out
		<-ctx.Done()
		cancel()
	}()
	return ctx
}

// awaitApproval parks the loop on the user's decision; abort or the
// 10-minute timeout deny (spec §6.1 step 5). code_run approvals advertise
// rememberable so the UI can offer "always allow in this conversation".
func (s *Server) awaitApproval(ctx context.Context, g *generation, conn *sseConn,
	callID, name, args string,
) approvalDecision {
	apID, decCh, ok := s.gens.AddApproval(g.ID)
	if !ok {
		return approvalDecision{} // generation gone: fail closed
	}
	defer s.gens.RemoveApproval(g.ID, apID)
	_ = conn.Event("awaiting_approval", map[string]any{
		"approval_id": apID, "call_id": callID, "tool": name, "arguments": args,
		"rememberable": name == "code_run",
	})
	timer := time.NewTimer(approvalTimeout)
	defer timer.Stop()
	select {
	case decision := <-decCh:
		return decision
	case <-ctx.Done(): // browser abort or shutdown ⇒ deny
		return approvalDecision{}
	case <-timer.C:
		return approvalDecision{}
	}
}

// appendToolResult persists the role='tool' answer row (§4).
func (s *Server) appendToolResult(ctx context.Context, userID, convID, callID, content string) {
	m := &store.Message{
		ConversationID: convID,
		Role:           "tool",
		Content:        content,
		Status:         "complete",
		ToolCallID:     callID,
		TokenCount:     int64ptr(int64(agent.EstimateTokens(content))),
	}
	if err := s.store.AddMessage(ctx, userID, m); err != nil {
		s.logger.Error("tool result persist failed", "error", err, "call", callID)
	}
}

// viewTurnMarker prefixes the persisted note of every synthetic
// view_image turn. It is prose for the model — "this attachment is
// here because you asked" — nothing more: display filtering and replay
// embedding both follow the row's store.Message.Synthetic flag (§7.2),
// so typing this prefix changes nothing. Migration 0012 retro-flags the
// rows written before the flag existed by matching this prefix.
const viewTurnMarker = "[view_image attached "

// visionMaxBytes is llm.vision_max_image_bytes with the program default
// applied, mirroring config.setDefaults so struct-literal configs built
// outside config.Load (tests) still get a sane cap.
func (s *Server) visionMaxBytes() int64 {
	if b := int64(s.cfg.LLM.VisionMaxImageBytes); b > 0 {
		return b
	}
	return 4 << 20
}

// attachImageView realises a view_image call: tool answers are text, so
// the picture instead joins the conversation as a synthetic user turn —
// the manifest + inline-image pipeline, which only such marked turns
// ever ride. The row is persisted so historyToLLM rebuilds the bytes on
// every later replay: once the model has asked to see a picture, it
// keeps seeing it. Only the note is stored; the manifest is generated at
// replay, exactly like a real user message. Detached context, as for
// tool results: a browser vanishing must not strand the conversation
// with an "attached" answer and a hole where the picture should be. If
// the bytes vanished between validation and here, nothing embeds: false,
// logged, and this round proceeds picture-less.
func (s *Server) attachImageView(userID, convID, rel string) (llm.ChatMessage, bool) {
	att := store.Attachment{Path: rel}
	manifest, urls := attachmentManifest(s.cfg.Scratchpad.Root, userID,
		[]store.Attachment{att}, manifestOpts{visionOn: true, embed: true, maxBytes: s.visionMaxBytes()})
	if len(urls) == 0 {
		s.logger.Warn("view_image embedded nothing (file gone or unreadable?)",
			"path", rel, "user", userID)
		return llm.ChatMessage{}, false
	}
	note := viewTurnMarker + jsonQuote(att.Name()) +
		" so you can see it; it stays in this conversation for later turns]"
	ctx := detachedCtx()
	m := &store.Message{
		ConversationID: convID, Role: "user", Content: note,
		Status:     "complete",
		Synthetic:  true, // the browser hides it: the model's copy, not your words
		TokenCount: int64ptr(int64(agent.EstimateTokens(note + manifest))),
	}
	if err := s.store.AddMessage(ctx, userID, m); err != nil {
		s.logger.Error("view_image attach persist failed", "error", err, "path", rel)
		return llm.ChatMessage{}, false
	}
	if err := s.store.AttachFilesToMessage(ctx, m.ID, userID, []string{rel}); err != nil {
		s.logger.Error("view_image attach link failed", "error", err, "path", rel)
		return llm.ChatMessage{}, false
	}
	return llm.ChatMessage{Role: "user", Content: note + manifest, Images: urls}, true
}

// nameConversation asks the model for a topic title for conv, stores it if
// the conversation is still unnamed, and announces it over the live stream
// when possible. Purely best-effort: every failure only means the chat
// keeps its placeholder title and gets retried on the next message.
func (s *Server) nameConversation(conv *store.Conversation, firstUser string, conn *sseConn) {
	// 30 s: an endpoint that thinks despite the disable needs room to
	// reason out loud before the title (see Title's budget note).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	title, err := s.generator.Title(ctx, conv.Model, firstUser)
	if err != nil {
		s.logger.Info("auto-title skipped", "conversation", conv.ID, "error", err)
		return
	}
	ok, err := s.store.SetTitleIfDefault(ctx, conv.ID, title)
	if err != nil || !ok {
		if err != nil {
			s.logger.Warn("auto-title store write failed", "conversation", conv.ID, "error", err)
		}
		return
	}
	_ = conn.Event("title", map[string]any{"conversation_id": conv.ID, "title": title})
}

// finishMessage persists the final state on a short background context: the
// browser may already be gone (and genCtx already cancelled) when we save.
func (s *Server) finishMessage(_ context.Context, msgID, content, status string, usage, completion, durationMS int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FinalizeMessage(ctx, msgID, content, status, usage, completion, durationMS); err != nil {
		s.logger.Error("failed to persist message final state", "error", err, "message", msgID)
	}
}

// --- history replay ----------------------------------------------------------------

// historyToLLM maps persisted rows to OpenAI protocol messages with strict
// pairing: an assistant call replays only if its tool answer exists, and a
// tool row replays only if some replayed assistant call matches it. A crash
// between the two writes therefore degrades to plain text instead of
// poisoning every later request with an orphan protocol row. When tools are
// disabled the replay strips all protocol furniture — the endpoint is never
// told about tools in that mode. userID owns the conversation; it is the
// sandbox root owner for the attachment manifests.
func (s *Server) historyToLLM(hist []store.Message, toolsEnabled bool, userID string) []llm.ChatMessage {
	answered := map[string]bool{}
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	// Pass 1: which calls survive (answered AND from an assistant row).
	replayed := map[string]bool{}
	for _, m := range hist {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.Calls {
			if c.LLMCallID != "" && answered[c.LLMCallID] {
				replayed[c.LLMCallID] = true
			}
		}
	}
	// Pass 2: emit, pairing both directions on `replayed`.
	out := make([]llm.ChatMessage, 0, len(hist))
	for _, m := range hist {
		switch m.Role {
		case "tool":
			if !replayed[m.ToolCallID] {
				continue // orphan: its assistant call is gone or denied
			}
			out = append(out, llm.ChatMessage{Role: "tool", Content: m.Content, ToolCallID: m.ToolCallID})
		case "assistant":
			var replay []llm.ChatToolCall
			for _, c := range m.Calls {
				if replayed[c.LLMCallID] {
					replay = append(replay, llm.ChatToolCall{
						ID: c.LLMCallID, Type: "function",
						Function: llm.ChatToolCallFunc{Name: c.Tool, Arguments: c.Arguments},
					})
				}
			}
			out = append(out, llm.ChatMessage{Role: "assistant", Content: m.Content, ToolCalls: replay})
		default:
			content := m.Content
			var imgs []string
			if m.Role == "user" && len(m.Attachments) > 0 {
				// Uploads ride as manifest lines; only synthetic rows
				// (view_image turns, §7.2) carry their bytes, decided by
				// the DB flag, not by sniffing the note text.
				embed := s.cfg.LLM.Vision && m.Synthetic
				var manifest string
				manifest, imgs = attachmentManifest(s.cfg.Scratchpad.Root, userID,
					m.Attachments, manifestOpts{visionOn: s.cfg.LLM.Vision, embed: embed,
						maxBytes: s.visionMaxBytes()})
				content += manifest
			}
			out = append(out, llm.ChatMessage{Role: m.Role, Content: content, Images: imgs})
		}
	}
	if !toolsEnabled {
		// Strip protocol furniture when tools are off, but never lose the
		// assistant's actual words: only purely-protocol rows disappear.
		clean := make([]llm.ChatMessage, 0, len(out))
		for _, m := range out {
			if m.Role == "tool" {
				continue
			}
			if len(m.ToolCalls) > 0 {
				m.ToolCalls = nil
				if m.Content == "" {
					continue
				}
			}
			clean = append(clean, m)
		}
		return clean
	}
	return out
}

// jsonQuote marshals one string, for embedding sanitized error text.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func int64ptr(v int64) *int64 { return &v }
