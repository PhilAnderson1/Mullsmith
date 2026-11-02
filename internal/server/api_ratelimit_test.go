package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"mullsmith/internal/auth"
	"mullsmith/internal/chat"
	"mullsmith/internal/store"
)

// Request and generation limits refuse excess work before it starts.

func TestUserAPILimitEnforced(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.AppLimits.RequestsPerMinute = 3
	a := newAuthTest(t, cfg)
	signupAs(a, "rl@test.io", "gk-code-1")

	for i := 0; i < 3; i++ {
		resp := a.do(http.MethodGet, "/api/v1/auth/me", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d under a limit of 3: %d", i+1, resp.StatusCode)
		}
	}
	resp := a.do(http.MethodGet, "/api/v1/auth/me", nil)
	if resp.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp) != "rate_limited" {
		t.Fatalf("4th request: %d, want 429 rate_limited", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("429 without Retry-After")
	}
	resp.Body.Close()
}

func TestSendAndRegenerationShareAPIBudget(t *testing.T) {
	var calls atomic.Int32
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		scriptReply("ok")(w, r)
	})
	cfg := chatCfg(t, fake.URL)
	cfg.AppLimits.RequestsPerMinute = 3
	a := signedInChat(t, cfg)
	conv := createConv(t, a, map[string]any{"title": "Budget"}) // first token
	r := sendChat(t, a, conv.ID, "hello")                       // second token
	newSSEReader(t, r).untilDone()
	r.Body.Close()
	items, _, err := a.db.ListMessages(context.Background(), ownerOf(t, a).ID, conv.ID, "", 100)
	if err != nil || len(items) != 2 {
		t.Fatalf("history: %d %v", len(items), err)
	}
	r = a.do(http.MethodPost, "/api/v1/messages/"+items[1].ID+"/regenerate", map[string]any{}) // third token
	if r.StatusCode != http.StatusOK {
		t.Fatalf("regenerate: %d %s", r.StatusCode, dumpBody(r))
	}
	newSSEReader(t, r).untilDone()
	r.Body.Close()
	for _, path := range []string{"/api/v1/conversations/" + conv.ID + "/messages", "/api/v1/messages/" + items[1].ID + "/regenerate"} {
		r = a.do(http.MethodPost, path, map[string]any{"content": "refused"})
		if r.StatusCode != http.StatusTooManyRequests || apiErrCode(t, r) != "rate_limited" {
			t.Fatalf("exhausted %s: %d", path, r.StatusCode)
		}
		if r.Header.Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
	}
	r = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("ordinary route: %d", r.StatusCode)
	}
	r.Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("refused attempt reached model: %d calls", calls.Load())
	}
}

// Track reads so a refusal proves the handler did not parse the request body.
type trackedRequestBody struct {
	io.Reader
	reads int
}

func (b *trackedRequestBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }

