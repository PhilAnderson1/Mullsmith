package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/store"
)

// --- fake OpenAI-compatible upstream ----------------------------------------

func newFakeLLM(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// deltaJSON builds one OpenAI-style streaming chunk around a delta object.
func deltaJSON(delta map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": delta}},
	})
	return string(b)
}

func sseUp(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// scriptReply streams fixed content pieces, then finishes cleanly.
func scriptReply(pieces ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, p := range pieces {
			sseUp(w, deltaJSON(map[string]any{"content": p}))
		}
		sseUp(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		sseUp(w, "[DONE]")
	}
}

// chatCfg points the server at the fake endpoint with sane budgets.
func chatCfg(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	cfg := authCfg(t)
	cfg.LLM.Endpoint = endpoint
	cfg.LLM.Model = "m1"
	cfg.LLM.Models = []string{"m1"}
	cfg.LLM.MaxContextTokens = 4096
	cfg.LLM.MaxTokensResponse = 256
	return cfg
}

func signedInChat(t *testing.T, cfg *config.Config) *authTest {
	t.Helper()
	a := newAuthTest(t, cfg)
	a.invite("gk-code-1") // register() replays exactly this code
	if resp := a.register("chat@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a
}

func ownerOf(t *testing.T, a *authTest) *store.User {
	t.Helper()
	u, err := a.db.UserByEmail(context.Background(), "chat@b.io")
	if err != nil || u == nil {
		t.Fatalf("owner lookup: %v", err)
	}
	return u
}

// --- client-side SSE reader ----------------------------------------------------

type sseReader struct {
	t  *testing.T
	sc *bufio.Scanner
}

func newSSEReader(t *testing.T, resp *http.Response) *sseReader {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	return &sseReader{t: t, sc: bufio.NewScanner(resp.Body)}
}

// next returns the next non-comment event; fails the test at EOF.
func (s *sseReader) next() (string, map[string]any) {
	s.t.Helper()
	var ev string
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case line == "" || strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				s.t.Fatalf("bad SSE data JSON %q: %v", line, err)
			}
			out := ev
			ev = ""
			return out, m
		}
	}
	if err := s.sc.Err(); err != nil {
		s.t.Fatalf("SSE read error waiting for event: %v", err)
	}
	s.t.Fatalf("SSE stream ended while waiting for an event (last wanted: %q)", ev)
	return "", nil
}

// untilDone drains events, concatenating token payloads, and returns the
// done-event data plus the reasoning seen.
func (s *sseReader) untilDone() (content, reasoning string, done map[string]any) {
	s.t.Helper()
	for {
		ev, d := s.next()
		piece, _ := d["t"].(string)
		switch ev {
		case "token":
			content += piece
		case "reasoning":
			reasoning += piece
		case "done":
			return content, reasoning, d
		case "error":
			s.t.Fatalf("unexpected error event mid-stream: %v", d)
		}
	}
}

