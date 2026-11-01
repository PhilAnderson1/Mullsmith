package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/tools"
)

// review3 regressions:
//
// Issue 1 — a FOREIGN account's conversation DELETE answers 404 and must
// leave the owner's live generation, rows and remembered consent untouched;
// the registry itself is owner-aware so no caller can cancel by convention
// alone.
//
// Issue 3 — cancellation between tool rounds (during approval, during
// execution, between calls) ends the generation as ABORTED: the proposing
// round's "complete" describes that round, never the whole request.

// --- issue 1 ----------------------------------------------------------------

func TestForeignDeleteCannotCancelOwnersGeneration(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 2)
	a, s := newBlockingChat(t, release, arrived)
	c := createConv(t, a, map[string]any{"title": "Mine"})

	resp := sendChat(t, a, c.ID, "hello")
	<-arrived
	if s.gens.Count() != 1 {
		t.Fatalf("generation never took its slot: %d", s.gens.Count())
	}
	// Consent on file for the owner's conversation — a side effect the
	// foreign delete must also leave alone.
	s.remembered.remember(c.ID, "code_run")

	// Second authenticated account on the same server. Invite codes are
	// single-use, so the intruder gets its own.
	b := newAuthTestOn(t, a.ts, a.db)
	b.invite("gk-code-2")
	if r := b.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "intruder@b.io", "name": "Intruder", "password": "uncommon-password-9",
		"remember_me": false, "invite": "gk-code-2",
	}); r.StatusCode != http.StatusCreated {
		t.Fatalf("second register: %d %s", r.StatusCode, dumpBody(r))
	}

	if r := b.do(http.MethodDelete, "/api/v1/conversations/"+c.ID, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign delete = %d, want 404", r.StatusCode)
	}
	// The owner's work is UNTOUCHED: still holding its slot (a cancel
	// would have ended the parked model call's generation bookkeeping).
	if s.gens.Count() != 1 {
		t.Fatalf("foreign delete cancelled the owner's generation (count %d)", s.gens.Count())
	}
	if !s.remembered.remembered(c.ID, "code_run") {
		t.Error("foreign delete dropped the owner's remembered consent")
	}

	// An unknown ID is equally inert.
	if r := b.do(http.MethodDelete, "/api/v1/conversations/0000000000000000000000", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown delete = %d, want 404", r.StatusCode)
	}
	if s.gens.Count() != 1 {
		t.Errorf("unknown delete touched the registry (count %d)", s.gens.Count())
	}

	// The owner's own delete still works end to end: cancel, slot gone,
	// consent forgotten with the conversation.
	if r := a.do(http.MethodDelete, "/api/v1/conversations/"+c.ID, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("owner delete = %d, want 204", r.StatusCode)
	}
	close(release)
	waitForNoGenerations(t, s)
	resp.Body.Close()
	if s.remembered.remembered(c.ID, "code_run") {
		t.Error("owner delete must forget remembered consent (§9.9)")
	}
}

// The registry refuses a foreign (conv, user) pair even if a caller ever
// forgets to authorize first — defence in depth (review3 issue 1).
func TestAbortConvIsOwnerAware(t *testing.T) {
	r := newGenRegistry()
	cancelled := 0
	cancel := func() { cancelled++ }
	g, err := r.Start("conv-1", "owner", 0, 0, cancel)
	if err != nil {
		t.Fatal(err)
	}
	if r.AbortConv("conv-1", "intruder") {
		t.Error("registry matched a foreign user's AbortConv")
	}
	if cancelled != 0 {
		t.Error("foreign AbortConv cancelled the generation")
	}
	if !r.AbortConv("conv-1", g.UserID) {
		t.Error("owner AbortConv must match and report the cancel")
	}
	if cancelled != 1 {
		t.Errorf("owner AbortConv cancelled %d times, want 1", cancelled)
	}
	if r.AbortConv("conv-absent", "owner") {
		t.Error("unknown conversation must report false")
	}
}

// --- issue 3 ----------------------------------------------------------------

// untilEvent drains rd to one named event (skipping the async titler's
// title frames) and returns its payload.
func untilEvent(t *testing.T, rd *sseReader, want string) map[string]any {
	t.Helper()
	for {
		ev, d := rd.next()
		if ev == "title" {
			continue
		}
		if ev == want {
			return d
		}
		if ev == "done" {
			t.Fatalf("stream ended (done) before %q: %v", want, d)
		}
	}
}

// blockingTool parks inside Execute until its context is cancelled — the
// reproduction shape: the user stops while a tool is still running.
type blockingTool struct {
	name    string
	entered chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *blockingTool) Name() string        { return b.name }
func (b *blockingTool) Description() string { return "blocks until cancelled" }
func (b *blockingTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}
}
func (b *blockingTool) Execute(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return tools.Result{}, ctx.Err()
}
func (b *blockingTool) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func newBlockingToolServer(t *testing.T, policies map[string]string, tool tools.Tool,
	replies [][]string,
) (*authTest, *toolLLM, *Server) {
	t.Helper()
	script := &toolLLM{replies: replies}
	fake := newFakeLLM(t, script.handler)
	cfg := toolChatCfg(t, 4, policies)
	cfg.LLM.Endpoint = fake.URL
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := New(cfg, db, logger, "test", nil, nil)
	s.tools = registryFor(policies, tool)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("review3@b.io", "uncommon-password-9", false); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a, script, s
}