func TestInvalidChatAttemptsSpendAPIBudget(t *testing.T) {
	for _, kind := range []string{"invalid JSON", "empty", "attachment", "foreign conversation", "foreign message"} {
		t.Run(kind, func(t *testing.T) {
			cfg := chatCfg(t, "http://127.0.0.1:1/v1")
			cfg.AppLimits.RequestsPerMinute = 2
			cfg.AppLimits.ChatMessagesPerHour = 1
			a := signedInChat(t, cfg)
			conv := createConv(t, a, map[string]any{"title": "Attempts"}) // one API token left
			foreign := mkStoreUser(t, a, "foreign@attempts.test")
			fc := &store.Conversation{UserID: foreign.ID, Title: "Foreign"}
			if err := a.db.CreateConversation(context.Background(), fc); err != nil {
				t.Fatal(err)
			}
			fm := &store.Message{ConversationID: fc.ID, Role: "assistant", Content: "Private"}
			if err := a.db.AddMessage(context.Background(), foreign.ID, fm); err != nil {
				t.Fatal(err)
			}
			path, payload, status := "/api/v1/conversations/"+conv.ID+"/messages", `{"content":""}`, http.StatusBadRequest
			switch kind {
			case "invalid JSON":
				payload = `{"content":`
			case "attachment":
				payload = `{"content":"hello","attachment_paths":["uploads/missing.txt"]}`
			case "foreign conversation":
				path, status = "/api/v1/conversations/"+fc.ID+"/messages", http.StatusNotFound
			case "foreign message":
				path, payload, status = "/api/v1/messages/"+fm.ID+"/regenerate", `{}`, http.StatusNotFound
			}
			request := func(body io.Reader) *http.Request {
				r := httptest.NewRequest(http.MethodPost, path, body)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set(auth.CSRFHeaderName, a.csrf)
				for k, v := range a.cookies {
					r.AddCookie(&http.Cookie{Name: k, Value: v})
				}
				return r
			}
			w := httptest.NewRecorder()
			a.ts.Config.Handler.ServeHTTP(w, request(strings.NewReader(payload)))
			if w.Code != status {
				t.Fatalf("initial invalid attempt: %d %s", w.Code, w.Body.String())
			}
			for i := 0; i < 3; i++ {
				body := &trackedRequestBody{Reader: strings.NewReader(payload + strings.Repeat(" ", 512<<10))}
				w := httptest.NewRecorder()
				a.ts.Config.Handler.ServeHTTP(w, request(body))
				if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"rate_limited"`) || body.reads != 0 {
					t.Fatalf("refusal: %d reads=%d body=%s", w.Code, body.reads, w.Body.String())
				}
			}
			items, _, err := a.db.ListMessages(context.Background(), ownerOf(t, a).ID, conv.ID, "", 100)
			if err != nil || len(items) != 0 {
				t.Fatalf("invalid send changed history: %d %v", len(items), err)
			}
		})
	}
}

func TestChatHourlyBucket(t *testing.T) {
	var llmCalls atomic.Int32
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		llmCalls.Add(1)
		scriptReply("fine")(w, r)
	})
	cfg := chatCfg(t, fake.URL)
	cfg.AppLimits.ChatMessagesPerHour = 2
	a := signedInChat(t, cfg)

	for i := 0; i < 2; i++ {
		r := sendChat(t, a, createConv(t, a, map[string]any{"title": "h"}).ID, "msg")
		rd := newSSEReader(t, r)
		rd.untilDone()
		r.Body.Close()
	}
	r := a.do(http.MethodPost, "/api/v1/conversations/"+createConv(t, a, map[string]any{"title": "h"}).ID+"/messages",
		map[string]any{"content": "one too many"})
	if r.StatusCode != http.StatusTooManyRequests || apiErrCode(t, r) != "chat_limited" {
		t.Fatalf("3rd message: %d, want 429 chat_limited", r.StatusCode)
	}
	r.Body.Close()
	if got := llmCalls.Load(); got != 2 {
		t.Errorf("refused message still reached the LLM %d times (expected refusal BEFORE work)", got)
	}
}

// TestChatPerUserStreamLimitAtomic is the regression for the check-then-act
// race: N parallel requests for DIFFERENT conversations must not all slip
// past a per-user cap of 1. With the reservation inside Start's mutex the
// outcome is deterministic; under the old split check it was a live race.
func TestChatPerUserStreamLimitAtomic(t *testing.T) {
	release, open := gate()
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		scriptReply("done")(w, r)
	})
	defer open()

	cfg := chatCfg(t, fake.URL)
	cfg.AppLimits.ConcurrentStreamsPerUser = 1
	a := signedInChat(t, cfg)
	const shots = 4
	convs := make([]string, shots)
	for i := range convs {
		convs[i] = createConv(t, a, map[string]any{"title": "race"}).ID
	}

	codes := make([]int, shots)
	respCh := make(chan *http.Response, shots)
	var wg sync.WaitGroup
	for i := 0; i < shots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := a.do(http.MethodPost, "/api/v1/conversations/"+convs[i]+"/messages",
				map[string]any{"content": "go"})
			codes[i] = resp.StatusCode
			if resp.StatusCode == http.StatusOK {
				respCh <- resp
			}
		}(i)
	}
	// The gate stays CLOSED until every POST has been classified: the winner
	// therefore holds its slot (the fake LLM never returns) while the others
	// attempt Start, so a non-atomic check-then-act cannot survive.
	wg.Wait()
	open()
	for len(respCh) > 0 {
		r := <-respCh
		rd := newSSEReader(t, r)
		rd.untilDone()
		r.Body.Close()
	}

	ok, busy := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			busy++
		}
	}
	if ok != 1 {
		t.Errorf("%d streams started with a per-user cap of 1 (codes %v)", ok, codes)
	}
	if ok+busy != shots {
		t.Errorf("unexpected status mix: %v", codes)
	}
}

func TestChatGlobalStreamCap(t *testing.T) {
	release, open := gate()
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		scriptReply("done")(w, r)
	})
	defer open()

	cfg := chatCfg(t, fake.URL)
	cfg.AppLimits.ConcurrentStreamsPerUser = 4 // personal cap must not mask the global one
	cfg.AppLimits.ConcurrentStreamsTotal = 1
	a := signedInChat(t, cfg)
	c1 := createConv(t, a, map[string]any{"title": "g1"})
	c2 := createConv(t, a, map[string]any{"title": "g2"})

	r1 := sendChat(t, a, c1.ID, "go")
	rd := newSSEReader(t, r1)
	rd.next()

	r2 := a.do(http.MethodPost, "/api/v1/conversations/"+c2.ID+"/messages",
		map[string]any{"content": "also go"})
	if r2.StatusCode != http.StatusServiceUnavailable || apiErrCode(t, r2) != "server_busy" {
		t.Fatalf("second stream vs global cap 1: %d, want 503 server_busy", r2.StatusCode)
	}
	r2.Body.Close()
	open()
	rd.untilDone()
	r1.Body.Close()
}

// --- registry unit level: reservation is atomic by construction --------------

func TestGenRegistryAtomicReservation(t *testing.T) {
	r := chat.NewRegistry()
	noop := func() {}

	g1, err := r.Start("c1", "alice", 1, 2, noop)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := r.Start("c1", "alice", 1, 2, noop); err != chat.ErrGenerating {
		t.Errorf("same conversation: err = %v, want chat.ErrGenerating", err)
	}
	if _, err := r.Start("c2", "alice", 1, 2, noop); err != chat.ErrUserStreams {
		t.Errorf("alice over personal cap: err = %v, want chat.ErrUserStreams", err)
	}
	// bob is under his personal cap but hits the GLOBAL cap of 2...
	if _, err := r.Start("c3", "bob", 1, 1, noop); err == nil {
		t.Errorf("global cap 1 with 1 running should refuse bob")
	}
	// ...global cap 2 admits him; then it is full.
	g2, err := r.Start("c4", "bob", 1, 2, noop)
	if err != nil {
		t.Fatalf("bob under global cap 2: %v", err)
	}
	if _, err := r.Start("c5", "carol", 0, 2, noop); err != chat.ErrGlobalStreams {
		t.Errorf("carol vs full global cap: err = %v, want chat.ErrGlobalStreams", err)
	}
	r.Finish(g1)
	r.Finish(g2)
	if r.Count() != 0 {
		t.Errorf("registry leaked after Finish: %+v", r.Count())
	}

	// Hammer: 8 parallel starts, distinct conversations, same user, personal
	// cap 3 ⇒ exactly 3 succeed under mutual exclusion (slots are HELD, so
	// later arrivals can never legitimately slip in after a finish).
	var wg sync.WaitGroup
	var mu sync.Mutex
	var held []*chat.Generation
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if g, err := r.Start(string(rune('a'+i)), "dave", 3, 0, noop); err == nil {
				mu.Lock()
				held = append(held, g)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(held) != 3 {
		t.Errorf("concurrent starts: %d succeeded, want exactly 3 (cap), registry=%v", len(held), r.Count())
	}
	for _, g := range held {
		r.Finish(g)
	}
}
