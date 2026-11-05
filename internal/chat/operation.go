package chat

import (
	"errors"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
)

func (op *Operation) Run(emit Emit) {
	defer op.Close()
	s := op.service
	genCtx := op.ctx
	g := op.generation
	conv := op.conv
	am := op.draft
	cmd := op.cmd
	userID := op.userID
	msgs, toolSpecs, visionOn := op.messages, op.specs, op.vision
	conn := sink{emit: emit}
	// `start` is the acceptance signal AND round 1's identity (C7): its
	// message_id is the row this round streams into and the bubble key the
	// client keeps. Later rounds are announced by `round` events, so the
	// client renders the same row boundaries a reload will show.
	if err := conn.Send(RoundStarted{Initial: true, GenerationID: g.ID, MessageID: am.ID, ConversationID: conv.ID, Round: 0}); err != nil {
		_ = s.finishMessage(am.ID, "", "aborted", "", 0, 0, 0) // swept if it fails too
		return
	}

	// Auto-title quest (spec §12): name the conversation from this first
	// message BEFORE the agentic loop starts. Sequencing it first is what
	// makes it work on serialised endpoints: it claims the single model
	// slot while the browser is showing its thinking spinner, instead of
	// queueing behind the whole reply — where queue time would burn its
	// budget and any long reply would cancel its own title forever. On
	// parallel endpoints the ordering costs nothing visible (its own
	// round-trip lands before the first token). Its timeout is bounded by
	// generation cancellation, and the guarded store write lets a manual
	// rename win.
	if cmd.AutoTitle != "" && (conv.Title == "" || conv.Title == store.DefaultTitle) {
		s.nameConversation(genCtx, conv, cmd.AutoTitle, conn)
	}

	// --- the agentic loop (spec §6.1 steps 3–5) --------------------------------
	// The thinking choice is snapshotted here, once: every round below —
	// tool continuations included — sends this same fragment, and a
	// selector change during the stream only reaches the NEXT generation
	// (thinking-control-specification.md §5).
	thinkFrag := s.ThinkingSnapshot(conv)
	maxRounds := s.cfg.Tools.Agent.MaxToolRounds
	if maxRounds < 0 {
		maxRounds = 0
	}
	maxCalls := s.cfg.Tools.Agent.MaxToolCallsPerTurn
	if maxCalls < 0 {
		maxCalls = 0
	}
	toolCallCount := 0
	limitFeedbackSent := false
	var res agent.Result
	// storeErr records a persistence failure the terminal events must
	// surface (finding 4): success may only be announced when the final
	// state is actually durable — a storage error is not an endpoint error.
	var storeErr error
	// midFull marks the loop stopping on its own overflow recheck
	// (finding 7) rather than on model or storage trouble.
	var midFull bool
	// cancelledOut marks the stop that PREVENTED the final answer (review3
	// issue 3): a completed tool-proposing round is not a completed
	// generation, so "was the user's request finished" is recorded where
	// cancellation ends the loop — never inferred from whether the last
	// round's status string happens to be empty. A disconnect AFTER a
	// persisted final answer sets nothing and relabels nothing.
	var cancelledOut bool
	var limitStopped bool
	// rounds records each round's outcome IN ROW ORDER (C7). Every entry
	// exists because its row was really finalised (C6's rule, unchanged): a
	// write the database refused is not a round the client is told about.
	rounds := []RoundOutcome{}
	// finRound finalises one round row and records what was persisted, so
	// the terminal event describes the writes rather than intentions.
	finRound := func(msgID, content, status, note string, usage, completion, durMS int64) error {
		ferr := s.finishMessage(msgID, content, status, note, usage, completion, durMS)
		if ferr == nil {
			rounds = append(rounds, RoundOutcome{
				MsgID: msgID, Status: status, Note: note, Completion: completion, DurationMS: durMS,
			})
		}
		return ferr
	}
	// noteStranded attaches a terminal reason to the LAST durable row when
	// the failure has no row of its own: the last round saved cleanly and
	// THEN the loop died (failed next-round placeholder, unwritable tool
	// result, window filled mid-loop). Live, `done` explained it; on reload
	// nothing did — the transcript read as a finished answer. Written
	// detached, because this is exactly the final write a disconnect must
	// not lose.
	noteStranded := func(text string) {
		if len(rounds) == 0 {
			return // nothing durable to annotate; `done` still tells the truth
		}
		ctx, cancel := detachedCtx()
		defer cancel()
		if err := s.store.SetStatusNote(ctx, rounds[len(rounds)-1].MsgID, text); err != nil {
			if !isCancellation(err, genCtx) {
				s.logger.Error("terminal status note failed", "error", err,
					"conversation", conv.ID, "rid", cmd.RequestID)
			}
			return
		}
		rounds[len(rounds)-1].Note = text
	}
	round := 0
	for {
		if round > 0 && s.continuationFull(conv, msgs, toolSpecs) {
			// Every tool result so far rides msgs; a continuation the
			// admission gate never saw can now cross the line itself.
			// Refuse the NEXT round before paying for it: the transcript
			// stays durable, this still-empty draft row is the only
			// casualty.
			midFull = true
			if ferr := finRound(am.ID, "", "aborted", "", 0, 0, 0); ferr != nil {
				storeErr = ferr
			}
			break
		}
		roundStart := time.Now() // model time only: tool runs land on tool cards
		res = s.runModelRound(genCtx, conn, s.Model(conv.Model), msgs, toolSpecs,
			thinkFrag)
		roundMS := time.Since(roundStart).Milliseconds()

		if res.Status != "complete" {
			if ferr := finRound(am.ID, res.Content, res.Status, res.Note, res.Usage, res.Completion, roundMS); ferr != nil {
				storeErr = ferr
			}
			break
		}
		if len(res.ToolCalls) == 0 {
			if ferr := finRound(am.ID, res.Content, "complete", "", res.Usage, res.Completion, roundMS); ferr != nil {
				storeErr = ferr
			}
			break
		}

		// Persist the answered assistant turn, then execute every call
		// (the protocol requires one answer per call) and continue with a
		// fresh placeholder row. runToolCall owns the audit rows.
		if ferr := finRound(am.ID, res.Content, "complete", "", res.Usage, res.Completion, roundMS); ferr != nil {
			if isCancellation(ferr, genCtx) {
				cancelledOut = true // deletion/abort ended it, not storage
			} else {
				storeErr = ferr
			}
			// C3: the proposing turn is a PREREQUISITE of every side
			// effect it suggested. If the answer cannot become replayable,
			// the calls it proposed must not run — the old code recorded
			// the failure and executed anyway, and the terminal error
			// event came far too late to prevent the side effect.
			break
		}

		msgs = append(msgs, llm.ChatMessage{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
		var perr error
		limitedThisRound := false
		for _, call := range res.ToolCalls {
			if genCtx.Err() != nil {
				// The stop landed between calls (review3 issue 3): the
				// remaining side effects of this round do NOT happen, and
				// the generation ends as cancelled — not as the "complete"
				// the proposing round's status would have announced.
				cancelledOut = true
				break
			}
			var result, attach string
			var terr error
			// The model round has finished, so this call's name and arguments
			// are complete. Announce it only now, immediately before its own
			// approval/execution lifecycle; calls and outputs remain adjacent.
			s.announceToolCall(conn, userID, am.ID, call)
			// Rounds bound model continuations; calls bound the aggregate
			// filesystem/network/database work one accepted user turn can
			// induce. Count every proposal, including unknown, malformed,
			// denied and already-over-limit calls, so no cheaper call class
			// becomes an accounting bypass.
			toolCallCount++
			if round >= maxRounds || toolCallCount > maxCalls {
				limitedThisRound = true
				result, terr = s.rejectToolCall(genCtx, conn, userID, conv.ID, am.ID, g.ID, call, "tool call limit reached")
			} else if call.Function.Name == tools.ViewImageName && visionOn &&
				requestImageCount(msgs) >= s.visionMaxImages() {
				result, terr = s.rejectToolCall(genCtx, conn, userID, conv.ID, am.ID, g.ID,
					call, "image limit reached for this request; start a new chat to view another image")
			} else {
				result, attach, terr = s.runToolCall(genCtx, g, conn, userID, conv.ID, am.ID, call, visionOn)
			}
			perr = terr
			msgs = append(msgs, llm.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result})
			if attach != "" {
				im, ok, aerr := s.attachImageView(userID, conv.ID, attach, g.ID)
				if aerr != nil {
					// C3: the picture the model was promised must be
					// durable before the continuation speaks it — a
					// synthetic-turn write failure is a persistence
					// error, not a picture-less shrug.
					perr = aerr
				} else if ok {
					msgs = append(msgs, im)
				}
			}
			if perr != nil {
				// The transcript is NOT durable (review2 finding 2): the
				// model saw a result later history cannot replay. Stop
				// here — and stop MEANS stop (C3): no later call of this
				// turn runs past a recorded failure, and the loop below
				// will not start another round on memory alone.
				// Deletion or abort explains the write: that is
				// cancellation, not a storage failure to announce.
				if isCancellation(perr, genCtx) {
					cancelledOut = true
				} else {
					storeErr = perr
				}
				break
			}
		}
		if cancelledOut || storeErr != nil || perr != nil {
			break
		}
		// The first round containing limit refusals feeds ordinary tool
		// errors back to the model, with the definitions intact. It chooses
		// its own next response. A repeated tool request is recorded and
		// refused too, then ends the generation rather than starting an
		// unbounded error loop. This applies equally to the round and total-
		// call ceilings.
		if limitedThisRound {
			if limitFeedbackSent {
				limitStopped = true
				break
			}
			limitFeedbackSent = true
		}

		round++
		// The next draft row advances the generation's identity ONLY when
		// the insert really landed (C6): a failed placeholder must never
		// leave the terminal event naming a row that does not exist. am
		// keeps the last row that is durably there — already finalised
		// above — and done describes THAT state, nothing imagined.
		next := &store.Message{ConversationID: conv.ID, Role: "assistant",
			Status: "streaming", GenerationID: g.ID}
		if err := s.store.AddMessage(genCtx, userID, next); err != nil {
			if isCancellation(err, genCtx) {
				cancelledOut = true // stopped before the next round, C6
			} else {
				s.logger.Error("placeholder for tool round failed", "error", err)
				// The loop ends WITHOUT an answer (finding 4): there is no
				// row to stream the next round into, and done must not
				// claim a success the database never got.
				storeErr = err
			}
			break
		}
		am = next
		// The round's identity is announced with its row (C7): the client
		// opens a fresh bubble keyed by this message id, so the live view's
		// bubble boundaries are the stored row boundaries. Emitted only
		// after the insert landed — a round that was never created is never
		// named, and a client that misses the event has lost a round that
		// does not exist rather than inventing one.
		if err := conn.Send(RoundStarted{GenerationID: g.ID, MessageID: am.ID, Round: round + 1}); err != nil {
			// A dead link mid-loop is the ordinary disconnect path: the next
			// round's writes still happen (the transcript stays durable) and
			// the round's own streaming failure ends the loop below.
			s.logger.Debug("round event write failed", "error", err,
				"conversation", conv.ID)
		}
	}

	if res.Err != nil {
		// The sanitized client-facing error stays upstream_error (§9.12);
		// the log carries the deployment context a thinking failure needs:
		// endpoint, model and the choice this generation sent (§6).
		s.logger.Warn("generation failed", "error", res.Err, "user", userID,
			"conversation", conv.ID, "endpoint", s.cfg.LLM.Endpoint,
			"model", s.Model(conv.Model), "thinking", s.ThinkingChoice(conv),
			"rid", cmd.RequestID)
		_ = conn.Send(Failed{Code: "upstream_error", Message: llm.EndpointErrorMessage(res.Err), RequestID: cmd.RequestID})
	}
	// Terminal events describe DURABLE outcomes (finding 4): the client
	// must be able to tell an endpoint failure from a storage failure from
	// the conversation hitting its window mid-flight — each needs a
	// different next move (retry / reload / new chat).
	if midFull {
		_ = conn.Send(Failed{Code: "context_full", Message: "this conversation reached its context window during tool rounds — start a new chat"})
	}
	if storeErr != nil {
		s.logger.Error("generation persistence failed", "error", storeErr, "user", userID,
			"conversation", conv.ID, "rid", cmd.RequestID)
		if errors.Is(storeErr, store.ErrMessageLimit) {
			_ = conn.Send(Failed{Code: "message_limit", Message: store.MessageLimitNotice})
		} else {
			_ = conn.Send(Failed{Code: "storage_error", Message: "the reply could not be fully saved; reload this conversation"})
		}
	}
	if limitStopped {
		_ = conn.Send(Failed{Code: "tool_call_limit", Message: "tool call limit reached"})
	}
	// One terminal-outcome calculation with explicit precedence (C6, and
	// review3 issue 3): a context refusal or a storage failure describe a
	// state the client must act on or reload; a cancellation that STOPPED
	// the operation (cancelledOut) is the outcome even when the last
	// model round completed — a round that finished proposing tools is not
	// a generation that finished answering; else the round's own verdict
	// stands. A disconnect after a durably completed answer relabels
	// nothing: only the recorded stop transitions decide. The identity
	// named below always exists — am advances only onto inserted rows.
	doneStatus := res.Status
	switch {
	case midFull:
		doneStatus = "error"
	case storeErr != nil && (doneStatus == "" || doneStatus == "complete"):
		doneStatus = "error" // never announce a success the database refused
	case cancelledOut:
		doneStatus = "aborted"
	case limitStopped:
		doneStatus = "error"
	case genCtx.Err() != nil && doneStatus == "":
		doneStatus = "aborted"
	}
	// A reason the LAST saved row does not itself carry becomes that row's
	// durable note (C7), with the same precedence as the status above: a
	// round that reached 'error'/'aborted' already says what happened, so
	// only an outcome resting on an otherwise-complete row needs the extra
	// sentence. This is what makes the live explanation survive a reload.
	if midFull {
		noteStranded(noteWindowMidLoop)
	} else if errors.Is(storeErr, store.ErrMessageLimit) {
		noteStranded(store.MessageLimitNotice)
	} else if storeErr != nil {
		noteStranded(noteSaveIncomplete)
	} else if doneStatus == "aborted" {
		noteStranded(noteStoppedShort)
	} else if limitStopped {
		noteStranded("Tool call limit reached.")
	}
	_ = conn.Send(Finished{GenerationID: g.ID, Status: doneStatus, Rounds: rounds, Usage: res.Usage})
}