func sendChat(t *testing.T, a *authTest, convID, content string) *http.Response {
	t.Helper()
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+convID+"/messages",
		map[string]any{"content": content})
	if resp.StatusCode != 200 {
		t.Fatalf("chat send: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return resp
}

func waitForStatus(t *testing.T, db *store.DB, userID, msgID string, want ...string) *store.Message {
	t.Helper()
	str := func(m *store.Message) string {
		if m == nil {
			return "<missing>"
		}
		return m.Status
	}
	deadline := time.Now().Add(5 * time.Second)
	var last *store.Message
	for time.Now().Before(deadline) {
		m, err := db.GetMessageForUser(context.Background(), userID, msgID)
		if err == nil {
			last = m
			for _, w := range want {
				if m.Status == w {
					return m
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("message %s stuck in status %q, want %v", msgID, str(last), want)
	return nil
}

// gate is a close-once channel: fake upstreams wait on it, tests release it
// early or via defer without risking a double close.
func gate() (chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

type msgPage struct {
	Items []struct {
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content string `json:"content"`
		Status  string `json:"status"`
	} `json:"items"`
}

// --- tests ---------------------------------------------------------------------

func TestChatSendStreamsAndPersists(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("Hel", "lo", " there"))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "T"})

	resp := sendChat(t, a, c.ID, "hi")
	defer resp.Body.Close()
	rd := newSSEReader(t, resp)

	ev, start := rd.next()
	if ev != "start" {
		t.Fatalf("first event = %q, want start", ev)
	}
	msgID, _ := start["message_id"].(string)
	genID, _ := start["generation_id"].(string)
	if msgID == "" || genID == "" {
		t.Fatalf("start payload = %v", start)
	}

	var got strings.Builder
	for _, want := range []string{"Hel", "lo", " there"} {
		ev, d := rd.next()
		if ev != "token" {
			t.Fatalf("event = %q, want token", ev)
		}
		if s, _ := d["t"].(string); s != want {
			t.Fatalf("token = %q, want %q", s, want)
		}
		got.WriteString(want)
	}
	ev, done := rd.next()
	if ev != "done" {
		t.Fatalf("last event = %q, want done", ev)
	}
	if done["status"] != "complete" || done["message_id"] != msgID {
		t.Errorf("done = %v", done)
	}

	// Both turns persisted and complete.
	var page msgPage
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	if len(page.Items) != 2 {
		t.Fatalf("messages = %+v", page.Items)
	}
	if page.Items[0].Role != "user" || page.Items[0].Content != "hi" || page.Items[0].Status != "complete" {
		t.Errorf("user row = %+v", page.Items[0])
	}
	if page.Items[1].ID != msgID || page.Items[1].Role != "assistant" ||
		page.Items[1].Content != "Hello there" || page.Items[1].Status != "complete" {
		t.Errorf("assistant row = %+v", page.Items[1])
	}
}

func TestChatContextSentUpstream(t *testing.T) {
	type chatReq struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	reqs := make(chan chatReq, 8) // channel hand-off: race-free + ordered
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		var cr chatReq
		json.NewDecoder(r.Body).Decode(&cr)
		reqs <- cr
		scriptReply("ok")(w, r)
	})
	cfg := chatCfg(t, fake.URL)
	a := signedInChat(t, cfg)
	c := createConv(t, a, map[string]any{"title": "Ctx", "system_prompt": "Be terse."})

	// First turn.
	resp := sendChat(t, a, c.ID, "first question")
	rd := newSSEReader(t, resp)
	rd.untilDone()
	resp.Body.Close()
	var last = <-reqs

	if last.Model != "m1" || !last.Stream {
		t.Errorf("upstream model/stream = %q/%v", last.Model, last.Stream)
	}
	if len(last.Messages) != 2 || last.Messages[0].Role != "system" ||
		!strings.HasPrefix(last.Messages[0].Content, "Be terse.") || last.Messages[1].Content != "first question" {
		t.Errorf("turn 1 messages = %+v", last.Messages)
	}

	// Second turn must carry the whole prior exchange plus the new one.
	resp = sendChat(t, a, c.ID, "second question")
	rd = newSSEReader(t, resp)
	rd.untilDone()
	resp.Body.Close()
	last = <-reqs

	want := []string{"system", "user", "assistant", "user"}
	if len(last.Messages) != len(want) {
		t.Fatalf("turn 2 messages = %+v", last.Messages)
	}
	for i, role := range want {
		if last.Messages[i].Role != role {
			t.Errorf("turn 2 msg %d role = %q, want %q", i, last.Messages[i].Role, role)
		}
	}
	if last.Messages[3].Content != "second question" {
		t.Errorf("current message wrong: %+v", last.Messages[3])
	}
}

func TestChatReasoningGating(t *testing.T) {
	reasoningFake := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		sseUp(w, deltaJSON(map[string]any{"reasoning_content": "hmm "}))
		sseUp(w, deltaJSON(map[string]any{"reasoning_content": "ok"}))
		sseUp(w, deltaJSON(map[string]any{"content": "answer"}))
		sseUp(w, "[DONE]")
	}

	t.Run("hidden by default", func(t *testing.T) {
		fake := newFakeLLM(t, reasoningFake)
		cfg := chatCfg(t, fake.URL) // ShowReasoning false
		a := signedInChat(t, cfg)
		c := createConv(t, a, map[string]any{"title": "R"})
		resp := sendChat(t, a, c.ID, "q")
		rd := newSSEReader(t, resp)
		rd.next() // start
		ev, d := rd.next()
		if ev != "token" {
			t.Fatalf("first streamed event = %q, want token (reasoning must not leak)", ev)
		}
		_ = d
		rd.untilDone()
		resp.Body.Close()
	})

	t.Run("shown when enabled", func(t *testing.T) {
		fake := newFakeLLM(t, reasoningFake)
		cfg := chatCfg(t, fake.URL)
		cfg.LLM.ShowReasoning = true
		a := signedInChat(t, cfg)
		c := createConv(t, a, map[string]any{"title": "R"})
		resp := sendChat(t, a, c.ID, "q")
		rd := newSSEReader(t, resp)
		rd.next() // start
		ev, _ := rd.next()
		if ev != "reasoning" {
			t.Fatalf("first streamed event = %q, want reasoning", ev)
		}
		content, reasoning, done := rd.untilDone()
		if done["status"] != "complete" {
			t.Errorf("done = %v", done)
		}
		// reasoning arrives streamed; the final content excludes it.
		_ = content
		if reasoning == "" {
			t.Error("expected reasoning deltas streamed")
		}
		resp.Body.Close()

		// Persisted content must not contain reasoning text.
		var page msgPage
		getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
		assistant := page.Items[len(page.Items)-1]
		if strings.Contains(assistant.Content, "hmm") {
			t.Errorf("reasoning leaked into stored content: %q", assistant.Content)
		}
	})
}

