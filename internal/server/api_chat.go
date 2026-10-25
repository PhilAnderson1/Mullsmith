package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gabkeep/internal/agent"
	"gabkeep/internal/id"
	"gabkeep/internal/llm"
	"gabkeep/internal/store"
	"gabkeep/internal/tools"
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

	// Persist the user turn first (spec §6.1 step 1), then stream. The user
	// text also seeds auto-titling when the conversation is still unnamed.
	s.streamAssistant(w, r, conv, content, func(bg context.Context) error {
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
	// so a 409 leaves the existing reply untouched. Regeneration never
	// retitles: the conversation already has its name.
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
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.ApprovalID == "" {
		writeAPIError(w, r, http.StatusBadRequest, "missing_approval_id", "approval_id is required")
		return
	}
	if !s.gens.ResolveApproval(r.PathValue("id"), body.ApprovalID, user.ID, body.Approve) {
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
	}
	msgs := agent.BuildContext(sysPrompt, historyToLLM(hist, s.tools.Enabled()),
		s.cfg.LLM.MaxContextTokens, s.cfg.LLM.MaxTokensResponse)

	am := &store.Message{ConversationID: conv.ID, Role: "assistant", Status: "streaming"}
	if err := s.store.AddMessage(genCtx, user.ID, am); err != nil {
		s.writeConvErr(w, r, err)
		return
	}

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
		conn.Close()
		s.finishMessage(genCtx, am.ID, "", "aborted", 0)
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
	maxRounds := s.cfg.Tools.Agent.MaxToolRounds
	if maxRounds < 0 {
		maxRounds = 0
	}
	var res agent.Result
	round := 0
	for {
		roundTools := toolSpecs
		if round >= maxRounds {
			roundTools = nil // force a final, tool-less answer
		}
		res = s.runModelRound(genCtx, conn, conv.Model, msgs, roundTools)

		if res.Err != nil || res.Status == "aborted" {
			s.finishMessage(genCtx, am.ID, res.Content, res.Status, res.Usage)
			break
		}
		if len(res.ToolCalls) == 0 || round >= maxRounds {
			s.finishMessage(genCtx, am.ID, res.Content, "complete", res.Usage)
			break
		}

		// Persist the answered assistant turn, then execute every call
		// (the protocol requires one answer per call) and continue with a
		// fresh placeholder row. runToolCall owns the audit rows.
		s.finishMessage(genCtx, am.ID, res.Content, "complete", res.Usage)

		msgs = append(msgs, llm.ChatMessage{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
		for _, call := range res.ToolCalls {
			result := s.runToolCall(genCtx, g, conn, user.ID, conv.ID, am.ID, call)
			msgs = append(msgs, llm.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result})
		}

		round++
		am = &store.Message{ConversationID: conv.ID, Role: "assistant", Status: "streaming"}
		if err := s.store.AddMessage(genCtx, user.ID, am); err != nil {
			s.logger.Error("placeholder for tool round failed", "error", err)
			break
		}
	}

	if res.Err != nil {
		s.logger.Warn("generation failed", "error", res.Err, "user", user.ID,
			"conversation", conv.ID, "rid", RequestIDFrom(r.Context()))
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
	})
	conn.Close()
}

// runModelRound streams one model round; tool deltas surface as
// tool_start/tool_args SSE events with stable call ids (§6.1 taxonomy).
func (s *Server) runModelRound(ctx context.Context, conn *sseConn, model string,
	msgs []llm.ChatMessage, toolSpecs []llm.ToolSpec,
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

	res := s.generator.Round(ctx, model, msgs, toolSpecs, func(kind, text string) error {
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

// runToolCall executes one proposed call: audit row, optional approval,
// execution, truncated result message, tool_end event. Returns the content
// to hand back to the model (always a valid tool message — denied and
// failed calls are reported as such, keeping the protocol intact).
// call.ID is guaranteed non-empty by runModelRound.
func (s *Server) runToolCall(ctx context.Context, g *generation, conn *sseConn,
	userID, convID, assistantMsgID string, call llm.ChatToolCall,
) string {
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
		return `{"error":"unknown tool ` + jsonQuote(name) + `"}`
	}

	status := "running"
	if s.tools.Policy(name) == tools.ApprovalConfirm {
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
		approved := s.awaitApproval(ctx, g, conn, callID, name, call.Function.Arguments)
		if tc.ID != "" {
			newStatus, uctx := "approved", ctx
			if !approved {
				newStatus, uctx = "denied", context.Background() // abort may have killed ctx
			}
			_ = s.store.UpdateToolCallStatus(uctx, tc.ID, newStatus)
		}
		if !approved {
			emitEnd("denied", "user denied this action", nil)
			// Deliberately no tool-result row: historyToLLM only replays
			// answered calls, so a denied call simply vanishes from later
			// prompts; the in-session msgs get the denial answer below.
			return `{"error":"the user denied this tool call"}`
		}
	}

	started := time.Now()
	execCtx, execCancel := context.WithTimeout(ctx, 30*time.Second)
	tres, err := tool.Execute(execCtx, tools.Invocation{
		UserID: userID, LLMCallID: callID, Arguments: call.Function.Arguments,
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
		return content
	}

	content := tools.TruncateMiddle(tres.Content, toolResultBudgetRunes)
	if tc.ID != "" {
		_ = s.store.FinishToolCall(detachedCtx(), tc.ID, "ok", content, dur)
	}
	s.appendToolResult(detachedCtx(), userID, convID, callID, content)
	emitEnd("ok", tres.Summary, tres.Data)
	return content
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
// 10-minute timeout deny (spec §6.1 step 5).
func (s *Server) awaitApproval(ctx context.Context, g *generation, conn *sseConn,
	callID, name, args string,
) bool {
	apID, decCh, ok := s.gens.AddApproval(g.ID)
	if !ok {
		return false // generation gone: fail closed
	}
	defer s.gens.RemoveApproval(g.ID, apID)
	_ = conn.Event("awaiting_approval", map[string]any{
		"approval_id": apID, "call_id": callID, "tool": name, "arguments": args,
	})
	timer := time.NewTimer(approvalTimeout)
	defer timer.Stop()
	select {
	case approve := <-decCh:
		return approve
	case <-ctx.Done(): // browser abort or shutdown ⇒ deny
		return false
	case <-timer.C:
		return false
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

// nameConversation asks the model for a topic title for conv, stores it if
// the conversation is still unnamed, and announces it over the live stream
// when possible. Purely best-effort: every failure only means the chat
// keeps its placeholder title and gets retried on the next message.
func (s *Server) nameConversation(conv *store.Conversation, firstUser string, conn *sseConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
func (s *Server) finishMessage(_ context.Context, msgID, content, status string, usage int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FinalizeMessage(ctx, msgID, content, status, usage); err != nil {
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
// told about tools in that mode.
func historyToLLM(hist []store.Message, toolsEnabled bool) []llm.ChatMessage {
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
			out = append(out, llm.ChatMessage{Role: m.Role, Content: m.Content})
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