// Abort while the loop waits for tool approval: the generation ends as
// ABORTED (the proposing round's "complete" must not leak into the
// terminal event), no model round two runs, and done names the real row.
func TestAbortDuringToolApprovalReportsAborted(t *testing.T) {
	search := &fakeTool{name: "web_search", content: `{}`, summary: "ran"}
	policies := map[string]string{"web_search": "confirm"}
	a, script, s := newBlockingToolServer(t, policies, search, [][]string{
		{toolCallChunk(0, "call_q", "web_search", `{"query":"y"}`), finishChunk("tool_calls")},
		{answerChunk("never streamed"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "R3a"})
	resp := sendChat(t, a, c.ID, "search y")
	defer resp.Body.Close()
	rd := newSSEReader(t, resp)
	ev, start := rd.next()
	if ev != "start" {
		t.Fatalf("first event = %q, want start", ev)
	}
	genID, _ := start["generation_id"].(string)
	firstID, _ := start["message_id"].(string)

	untilEvent(t, rd, "awaiting_approval") // the loop is parked on the user
	if r := a.do(http.MethodPost, "/api/v1/generations/"+genID+"/abort", nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("abort = %d, want 204", r.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	var done map[string]any
	for time.Now().Before(deadline) {
		ev, d := rd.next()
		if ev == "done" {
			done = d
			break
		}
	}
	if done == nil {
		t.Fatal("no done event after the abort")
	}
	if done["status"] != "aborted" {
		t.Errorf("done.status = %v, want aborted — approval-wait cancellation must not announce completion", done["status"])
	}
	if lastRoundID(t, done) != firstID {
		t.Errorf("done names %v, want the row that exists (%s)", lastRoundID(t, done), firstID)
	}
	if search.callCount() != 0 {
		t.Errorf("tool ran %d times despite the abort, want 0", search.callCount())
	}
	if script.roundCount() != 1 {
		t.Errorf("model rounds = %d, want 1 (no continuation after the abort)", script.roundCount())
	}
	waitForNoGenerations(t, s) // the slot is released
}

// Abort while a tool is EXECUTING, with a second call queued in the same
// round: the remaining call does not run, and the terminal status is
// aborted, not the round's "complete".
func TestAbortDuringToolExecutionReportsAbortedAndStopsCalls(t *testing.T) {
	blocker := &blockingTool{name: "web_search", entered: make(chan struct{}, 1)}
	policies := map[string]string{"web_search": "auto"}
	a, script, s := newBlockingToolServer(t, policies, blocker, [][]string{
		{
			toolCallChunk(0, "call_a", "web_search", `{"query":"1"}`),
			toolCallChunk(1, "call_b", "web_search", `{"query":"2"}`),
			finishChunk("tool_calls"),
		},
		{answerChunk("never streamed"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "R3b"})
	resp := sendChat(t, a, c.ID, "block please")
	defer resp.Body.Close()
	rd := newSSEReader(t, resp)
	ev, start := rd.next()
	if ev != "start" {
		t.Fatalf("first event = %q, want start", ev)
	}
	genID, _ := start["generation_id"].(string)
	firstID, _ := start["message_id"].(string)

	<-blocker.entered // the server is inside Execute now
	if r := a.do(http.MethodPost, "/api/v1/generations/"+genID+"/abort", nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("abort = %d, want 204", r.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	var done map[string]any
	for time.Now().Before(deadline) {
		ev, d := rd.next()
		if ev == "done" {
			done = d
			break
		}
	}
	if done == nil {
		t.Fatal("no done event after the abort")
	}
	if done["status"] != "aborted" {
		t.Errorf("done.status = %v, want aborted", done["status"])
	}
	if lastRoundID(t, done) != firstID {
		t.Errorf("done names %v, want the completed proposing row %s", lastRoundID(t, done), firstID)
	}
	if got := blocker.callCount(); got != 1 {
		t.Errorf("tool ran %d times, want 1 — call_b must not execute after the stop", got)
	}
	if script.roundCount() != 1 {
		t.Errorf("model rounds = %d, want 1", script.roundCount())
	}
	waitForNoGenerations(t, s)
}

// Control for both: the same scripted rounds run to completion with no
// cancellation — done says complete and the final answer row is named.
// (The cancelledOut flag must not fire on happy paths.)
func TestToolRoundCompletionStillReportsComplete(t *testing.T) {
	search := &fakeTool{name: "web_search", content: `{}`, summary: "ran"}
	policies := map[string]string{"web_search": "auto"}
	reg := registryFor(policies, search)
	cfg := toolChatCfg(t, 4, policies)
	a, script := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "call_q", "web_search", `{"query":"y"}`), finishChunk("tool_calls")},
		{answerChunk("final answer"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "R3c"})
	resp := sendChat(t, a, c.ID, "search y")
	defer resp.Body.Close()
	rd := newSSEReader(t, resp)
	rd.next() // start
	content, _, done := rd.untilDone()
	if done["status"] != "complete" {
		t.Errorf("done.status = %v, want complete", done["status"])
	}
	if content != "final answer" {
		t.Errorf("content = %q, want the final answer", content)
	}
	if script.roundCount() != 2 || search.callCount() != 1 {
		t.Errorf("rounds=%d calls=%d, want 2/1", script.roundCount(), search.callCount())
	}
}
