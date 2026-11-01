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
	// Everything beyond request-shape validation runs inside ONE operation
	// boundary (C1): runGeneration reserves the conversation, reads the
	// authoritative snapshot under that reservation, admits the request and
	// commits it — in that order, with no second history read. The hourly
	// bucket, the busy refusals and the overflow gate all answer BEFORE
	// anything is persisted; a thinking choice shipped with the body is
	// validated and stored there too, so a capacity refusal can never
	// leave it applied (C1).
	s.runGeneration(w, r, genCommand{
		convID:    conv.ID,
		autoTitle: content, // may name a still-unnamed conversation (§12)
		extra:     content,
		attPaths:  attPaths,
		thinking:  body.Thinking,
		accept: func(bg context.Context, genID string) (*store.Message, error) {
			// ONE transaction (C2): the user turn, its attachment links,
			// the assistant draft and the activity bump commit together —
			// a failed send leaves NO turn behind the error the client
			// rolls its optimistic bubble back to, so a retry cannot
			// duplicate the message (the old split left the user row
			// durable under a plain HTTP failure).
			_, draft, err := s.store.BeginSend(bg, user.ID, conv.ID, content, attPaths,
				int64(agent.EstimateTokens(content)), genID)
			return draft, err
		},
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
	s.runGeneration(w, r, genCommand{
		convID:     conv.ID,
		regenMsgID: m.ID,
		accept: func(bg context.Context, genID string) (*store.Message, error) {
			// Atomic retire-and-replace (finding 4): newest-check, delete
			// and placeholder insert share ONE transaction — there is no
			// window where the old reply is gone without a replacement
			// standing in. The whole turn goes, not just the row this
			// button named (C7): a multi-round reply's earlier rounds and
			// their tool results are part of the answer being retired, and
			// leaving them behind made the retry quote the previous
			// attempt's own tool work.
			ph, err := s.store.BeginTurnRegeneration(bg, user.ID, conv.ID, m.ID, genID)
			return ph, err
		},
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

// --- shared operation boundary (R1/C1) -------------------------------------------

// genCommand describes one generation the shared boundary is about to
// reserve, admit and run: the conversation, the material the admission gate
// measures (the incoming turn and its attachments for a send; the excluded
// retired reply for a regenerate), the thinking choice shipped with a send,
// and the accept operation that makes the turn durable once admission has
// passed.
type genCommand struct {
	convID    string
	autoTitle string // user text that may name a still-unnamed conversation (§12)
	extra     string // send: the incoming user text; regenerate: ""
	attPaths  []string
	// regenMsgID is the reply a regeneration replaces (send: ""). The rows it
	// retires — the whole TURN, not the clicked row (C7) — are resolved under
	// the reservation and excluded from admission AND from the prompt, so the
	// retry never quotes the retired attempt's earlier rounds or tool results
	// from rows the swap is about to delete.
	regenMsgID string
	thinking   *string
	// accept makes the turn durable once admission has passed, and is handed
	// the generation identity (C7) to stamp on the rows it inserts: the
	// reservation mints it before anything is written, so every round row of
	// one operation carries the same id and "the rows of this turn" is a
	// query (regenerate retires a turn, not a row).
	accept func(bg context.Context, genID string) (*store.Message, error)
}

// roundOutcome is one round's DURABLE result as the generation-wide `done`
// event reports it (C7): the row's own id, the status that row actually
// reached, its speed figures and any note stored against it. The client
// applies each entry to the bubble that id names, which is what stops a
// generation-wide event from relabelling a bubble holding a different
// stored message.
type roundOutcome struct {
	MsgID      string `json:"message_id"`
	Status     string `json:"status"`
	Completion int64  `json:"completion_tokens,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Terminal notes (C7). The reason a transcript whose rows all saved cleanly
// still has no answer must be DURABLE, or a reload quietly disagrees with
// what the live stream said — and these exact strings are what the client
// renders, live and on reload, from messages.status_note.
const (
	noteStoppedShort   = "Stopped before the answer finished."
	noteSaveIncomplete = "The reply could not be fully saved; reload this conversation."
	noteWindowMidLoop  = "This conversation reached its context window during tool rounds — start a new chat."
)

// runGeneration is the ONE boundary every §6.1 send passes through: hourly
// bucket → conversation reservation → authoritative snapshot read →
// admission → accept (the atomic begin-send or regeneration swap) → the
// streaming loop, model-round → tool-calls → tool-results until the model
// answers (or the round/approval rules cut it short), pumping SSE the whole
// way.
//
// The order is the contract (C1): the history is read only AFTER the
// conversation is reserved, so the admission snapshot belongs to the
// reserved operation — a second request that races a running generation is
// refused generation_in_progress, it can never be admitted against history
// the running generation is about to invalidate. Admission is typed: a
// refusal carries a reason, a failed store read carries an error, and a nil
// history is never an error signal. Before accept nothing is persisted;
// after accept the turn belongs to the generation and every failure
// reconciles through the SSE terminal events (C2). A thinking choice
// shipped with a send is validated and persisted only once capacity has
// admitted it — a refusal leaves the stored setting untouched.
//
// All persistence happens on a context detached from the browser request so
// that disconnect-triggered aborts can still save the partial answer
// (spec §6.2).
func (s *Server) runGeneration(w http.ResponseWriter, r *http.Request, cmd genCommand) {
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
	g, err := s.gens.Start(cmd.convID, user.ID,
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

	// The conversation row re-read UNDER the reservation: a settings PATCH
	// that landed between the handler's lookup and this point is visible
	// here, and the conversation is append-only until this slot is
	// released — admission, acceptance and streaming all see this state.
	conv, err := s.store.ConversationForUser(genCtx, user.ID, cmd.convID)
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}

	// Overflow gate (spec §6.1): with a known window, a send that would
	// cross it is refused before anything is persisted or paid for. The
	// estimate covers the actual built request (finding 7) including this
	// turn's content and attachment manifests, and the row cap binds with
	// or without a discovered window (review2 finding 1). The history the
	// verdict was measured against rides into the prompt: decision and
	// request come from ONE read under ONE reservation. Unknown window:
	// the token gate is open and the endpoint's own error stands.
	// A regeneration's retire set is read HERE, under the reservation and
	// before admission (C7): the same rows BeginTurnRegeneration will delete,
	// so what the gate measured and the prompt built both describe the
	// history the swap actually leaves behind.
	var excludeIDs []string
	if cmd.regenMsgID != "" {
		ids, err := s.store.TurnRetireIDs(genCtx, conv.ID, cmd.regenMsgID)
		if err != nil {
			s.writeConvErr(w, r, err)
			return
		}
		excludeIDs = ids
	}

	adm, err := s.admitSend(genCtx, conv, user.ID, cmd.extra, cmd.attPaths, excludeIDs)
	if err != nil {
		// Typed store failure (C1): refuse without persisting — never a
		// silently re-read second opinion over a half-built snapshot.
		s.writeConvErr(w, r, err)
		return
	}
	if adm.full {
		s.fullRefusal(w, r, adm.reason)
		return
	}

	// A thinking choice shipped with the message is validated against the
	// backend's effective list and persisted once capacity admitted (C1):
	// an invalid one is a plain 400 that never reaches the endpoint, a
	// capacity refusal never leaves it applied, and a valid one is this
	// generation's snapshot — later tool rounds of the same reply speak it
	// even if the selector moves mid-flight.
	if cmd.thinking != nil {
		choice, fp, ok := s.validateThinkingChoice(s.thinkingModel(conv.Model), *cmd.thinking)
		if !ok {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_thinking_choice",
				"unknown thinking choice for this endpoint and profile")
			return
		}
		if choice != conv.Thinking || fp != conv.ThinkingCtx {
			updated, err := s.store.UpdateConversation(genCtx, user.ID, conv.ID,
				store.ConversationPatch{Thinking: &choice, ThinkingCtx: &fp})
			if err != nil {
				s.writeConvErr(w, r, err)
				return
			}
			conv = updated
		}
	}

	// Acceptance (C2): ONE store call makes the turn durable — BeginSend
	// commits user row, attachment links, draft and activity bump
	// together; BeginTurnRegeneration swaps the retired turn for its
	// placeholder atomically. A failure here is a plain HTTP error with
	// nothing committed; past this point the turn belongs to the accepted
	// generation.
	am, err := cmd.accept(genCtx, g.ID)
	if err != nil {
		s.writeConvErr(w, r, err)
		return
	}

	// From here on, errors travel as SSE events.

	// ONE prompt builder for this loop and the admission gate (finding 7):
	// what the gate measured is what is sent — conversation system prompt,
	// tool guidance, the vision-filtered tool list and attachment
	// manifests, with the incoming turn as the last row. The draft row is
	// status='streaming', so the admitted history never quotes it, and the
	// regeneration placeholder likewise.
	msgs, toolSpecs := s.buildRequest(conv, user.ID, adm.hist)
	visionOn := s.cfg.LLM.VisionFor(s.thinkingModel(conv.Model))

	conn, err := newSSE(w)
	if err != nil {
		// Best-effort: a failure here leaves the draft 'streaming', and the
		// startup sweep stamps it (C6) — nothing actionable mid-failure.
		_ = s.finishMessage(am.ID, "", "aborted", "", 0, 0, 0)
		return
	}
	// `start` is the acceptance signal AND round 1's identity (C7): its
	// message_id is the row this round streams into and the bubble key the
	// client keeps. Later rounds are announced by `round` events, so the
	// client renders the same row boundaries a reload will show.
	if err := conn.Event("start", map[string]any{
		"generation_id":   g.ID,
		"message_id":      am.ID,
		"conversation_id": conv.ID,
		"round":           0,
	}); err != nil {
		conn.Close()
		_ = s.finishMessage(am.ID, "", "aborted", "", 0, 0, 0) // swept if it fails too
		return
	}

	// Heartbeat every 15 s: keeps proxies warm and surfaces dead clients via
	// a write error, which cancels generation. Started before the title
	// quest below so the stream is never silent while the quest runs.
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

	// Auto-title quest (spec §12): name the conversation from this first
	// message BEFORE the agentic loop starts. Sequencing it first is what
	// makes it work on serialised endpoints: it claims the single model
	// slot while the browser is showing its thinking spinner, instead of
	// queueing behind the whole reply — where queue time would burn its
	// budget and any long reply would cancel its own title forever. On
	// parallel endpoints the ordering costs nothing visible (its own
	// round-trip lands before the first token). It runs on its own
	// context so a browser disconnect cannot strand the rename, and the
	// store write is guarded so a user's manual rename always wins.
	if cmd.autoTitle != "" && (conv.Title == "" || conv.Title == store.DefaultTitle) {
		s.nameConversation(conv, cmd.autoTitle, conn)
	}

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
	// rounds records each round's outcome IN ROW ORDER (C7). Every entry
	// exists because its row was really finalised (C6's rule, unchanged): a
	// write the database refused is not a round the client is told about.
	rounds := []roundOutcome{}
	// finRound finalises one round row and records what was persisted, so
	// the terminal event describes the writes rather than intentions.
	finRound := func(msgID, content, status, note string, usage, completion, durMS int64) error {
		ferr := s.finishMessage(msgID, content, status, note, usage, completion, durMS)
		if ferr == nil {
			rounds = append(rounds, roundOutcome{
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
					"conversation", conv.ID, "rid", RequestIDFrom(r.Context()))
			}
			return
		}
		rounds[len(rounds)-1].Note = text
	}
	round := 0
	for {
		roundTools := toolSpecs
		if round >= maxRounds {
			roundTools = nil // force a final, tool-less answer
		}
		if round > 0 && s.continuationFull(conv, msgs, roundTools) {
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
		res = s.runModelRound(genCtx, conn, s.thinkingModel(conv.Model), msgs, roundTools,
			thinkFrag, am.ID)
		roundMS := time.Since(roundStart).Milliseconds()

		if res.Status != "complete" {
			if ferr := finRound(am.ID, res.Content, res.Status, res.Note, res.Usage, res.Completion, roundMS); ferr != nil {
				storeErr = ferr
			}
			break
		}
		if len(res.ToolCalls) == 0 || round >= maxRounds {
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
		for _, call := range res.ToolCalls {
			if genCtx.Err() != nil {
				// The stop landed between calls (review3 issue 3): the
				// remaining side effects of this round do NOT happen, and
				// the generation ends as cancelled — not as the "complete"
				// the proposing round's status would have announced.
				cancelledOut = true
				break
			}
			result, attach, terr := s.runToolCall(genCtx, g, conn, user.ID, conv.ID, am.ID, call, visionOn)
			perr = terr
			msgs = append(msgs, llm.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result})
			if attach != "" {
				im, ok, aerr := s.attachImageView(user.ID, conv.ID, attach, g.ID)
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

		round++
		// The next draft row advances the generation's identity ONLY when
		// the insert really landed (C6): a failed placeholder must never
		// leave the terminal event naming a row that does not exist. am
		// keeps the last row that is durably there — already finalised
		// above — and done describes THAT state, nothing imagined.
		next := &store.Message{ConversationID: conv.ID, Role: "assistant",
			Status: "streaming", GenerationID: g.ID}
		if err := s.store.AddMessage(genCtx, user.ID, next); err != nil {
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
		if err := conn.Event("round", map[string]any{
			"generation_id": g.ID, "message_id": next.ID, "round": round + 1,
		}); err != nil {
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
	// Terminal events describe DURABLE outcomes (finding 4): the client
	// must be able to tell an endpoint failure from a storage failure from
	// the conversation hitting its window mid-flight — each needs a
	// different next move (retry / reload / new chat).
	if midFull {
		_ = conn.Event("error", map[string]any{
			"code":    "context_full",
			"message": "this conversation reached its context window during tool rounds — start a new chat",
		})
	}
	if storeErr != nil {
		s.logger.Error("generation persistence failed", "error", storeErr, "user", user.ID,
			"conversation", conv.ID, "rid", RequestIDFrom(r.Context()))
		_ = conn.Event("error", map[string]any{
			"code":    "storage_error",
			"message": "the reply could not be fully saved; reload this conversation",
		})
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
	} else if storeErr != nil {
		noteStranded(noteSaveIncomplete)
	} else if doneStatus == "aborted" {
		noteStranded(noteStoppedShort)
	}
	_ = conn.Event("done", map[string]any{
		"generation_id": g.ID,
		"status":        doneStatus,
		// Per-round outcomes in row order (C7): the client applies each one
		// to the bubble its own message_id names.
		"rounds":       rounds,
		"usage_tokens": res.Usage,
	})
	conn.Close()
}

// runModelRound streams one model round; tool deltas surface as
// tool_start/tool_args SSE events with stable call ids (§6.1 taxonomy).
// thinking is the generation's snapshotted request fragment (nil = no
// thinking override). msgID is the round's own row (C7): every event this
// round causes names it, so the client attaches cards to the bubble that
// matches the row the audit rows point at — not to "whatever bubble is
// last", which is what made a multi-round live view diverge from history.
func (s *Server) runModelRound(ctx context.Context, conn *sseConn, model string,
	msgs []llm.ChatMessage, toolSpecs []llm.ToolSpec, thinking map[string]any, msgID string,
) agent.Result {
	type card struct {
		callID  string
		started bool
	}
	cards := map[int]*card{}
	// announced freezes each slot's card id once (mirrors the accumulator's
	// "id freezes once published" rule, agent generate.go). A slot whose id
	// arrives late must NOT republish tool_start under a fresh mint: the
	// first card would hang as a phantom, then the accumulator's canonical
	// id would open a second card for the same call.
	announced := map[string]bool{}

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
			announced[c.callID] = true
			_ = conn.Event("tool_start", map[string]any{
				"call_id": c.callID, "tool": d.Name, "message_id": msgID,
			})
		}
		if c.started && d.Arguments != "" {
			_ = conn.Event("tool_args", map[string]any{
				"call_id": c.callID, "t": d.Arguments, "message_id": msgID,
			})
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

	// Defensive backstop: Round now mints ONE canonical id per call while
	// accumulating (finding 6), so every completed call arrives identified
	// and the live card already carries that same id. The loop stays — an
	// empty id reaching the protocol, audit rows or cards downstream would
	// orphan them, and this is the last cheap place to guarantee it.
	for i := range res.ToolCalls {
		if res.ToolCalls[i].ID == "" {
			res.ToolCalls[i].ID = id.New()
		}
	}
	// Backstop for calls the stream never announced (e.g. a name arriving
	// with no delta we saw). An already-announced card keeps its id even if
	// the completed call carries a different one: the card was published
	// under the frozen id and the arguments streamed into it — republishing
	// under the canonical id would leave the first card as a phantom.
	announcedID := func() (string, bool) {
		for _, c := range cards {
			if c.started && c.callID != "" {
				return c.callID, true
			}
		}
		return "", false
	}
	for _, call := range res.ToolCalls {
		if announced[call.ID] {
			continue
		}
		if _, dup := announcedID(); call.ID == "" || dup {
			continue // card exists under a frozen id; the views keep it
		}
		_ = conn.Event("tool_start", map[string]any{
			"call_id": call.ID, "tool": call.Function.Name, "message_id": msgID,
		})
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
		_ = conn.Event("tool_end", map[string]any{
			"call_id": callID, "tool": name, "status": status,
			"summary": c.Summary, "data": c.Data, "truncated": c.Truncated,
			"message_id": assistantMsgID,
		})
	}

	tool, known := s.tools.Get(name)
	if !known {
		emitEnd("error", card("unknown tool", nil))
		return `{"error":"unknown tool ` + jsonQuote(name) + `"}`, "", nil
	}
	// Belt to the spec filter: a model whose entry left vision off was
	// never offered view_image, so a call for it can only be a
	// hallucination — answered as such, never executed (§7.2).
	if name == "view_image" && !visionOn {
		emitEnd("error", card("model cannot see images", nil))
		return `{"error":"this model cannot see images"}`, "", nil
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
			s.remembered.remember(convID, name) // only after the approval is durable (C3, §9.9)
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
			dur, userID, convID, callID); ferr != nil {
			persistErr = ferr
		}
		emitEnd("error", c)
		return content, "", persistErr
	}

	content := tools.TruncateMiddle(tres.Content, toolResultBudgetRunes)
	c := card(tres.Summary, tres.Data)
	if ferr := s.finalizeToolCall(tc.ID, "ok", content, c.JSON(),
		dur, userID, convID, callID); ferr != nil {
		persistErr = ferr
	}
	emitEnd("ok", c)
	return content, tres.AttachPath, persistErr
}

// finalizeToolCall performs the two REQUIRED writes that close one tool
// call: the audit row's final state and the role='tool' answer row. Both
// errors surface — a call whose answer never landed must not be replayed
// from memory as though it had (review2 finding 2).
func (s *Server) finalizeToolCall(tcID, status, content, display string, dur int64,
	userID, convID, callID string,
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
		_ = s.appendToolResult(ctx, userID, convID, callID, content)
		return err
	}
	return s.appendToolResult(ctx, userID, convID, callID, content)
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
func (s *Server) awaitApproval(ctx context.Context, g *generation, conn *sseConn,
	callID, name, args, msgID string,
) approvalDecision {
	apID, decCh, ok := s.gens.AddApproval(g.ID)
	if !ok {
		return approvalDecision{} // generation gone: fail closed
	}
	defer s.gens.RemoveApproval(g.ID, apID)
	_ = conn.Event("awaiting_approval", map[string]any{
		"approval_id": apID, "call_id": callID, "tool": name, "arguments": args,
		"rememberable": name == "code_run", "message_id": msgID,
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

// appendToolResult persists the role='tool' answer row (§4). The error
// surfaces: replaying a result that never landed is exactly the live-vs-
// history divergence review2 finding 2 names.
func (s *Server) appendToolResult(ctx context.Context, userID, convID, callID, content string) error {
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
		return err
	}
	return nil
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
// the bytes vanished between validation and here, nothing embeds: false
// with no error, logged, and this round proceeds picture-less. A write
// that refuses the row is a typed persistence error (C3): the model was
// promised a picture, and continuing as though nothing happened would
// diverge live memory from replayable history.
func (s *Server) attachImageView(userID, convID, rel, genID string) (llm.ChatMessage, bool, error) {
	att := store.Attachment{Path: rel}
	manifest, urls := attachmentManifest(s.cfg.Scratchpad.Root, userID,
		[]store.Attachment{att}, manifestOpts{visionOn: true, embed: true, maxBytes: s.visionMaxBytes()})
	if len(urls) == 0 {
		s.logger.Warn("view_image embedded nothing (file gone or unreadable?)",
			"path", rel, "user", userID)
		return llm.ChatMessage{}, false, nil
	}
	note := viewTurnMarker + jsonQuote(att.Name()) +
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
func (s *Server) nameConversation(conv *store.Conversation, firstUser string, conn *sseConn) {
	// 30 s: an endpoint that thinks despite the disable needs room to
	// reason out loud before the title (see Title's budget note).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The title quest runs as quietly as the catalogues allow: this
	// model's off option, else its lightest rung (§12). The
	// conversation's own thinking choice never applies here — a title
	// is not a reply. Nil when no selector covers the model, leaving
	// llm.Complete's fallback hint.
	quiet := s.cfg.Thinking.QuietFragment(s.thinkingModel(conv.Model))
	title, err := s.generator.Title(ctx, s.thinkingModel(conv.Model), firstUser, quiet)
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
// The error reaches the orchestrator (finding 4) so terminal events can
// distinguish "saved" from "announced but not durable".
func (s *Server) finishMessage(msgID, content, status, note string, usage, completion, durationMS int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FinalizeMessage(ctx, msgID, content, status, note, usage, completion, durationMS); err != nil {
		s.logger.Error("failed to persist message final state", "error", err, "message", msgID)
		return err
	}
	return nil
}

// buildRequest is THE prompt builder (finding 7): stored history becomes the
// exact request the next model round carries — conversation system prompt,
// tool guidance, the vision-filtered tool list, attachment manifests — so
// the streaming loop and the admission gate measure and send the same
// thing. Returns the messages and the specs that round would advertise;
// the caller-provided turn a send has not yet persisted is appended to hist
// by the gate itself, so this function only ever reads stored rows.
func (s *Server) buildRequest(conv *store.Conversation, userID string, hist []store.Message) ([]llm.ChatMessage, []llm.ToolSpec) {
	// The tool list and picture pipeline follow the conversation's model:
	// view_image is offered only when its entry spells vision: true (§7.2),
	// and a hallucinated call to a tool that was never offered is answered
	// as an error, not executed.
	visionOn := s.cfg.LLM.VisionFor(s.thinkingModel(conv.Model))
	toolSpecs := s.tools.Specs()
	if !visionOn {
		filtered := toolSpecs[:0:0]
		for _, sp := range toolSpecs {
			if sp.Function.Name != "view_image" {
				filtered = append(filtered, sp)
			}
		}
		toolSpecs = filtered
	}
	sysPrompt := conv.SystemPrompt
	if len(toolSpecs) > 0 {
		sysPrompt += agent.ToolGuidance
		if _, hasCode := s.tools.Get("code_run"); hasCode {
			sysPrompt += agent.CodeRunGuidance
		}
	}
	return agent.BuildContext(sysPrompt, s.historyToLLM(hist, len(toolSpecs) > 0, visionOn, userID)), toolSpecs
}

// --- history replay ----------------------------------------------------------------

// historyToLLM maps persisted rows to OpenAI protocol messages with strict
// pairing: an assistant call replays only if its tool answer exists, and a
// tool row replays only if some replayed assistant call matches it. A crash
// between the two writes therefore degrades to plain text instead of
// poisoning every later request with an orphan protocol row. When tools are
// disabled the replay strips all protocol furniture — the endpoint is never
// told about tools in that mode. userID owns the conversation; it is the
// sandbox root owner for the attachment manifests. visionOn is the running
// model's entry flag (§7.2): with it off the manifest names no view_image
// and no synthetic turn embeds bytes — history replayed to a text-only
// model is text-only, whatever model wrote the rows.
func (s *Server) historyToLLM(hist []store.Message, toolsEnabled, visionOn bool, userID string) []llm.ChatMessage {
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
				embed := visionOn && m.Synthetic
				var manifest string
				manifest, imgs = attachmentManifest(s.cfg.Scratchpad.Root, userID,
					m.Attachments, manifestOpts{visionOn: visionOn, embed: embed,
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