func TestChatBusyConversation409(t *testing.T) {
	release, open := gate()
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		scriptReply("late")(w, r)
	})
	defer open()

	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Busy"})

	resp1 := sendChat(t, a, c.ID, "first")
	rd := newSSEReader(t, resp1)
	rd.next() // start — generation is now registered

	// Second send while the first is in flight: 409, JSON envelope.
	resp2 := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages",
		map[string]any{"content": "second"})
	if resp2.StatusCode != http.StatusConflict || apiErrCode(t, resp2) != "generation_in_progress" {
		t.Fatalf("concurrent send = %d, want 409 generation_in_progress", resp2.StatusCode)
	}

	open()
	rd.untilDone()
	resp1.Body.Close()
}

func TestChatPerUserStreamLimit(t *testing.T) {
	release, open := gate()
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		scriptReply("done")(w, r)
	})
	defer open()

	cfg := chatCfg(t, fake.URL)
	cfg.Limits.ConcurrentStreamsPerUser = 1
	a := signedInChat(t, cfg)
	c1 := createConv(t, a, map[string]any{"title": "A"})
	c2 := createConv(t, a, map[string]any{"title": "B"})

	resp1 := sendChat(t, a, c1.ID, "go")
	rd := newSSEReader(t, resp1)
	rd.next() // registered

	resp2 := a.do(http.MethodPost, "/api/v1/conversations/"+c2.ID+"/messages",
		map[string]any{"content": "also go"})
	if resp2.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp2) != "too_many_streams" {
		t.Fatalf("second conversation stream = %d, want 429 too_many_streams", resp2.StatusCode)
	}
	open()
	rd.untilDone()
	resp1.Body.Close()
}

