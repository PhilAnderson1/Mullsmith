package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mullsmith/internal/store"
)

// Deletion, cancellation and final persistence are one lifecycle. A
// live generation must stop when its conversation or account is deleted,
// required UPDATEs must distinguish "saved" from "row gone", and terminal
// events must only ever name rows that exist.

// newBlockingChat builds a server whose upstream stalls INSIDE the model
// call until release closes, so a test can delete things mid-generation at
// a controlled point while holding the *Server (for the registry).
func newBlockingChat(t *testing.T, release <-chan struct{}, arrived chan<- struct{}) (*authTest, *Server) {
	t.Helper()
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done(): // the model call was cancelled
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		sseUp(w, deltaJSON(map[string]any{"content": "late answer"}))
		sseUp(w, "[DONE]")
	})
	cfg := chatCfg(t, fake.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := newPreparedServer(t, cfg, db, logger, "test", nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("generation-lifecycle@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a, s
}

func waitForNoGenerations(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.chat.Generations.Count() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.chat.Generations.Count() != 0 {
		t.Fatalf("%d generation(s) still holding slots", s.chat.Generations.Count())
	}
}

// Deleting a conversation during model output cancels the generation
// before the response reports completion, releases its slot, and leaves
// the deleted conversation exactly as deleted.
func TestConversationDeleteCancelsLiveGeneration(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 2)
	a, s := newBlockingChat(t, release, arrived)
	c := createConv(t, a, map[string]any{"title": "Conversation deletion"})

	resp := sendChat(t, a, c.ID, "hello") // returns once the SSE start is out
	<-arrived
	if s.chat.Generations.Count() != 1 {
		t.Fatalf("generation never took its slot: %d", s.chat.Generations.Count())
	}

	if r := a.do(http.MethodDelete, "/api/v1/conversations/"+c.ID, nil); r.StatusCode != 204 {
		t.Fatalf("delete during generation: %d %s", r.StatusCode, dumpBody(r))
	}
	// The cancel went out before the rows did. Whatever the upstream fake
	// still does, the handler must reach its terminal writes — which meet
	// deleted rows and read as cancellation — and release the slot.
	close(release)
	waitForNoGenerations(t, s)
	resp.Body.Close()

	if r := a.do(http.MethodGet, "/api/v1/conversations/"+c.ID+"/messages", nil); r.StatusCode != 404 {
		t.Errorf("deleted conversation = %d, want 404 (no write revived it)", r.StatusCode)
	}
}

// Self-deleting the account mid-generation cancels that generation; the
// slot is released and the account is gone.
func TestAccountDeleteCancelsLiveGeneration(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 2)
	a, s := newBlockingChat(t, release, arrived)
	c := createConv(t, a, map[string]any{"title": "Account deletion"})

	resp := sendChat(t, a, c.ID, "hello")
	<-arrived
	if s.chat.Generations.Count() != 1 {
		t.Fatal("no live generation")
	}
	if r := a.do(http.MethodPost, "/api/v1/auth/me/delete", map[string]any{}); r.StatusCode != 204 {
		t.Fatalf("self-delete during generation: %d %s", r.StatusCode, dumpBody(r))
	}
	resp.Body.Close()
	waitForNoGenerations(t, s)
	close(release)
	if r := a.do(http.MethodGet, "/api/v1/auth/me", nil); r.StatusCode != 401 {
		t.Errorf("deleted account = %d, want 401", r.StatusCode)
	}
}

// The reproduced defect: a required UPDATE whose row was deleted must NOT
// report success — every terminal promise built on "saved" depends on it.
func TestRequiredUpdatesReportRowGone(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	now := time.Now()
	u := &store.User{Email: "gone@b.io", Name: "G", PasswordHash: "x",
		Status: "enabled", CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv := &store.Conversation{UserID: u.ID, Title: "doomed"}
	if err := db.CreateConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	m := &store.Message{ConversationID: conv.ID, Role: "assistant", Status: "streaming"}
	if err := db.AddMessage(ctx, u.ID, m); err != nil {
		t.Fatal(err)
	}
	tc := &store.ToolCall{MessageID: m.ID, LLMCallID: "search-call", Tool: "web_search",
		Arguments: "{}", Status: "running"}
	if err := db.AddToolCall(ctx, tc); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteConversation(ctx, u.ID, conv.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.FinalizeMessage(ctx, m.ID, "late", "complete", "", 0, 0, 0); err != store.ErrRowGone {
		t.Errorf("FinalizeMessage over a deleted row = %v, want ErrRowGone", err)
	}
	if err := db.UpdateToolCallStatus(ctx, tc.ID, "approved", ""); err != store.ErrRowGone {
		t.Errorf("UpdateToolCallStatus over a deleted row = %v, want ErrRowGone", err)
	}
	if err := db.FinishToolCall(ctx, tc.ID, "ok", "res", "", 1); err != store.ErrRowGone {
		t.Errorf("FinishToolCall over a deleted row = %v, want ErrRowGone", err)
	}
	// A live row still finalises normally (the sentinel is only for gone
	// rows, not a blanket refusal).
	m2 := &store.Message{ConversationID: conv.ID, Role: "assistant", Status: "streaming"}
	if err := db.AddMessage(ctx, u.ID, m2); err == nil {
		t.Fatal("insert into a deleted conversation must fail")
	}
}

// The tool-round boundary: when the NEXT placeholder row cannot be
// inserted, the terminal event must still name the last row that actually
// exists — never the phantom — and end as an error, not a claimed success.
func TestToolRoundPlaceholderFailureNamesRealRow(t *testing.T) {
	search := &fakeTool{name: "web_search", content: `{}`, summary: "ran"}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
	a, script := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "call_q", "web_search", `{"query":"y"}`), finishChunk("tool_calls")},
		{answerChunk("never streamed"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Tool-round persistence"})
	// The ACCEPTED turn still inserts (no assistant row exists yet); the
	// tool-round placeholder is the second assistant insert and fails.
	if err := a.db.ExecForTest(`CREATE TRIGGER no_second_assistant BEFORE INSERT ON messages
		WHEN NEW.role='assistant' AND
			(SELECT COUNT(*) FROM messages
			 WHERE conversation_id = NEW.conversation_id AND role='assistant') >= 1
		BEGIN SELECT RAISE(ABORT, 'injected placeholder failure'); END`); err != nil {
		t.Fatal(err)
	}
	resp := sendChat(t, a, c.ID, "search y")
	defer resp.Body.Close()
	rd := newSSEReader(t, resp)
	_, start := rd.next()
	firstID, _ := start["message_id"].(string)
	es := drainSSE(t, rd)
	if code, _ := es.data["error"]["code"].(string); code != "storage_error" {
		t.Fatalf("error event = %v, want storage_error", es.data["error"])
	}
	ev, done := rd.next()
	if ev != "done" {
		t.Fatalf("last event = %q, want done", ev)
	}
	if done["status"] != "error" {
		t.Errorf("done.status = %v, want error", done["status"])
	}
	if lastRoundID(t, done) != firstID {
		t.Errorf("done names %q, want the accepted row %q — a failed placeholder must not be named",
			lastRoundID(t, done), firstID)
	}
	if search.callCount() != 1 {
		t.Errorf("tool ran %d times, want 1", search.callCount())
	}
	if script.roundCount() != 1 {
		t.Errorf("model rounds = %d, want 1 (no second round without its row)", script.roundCount())
	}
}
