package chat

import (
	"context"
	"errors"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
)

func (s *Service) runModelRound(ctx context.Context, conn sink, model string,
	msgs []llm.ChatMessage, toolSpecs []llm.ToolSpec, thinking map[string]any, msgID string,
) agent.Result {
	return s.generator.Round(ctx, model, msgs, toolSpecs, thinking, func(kind, text string) error {
		if kind == agent.EventReasoning && !s.cfg.LLM.ShowReasoning {
			return nil
		}
		if err := conn.Send(TextDelta{Reasoning: kind == agent.EventReasoning, Text: text}); err != nil {
			return agent.ErrClientGone
		}
		return nil
	}, func(event agent.ToolEvent) {
		if event.Kind == "tool_start" {
			_ = conn.Send(ToolStarted{CallID: event.CallID, MessageID: msgID, Tool: event.Tool})
		} else {
			_ = conn.Send(ToolArguments{CallID: event.CallID, MessageID: msgID, Text: event.Arguments})
		}
	})
}

// execTimeout bounds one tool execution. code_run gets the configured run
// budget plus a margin so the runner's own process-group kill (§9.9) fires
// first; this is the outer backstop. Web tools similarly get their configured
// client timeout plus a five-second margin, rather than the generic 30 seconds.
func (s *Service) execTimeout(tool string) time.Duration {
	switch tool {
	case "web_fetch":
		return s.cfg.Tools.WebFetch.Timeout.D() + 5*time.Second
	case "web_download":
		return s.cfg.Tools.WebDownload.Timeout.D() + 5*time.Second
	}
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
func (s *Service) runToolCall(ctx context.Context, g *Generation, conn sink,
	userID, convID, assistantMsgID string, call llm.ChatToolCall, visionOn bool,
) (string, string, error) {
	callID := call.ID
	name := call.Function.Name
	// persistErr collects failed REQUIRED writes (audit finalisation,
	// tool-result row): the transcript the loop continues from would not
	// replay from history, so the orchestrator ends the generation as a
	// storage error instead of announcing success (review2 finding 2).
	var persistErr error

	// One clamped card, emitted live AND stored (C7): the identical
	// {summary,data,truncated} goes into the tool_end event and into
	// tool_calls.display, so no card can be built one way live and another
	// way from history — and neither view re-parses the middle-elided
	// model-facing text. Over the display budget the DATA drops and the
	// SUMMARY stays with a flag; a JSON structure is never cut in half.
	card := tools.ClampCard
	emitEnd := func(status string, c tools.Card) {
		_ = conn.Send(ToolFinished{CallID: callID, MessageID: assistantMsgID, Tool: name, Status: status, Card: c})
	}

	tool, known := s.Tools.Get(name)
	if !known {
		content, err := s.rejectToolCall(ctx, conn, userID, convID, assistantMsgID, g.ID, call, "unknown tool: "+name)
		return content, "", err
	}
	// Belt to the spec filter: a model whose entry left vision off was
	// never offered view_image, so a call for it can only be a
	// hallucination — answered as such, never executed (§7.2).
	if name == "view_image" && !visionOn {
		content, err := s.rejectToolCall(ctx, conn, userID, convID, assistantMsgID, g.ID, call, "this model cannot see images")
		return content, "", err
	}

	status := "running"
	// code_run defaults to confirm; a remembered "always allow in this
	// conversation" (§9.9) skips the prompt for the rest of the chat.
	needApproval := s.Tools.Policy(name) == tools.ApprovalConfirm &&
		!s.Approvals.Remembered(convID, name)
	if needApproval {
		status = "pending_approval"
	}
	tc := &store.ToolCall{
		MessageID: assistantMsgID, LLMCallID: callID, Tool: name,
		Arguments: call.Function.Arguments, Status: status,
	}
	if err := s.store.AddToolCall(ctx, tc); err != nil {
		// Audit is a PREREQUISITE for execution (finding 4): an unaudited
		// side effect is invisible to review and unreplayable, which is
		// worse than a refusal — the old behaviour logged and ran it. The
		// model gets an ordinary tool error, so the protocol stays intact.
		s.logger.Error("tool audit insert failed; call not executed", "error", err, "tool", name)
		msg := "tool call could not be audited; it was not executed"
		emitEnd("error", card(msg, nil))
		// C3: this failure IS a persistence error and travels as one. The
		// refusal answer lives only in this session's memory — no row
		// makes it durable — so continuing (or answering) over it would
		// replay as silence; the loop must end, not sample later calls.
		return `{"error":` + jsonQuote(msg) + `}`, "", err
	}

	if status == "pending_approval" {
		decision := s.awaitApproval(ctx, g, conn, callID, name, call.Function.Arguments, assistantMsgID)
		newStatus, uctx := "approved", ctx
		if !decision.Approve {
			newStatus, uctx = "denied", context.Background() // abort may have killed ctx
		}
		// The approval verdict IS the audit trail for a side-effecting
		// run (review2 finding 2), and C3 makes it a PREREQUISITE: an
		// approved call whose verdict could not be recorded is neither
		// executed nor remembered — the durable record first, the side
		// effect after.
		// The denial's card is durable too (C7): "user denied this action"
		// was live-only, so a reload showed a bare state label. The clamped
		// card below is what both the stored row and the event carry.
		verdict := card("user denied this action", nil)
		verdictDisplay := verdict.JSON()
		if uerr := s.store.UpdateToolCallStatus(uctx, tc.ID, newStatus, verdictDisplay); uerr != nil {
			s.logger.Error("tool approval audit update failed", "error", uerr, "tool", name)
			persistErr = uerr
			if decision.Approve {
				msg := "tool call approval could not be recorded; it was not executed"
				emitEnd("error", card(msg, nil))
				return `{"error":` + jsonQuote(msg) + `}`, "", persistErr
			}
		}
		if decision.Approve && decision.Remember && name == "code_run" {
			s.Approvals.Remember(convID, name) // only after the approval is durable (C3, §9.9)
		}
		if !decision.Approve {
			emitEnd("denied", verdict)
			// Deliberately no tool-result row: historyToLLM only replays
			// answered calls, so a denied call simply vanishes from later
			// prompts; the in-session msgs get the denial answer below.
			return `{"error":"the user denied this tool call"}`, "", persistErr
		}
	}

	started := time.Now()
	execCtx, execCancel := context.WithTimeout(ctx, s.execTimeout(name))
	tres, err := tool.Execute(execCtx, tools.Invocation{
		UserID: userID, Arguments: call.Function.Arguments, Scratchpad: s.cfg.Scratchpad.Root,
	})
	execCancel()
	dur := time.Since(started).Milliseconds()

	// The browser may have vanished mid-execution (genCtx cancelled): final
	// writes still happen on a detached context so history stays coherent.
	if err != nil {
		msg := tools.Elide(err.Error(), 200) // tool errors are pre-sanitized
		content := `{"error":` + jsonQuote(msg) + `}`
		c := card(msg, nil)
		if ferr := s.finalizeToolCall(tc.ID, "error", content, c.JSON(),
			dur, userID, convID, callID, g.ID); ferr != nil {
			persistErr = ferr
		}
		emitEnd("error", c)
		return content, "", persistErr
	}

	content := tools.TruncateMiddle(tres.Content, ToolResultBudgetRunes)
	c := card(tres.Summary, tres.Data)
	if ferr := s.finalizeToolCall(tc.ID, "ok", content, c.JSON(),
		dur, userID, convID, callID, g.ID); ferr != nil {
		persistErr = ferr
	}
	emitEnd("ok", c)
	return content, tres.AttachPath, persistErr
}

// rejectToolCall records a refused proposal and its model-facing answer
// without looking up, approving or executing the tool. These error results
// must replay from history just like the successful calls before the cap.
func (s *Service) rejectToolCall(ctx context.Context, conn sink,
	userID, convID, messageID, genID string, call llm.ChatToolCall, reason string,
) (string, error) {
	content := `{"error":` + jsonQuote(reason) + `}`
	card := tools.ClampCard(reason, nil)
	tc := &store.ToolCall{
		MessageID: messageID, LLMCallID: call.ID, Tool: call.Function.Name,
		Arguments: call.Function.Arguments, Status: "error",
	}
	err := s.store.AddToolCall(ctx, tc)
	if err != nil {
		s.logger.Error("refused tool audit insert failed", "error", err, "call", call.ID)
	} else {
		err = s.finalizeToolCall(tc.ID, "error", content, card.JSON(), 0, userID, convID, call.ID, genID)
	}
	_ = conn.Send(ToolFinished{CallID: call.ID, MessageID: messageID, Tool: call.Function.Name, Status: "error", Card: card})
	return content, err
}

// finalizeToolCall performs the two REQUIRED writes that close one tool
// call: the audit row's final state and the role='tool' answer row. Both
// errors surface — a call whose answer never landed must not be replayed
// from memory as though it had (review2 finding 2).
func (s *Service) finalizeToolCall(tcID, status, content, display string, dur int64,
	userID, convID, callID, genID string,
) error {
	ctx, cancel := detachedCtx()
	defer cancel()
	if err := s.store.FinishToolCall(ctx, tcID, status, content, display, dur); err != nil {
		s.logger.Error("tool audit finalize failed", "error", err, "call", callID)
		if errors.Is(err, store.ErrRowGone) {
			// The audit row went with its conversation (C6): the answer
			// has nothing to be recorded against, and the caller reads
			// this as cancellation.
			return err
		}
		// The result row still goes in: an answer the model acted on
		// should at least be replayable even if its audit tail is thin.
		_ = s.appendToolResult(ctx, userID, convID, callID, genID, content)
		return err
	}
	return s.appendToolResult(ctx, userID, convID, callID, genID, content)
}

// detachedCtx gives a final persistence write 5 s of independence from the
// browser request. The returned cancel releases it as soon as the write
// returns (C6) — no goroutine spawned per write just to wait out the
// timeout.
func detachedCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// isCancellation classifies the write failures that deletion or abort
// explain (C6): the subject row is gone because the user deleted the
// conversation (or account), or the generation's own context is done.
// Neither is a storage failure to announce — the operation that removed
// the subject is the reason the write cannot land, and the honest terminal
// outcome is the cancellation already recorded, not a storage error.
func isCancellation(err error, genCtx context.Context) bool {
	return errors.Is(err, store.ErrRowGone) ||
		errors.Is(err, store.ErrConversationNotFound) ||
		genCtx.Err() != nil
}

// awaitApproval parks the loop on the user's decision; abort or the
// 10-minute timeout deny (spec §6.1 step 5). code_run approvals advertise
// rememberable so the UI can offer "always allow in this conversation".
func (s *Service) awaitApproval(ctx context.Context, g *Generation, conn sink,
	callID, name, args, msgID string,
) ApprovalDecision {
	apID, decCh, ok := s.Generations.AddApproval(g.ID)
	if !ok {
		return ApprovalDecision{} // generation gone: fail closed
	}
	defer s.Generations.RemoveApproval(g.ID, apID)
	_ = conn.Send(ApprovalRequested{ApprovalID: apID, CallID: callID, MessageID: msgID, Tool: name, Arguments: args, Rememberable: name == "code_run"})
	timer := time.NewTimer(ApprovalTimeout)
	defer timer.Stop()
	select {
	case decision := <-decCh:
		return decision
	case <-ctx.Done(): // browser abort or shutdown ⇒ deny
		return ApprovalDecision{}
	case <-timer.C:
		return ApprovalDecision{}
	}
}

// appendToolResult persists the role='tool' answer row (§4). The error
// surfaces: replaying a result that never landed is exactly the live-vs-
// history divergence review2 finding 2 names.
func (s *Service) appendToolResult(ctx context.Context, userID, convID, callID, genID, content string) error {
	m := &store.Message{
		ConversationID: convID,
		Role:           "tool",
		GenerationID:   genID,
		Content:        content,
		Status:         "complete",
		ToolCallID:     callID,
		TokenCount:     int64ptr(int64(agent.EstimateTokens(content))),
	}
	if err := s.store.AddMessage(ctx, userID, m); err != nil {
		s.logger.Error("tool result persist failed", "error", err, "call", callID)
		return err
	}
	return nil
}

// ViewTurnMarker prefixes the persisted note of every synthetic
// view_image turn. It is prose for the model — "this attachment is
// here because you asked" — nothing more: display filtering and replay
// embedding both follow the row's store.Message.Synthetic flag (§7.2),
// so typing this prefix changes nothing.
const ViewTurnMarker = "[view_image attached "

func (s *Service) visionMaxBytes() int64 { return int64(s.cfg.LLM.VisionMaxImageBytes) }

// attachImageView realises a view_image call: tool answers are text, so
// the picture instead joins the conversation as a synthetic user turn —
// the manifest + inline-image pipeline, which only such marked turns
// ever ride. The row is persisted so historyToLLM rebuilds the bytes on
// every later replay: once the model has asked to see a picture, it
// keeps seeing it. Only the note is stored; the manifest is generated at
// replay, exactly like a real user message. Detached context, as for
// tool results: a browser vanishing must not strand the conversation
// with an "attached" answer and a hole where the picture should be. If
// the bytes vanished between validation and here, nothing embeds: false
// with no error, logged, and this round proceeds picture-less. A write
// that refuses the row is a typed persistence error (C3): the model was
// promised a picture, and continuing as though nothing happened would
// diverge live memory from replayable history.
func (s *Service) attachImageView(userID, convID, rel, genID string) (llm.ChatMessage, bool, error) {
	att := store.Attachment{Path: rel}
	manifest, urls := attachmentManifest(s.cfg.Scratchpad.Root, userID,
		[]store.Attachment{att}, manifestOpts{visionOn: true, embed: true, maxBytes: s.visionMaxBytes()})
	if len(urls) == 0 {
		s.logger.Warn("view_image embedded nothing (file gone or unreadable?)",
			"path", rel, "user", userID)
		return llm.ChatMessage{}, false, nil
	}
	note := ViewTurnMarker + jsonQuote(att.Name()) +
		" so you can see it; it stays in this conversation for later turns]"
	ctx, cancel := detachedCtx()
	defer cancel()
	m := &store.Message{
		ConversationID: convID, Role: "user", Content: note,
		Status:     "complete",
		Synthetic:  true, // the browser hides it: the model's copy, not your words
		TokenCount: int64ptr(int64(agent.EstimateTokens(note + manifest))),
		// Part of THIS turn (C7), so retiring the turn retires the picture
		// the model was shown along with the rounds that asked for it.
		GenerationID: genID,
	}
	// ONE transaction (finding 4): the note row and its file link land
	// together — a half-attached synthetic turn would replay as prose
	// promising a picture that never embeds.
	if err := s.store.AddMessageWithFiles(ctx, userID, m, []string{rel}); err != nil {
		s.logger.Error("view_image attach persist failed", "error", err, "path", rel)
		return llm.ChatMessage{}, false, err
	}
	return llm.ChatMessage{Role: "user", Content: note + manifest, Images: urls}, true, nil
}

// nameConversation asks the model for a topic title for conv, stores it if
// the conversation is still unnamed, and announces it over the live stream
// when possible. Purely best-effort: every failure only means the chat
// keeps its placeholder title and gets retried on the next message.
func (s *Service) nameConversation(parent context.Context, conv *store.Conversation, firstUser string, conn sink) {
	// 30 s: an endpoint that thinks despite the disable needs room to
	// reason out loud before the title (see Title's budget note).
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	// The title quest runs as quietly as the catalogues allow: this
	// model's off option, else its lightest rung (§12). The
	// conversation's own thinking choice never applies here — a title
	// is not a reply. Nil when no selector covers the model, leaving
	// llm.Complete's fallback hint.
	quiet := s.cfg.Thinking.QuietFragment(s.Model(conv.Model))
	title, err := s.generator.Title(ctx, s.Model(conv.Model), firstUser, quiet)
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
	_ = conn.Send(TitleChanged{ConversationID: conv.ID, Title: title})
}

// finishMessage persists the final state on a short background context: the
// browser may already be gone (and genCtx already cancelled) when we save.
// The error reaches the orchestrator (finding 4) so terminal events can
// distinguish "saved" from "announced but not durable".
func (s *Service) finishMessage(msgID, content, status, note string, usage, completion, durationMS int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FinalizeMessage(ctx, msgID, content, status, note, usage, completion, durationMS); err != nil {
		s.logger.Error("failed to persist message final state", "error", err, "message", msgID)
		return err
	}
	return nil
}