func TestChatAbortEndpoint(t *testing.T) {
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 500; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(25 * time.Millisecond):
			}
			sseUp(w, deltaJSON(map[string]any{"content": fmt.Sprintf("w%d ", i)}))
		}
	})
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Stop"})
	u := ownerOf(t, a)

	resp := sendChat(t, a, c.ID, "stream forever")
	rd := newSSEReader(t, resp)
	_, start := rd.next()
	genID, _ := start["generation_id"].(string)
	msgID, _ := start["message_id"].(string)
	rd.next() // one token so there is something to persist

	ab := a.do(http.MethodPost, "/api/v1/generations/"+genID+"/abort", nil)
	ab.Body.Close()
	if ab.StatusCode != 204 {
		t.Fatalf("abort = %d, want 204", ab.StatusCode)
	}

	// The stream terminates with done/aborted (no more tokens after).
	_, _, done := rd.untilDone()
	if done["status"] != "aborted" {
		t.Errorf("done status = %v, want aborted", done["status"])
	}
	resp.Body.Close()

	m := waitForStatus(t, a.db, u.ID, msgID, "aborted")
	if m.Content == "" {
		t.Error("partial content was not persisted on abort")
	}

	// Aborting again (or an unknown id) is a 404 — and ids cannot be
	// probed across users (ownership checked in Abort). Polled rather than
	// asserted once: the registry entry is dropped by the generation's
	// deferred Finish (api_chat.go), which runs *after* the status write
	// this test already waited on, so under full-suite load the re-abort
	// can otherwise arrive in the unregister window and see 204.
	deadline := time.Now().Add(5 * time.Second)
	for {
		again := a.do(http.MethodPost, "/api/v1/generations/"+genID+"/abort", nil)
		again.Body.Close()
		if again.StatusCode == 404 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("re-abort = %d, want 404", again.StatusCode)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChatClientDisconnectSavesPartial(t *testing.T) {
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 500; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(25 * time.Millisecond):
			}
			sseUp(w, deltaJSON(map[string]any{"content": fmt.Sprintf("w%d ", i)}))
		}
	})
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Disconnect"})
	u := ownerOf(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{"content": "go"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		a.ts.URL+"/api/v1/conversations/"+c.ID+"/messages", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gabkeep-CSRF", a.csrf)
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rd := newSSEReader(t, resp)
	_, start := rd.next()
	msgID, _ := start["message_id"].(string)
	rd.next() // first token arrived

	cancel() // browser closes the tab
	resp.Body.Close()

	m := waitForStatus(t, a.db, u.ID, msgID, "aborted")
	if m.Content == "" {
		t.Error("disconnect-time partial save missing")
	}
}

func TestRegenerate(t *testing.T) {
	var n int32
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&n, 1)
		scriptReply(fmt.Sprintf("reply-%d", i))(w, r)
	})
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Regen"})
	u := ownerOf(t, a)

	resp := sendChat(t, a, c.ID, "ask")
	rd := newSSEReader(t, resp)
	_, start := rd.next()
	firstID, _ := start["message_id"].(string)
	rd.untilDone()
	resp.Body.Close()

	// Regenerate it.
	resp = a.do(http.MethodPost, "/api/v1/messages/"+firstID+"/regenerate", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("regenerate: %d %s", resp.StatusCode, dumpBody(resp))
	}
	rd = newSSEReader(t, resp)
	_, start2 := rd.next()
	secondID, _ := start2["message_id"].(string)
	if secondID == "" || secondID == firstID {
		t.Fatalf("regenerate reused message id %q", secondID)
	}
	rd.untilDone()
	resp.Body.Close()

	// History is user + ONE assistant (the new one); old reply deleted.
	var page msgPage
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	if len(page.Items) != 2 {
		t.Fatalf("after regenerate messages = %+v", page.Items)
	}
	if page.Items[1].ID != secondID || page.Items[1].Content != "reply-2" {
		t.Errorf("assistant row = %+v, want the regenerated reply", page.Items[1])
	}
	if _, err := a.db.GetMessageForUser(context.Background(), u.ID, firstID); err != store.ErrConversationNotFound {
		t.Errorf("old assistant message survived: %v", err)
	}
}

func TestRegenerateRejections(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("r1", " r2"))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Rej"})
	u := ownerOf(t, a)

	resp := sendChat(t, a, c.ID, "ask")
	rd := newSSEReader(t, resp)
	_, start := rd.next()
	assistantID, _ := start["message_id"].(string)
	rd.untilDone()
	resp.Body.Close()

	// A user message is not regeneratable.
	var page msgPage
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	userID := page.Items[0].ID
	r := a.do(http.MethodPost, "/api/v1/messages/"+userID+"/regenerate", nil)
	if r.StatusCode != 400 || apiErrCode(t, r) != "not_regeneratable" {
		t.Errorf("user-message regenerate = %d, want 400 not_regeneratable", r.StatusCode)
	}

	// Append a newer message directly; the assistant row is no longer last.
	if err := a.db.AddMessage(context.Background(), u.ID, &store.Message{
		ConversationID: c.ID, Role: "user", Content: "newest"}); err != nil {
		t.Fatal(err)
	}
	r = a.do(http.MethodPost, "/api/v1/messages/"+assistantID+"/regenerate", nil)
	if r.StatusCode != 400 || apiErrCode(t, r) != "not_last_message" {
		t.Errorf("not-last regenerate = %d, want 400 not_last_message", r.StatusCode)
	}

	// Unknown message id reads as plain 404.
	r = a.do(http.MethodPost, "/api/v1/messages/0000000000000000000000/regenerate", nil)
	if r.StatusCode != 404 || apiErrCode(t, r) != "not_found" {
		t.Errorf("unknown regenerate = %d, want 404", r.StatusCode)
	}
}

func TestChatUpstreamFailure(t *testing.T) {
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model overloaded", 503)
	})
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Fail"})
	u := ownerOf(t, a)

	resp := sendChat(t, a, c.ID, "hi")
	rd := newSSEReader(t, resp)
	_, start := rd.next()
	msgID, _ := start["message_id"].(string)

	ev, data := rd.next()
	if ev != "error" {
		t.Fatalf("event = %q, want error", ev)
	}
	if data["code"] != "upstream_error" {
		t.Errorf("error code = %v", data["code"])
	}
	if strings.Contains(fmt.Sprint(data["message"]), "503") ||
		strings.Contains(fmt.Sprint(data), "overloaded") {
		t.Errorf("upstream detail leaked to client: %v", data)
	}
	ev, done := rd.next()
	if ev != "done" || done["status"] != "error" {
		t.Errorf("last event = %q %v, want done/error", ev, done)
	}
	resp.Body.Close()

	m := waitForStatus(t, a.db, u.ID, msgID, "error")
	if m.Status != "error" {
		t.Errorf("stored status = %q", m.Status)
	}
}

func TestChatValidation(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("x"))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "V"})

	cases := []struct {
		name, path string
		body       map[string]any
		want       int
		code       string
	}{
		{"empty", c.ID, map[string]any{"content": "   "}, 400, "empty_message"},
		{"too long", c.ID, map[string]any{"content": strings.Repeat("x", 32001)}, 400, "message_too_long"},
		{"attachments", c.ID, map[string]any{"content": "hi", "attachment_paths": []string{"uploads/ghost.csv"}}, 400, "attachment_not_found"},
		{"attachment outside uploads", c.ID, map[string]any{"content": "hi", "attachment_paths": []string{"work/plan.txt"}}, 400, "attachment_not_found"},
		{"attachment bogus shape", c.ID, map[string]any{"content": "hi", "attachment_paths": []string{"../uploads/x"}}, 400, "attachment_not_found"},
		{"unknown conv", "0000000000000000000000", map[string]any{"content": "hi"}, 404, "not_found"},
	}
	for _, tc := range cases {
		resp := a.do(http.MethodPost, "/api/v1/conversations/"+tc.path+"/messages", tc.body)
		if resp.StatusCode != tc.want || apiErrCode(t, resp) != tc.code {
			t.Errorf("%s: %d %v, want %d %s", tc.name, resp.StatusCode, dumpBody(resp), tc.want, tc.code)
		} else {
			resp.Body.Close()
		}
	}
}

func TestChatRequiresAuthAndCSRF(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("x"))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Sec"})

	// Logged out.
	logout := a.do(http.MethodPost, "/api/v1/auth/logout", nil)
	logout.Body.Close()
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages",
		map[string]any{"content": "hi"})
	if resp.StatusCode != 401 || apiErrCode(t, resp) != "unauthenticated" {
		t.Errorf("logged-out chat = %d, want 401", resp.StatusCode)
	}

	// Signed in again, then a request without the CSRF header.
	if r := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "chat@b.io", "password": "uncommon-password-9"}); r.StatusCode != 200 {
		t.Fatalf("re-login: %d %s", r.StatusCode, dumpBody(r))
	}
	body, _ := json.Marshal(map[string]any{"content": "hi"})
	req, _ := http.NewRequest(http.MethodPost,
		a.ts.URL+"/api/v1/conversations/"+c.ID+"/messages", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	noCSRF, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	noCSRF.Body.Close()
	if noCSRF.StatusCode != 403 {
		t.Errorf("missing CSRF = %d, want 403", noCSRF.StatusCode)
	}
}

func TestSweepStreamingHelper(t *testing.T) {
	// Store-level: rows stuck in streaming become aborted exactly once.
	fake := newFakeLLM(t, scriptReply("x"))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "Sweep"})
	u := ownerOf(t, a)

	m := &store.Message{ConversationID: c.ID, Role: "assistant", Status: "streaming", Content: "half"}
	if err := a.db.AddMessage(context.Background(), u.ID, m); err != nil {
		t.Fatal(err)
	}
	n, err := a.db.SweepStreaming(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d %v", n, err)
	}
	got, err := a.db.GetMessageForUser(context.Background(), u.ID, m.ID)
	if err != nil || got.Status != "aborted" {
		t.Fatalf("after sweep = %+v %v", got, err)
	}
	if n, _ := a.db.SweepStreaming(context.Background()); n != 0 {
		t.Errorf("second sweep touched %d rows", n)
	}
}
