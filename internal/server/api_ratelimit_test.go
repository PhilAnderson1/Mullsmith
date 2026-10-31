package server

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

// --- audit finding 3: the configured limits must actually bite ---------------

func TestUserAPILimitEnforced(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.Limits.RequestsPerMinute = 3
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

func TestChatStreamExemptFromMinuteBucket(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("ok"))
	cfg := chatCfg(t, fake.URL)
	cfg.Limits.RequestsPerMinute = 1 // chat routes must ignore this
	a := signedInChat(t, cfg)

	// Drain the (non-chat) bucket with an ordinary call, then prove the
	// chat POST still streams, and ordinary calls stay refused.
	conv := createConv(t, a, map[string]any{"title": "exempt"})
	resp := a.do(http.MethodGet, "/api/v1/auth/me", nil) // ordinary: allowed (last token)
	resp.Body.Close()

	r := sendChat(t, a, conv.ID, "hello")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("chat POST with drained API bucket: %d, want 200 (exempt)", r.StatusCode)
	}
	rd := newSSEReader(t, r)
	rd.untilDone()
	r.Body.Close()

	resp = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("ordinary route after drain: %d, want 429", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestChatHourlyBucket(t *testing.T) {
	var llmCalls atomic.Int32
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		llmCalls.Add(1)
		scriptReply("fine")(w, r)
	})
	cfg := chatCfg(t, fake.URL)
	cfg.Limits.ChatMessagesPerHour = 2
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
	cfg.Limits.ConcurrentStreamsPerUser = 1
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
	cfg.Limits.ConcurrentStreamsPerUser = 4 // personal cap must not mask the global one
	cfg.Limits.ConcurrentStreamsTotal = 1
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
	r := newGenRegistry()
	noop := func() {}

	g1, err := r.Start("c1", "alice", 1, 2, noop)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := r.Start("c1", "alice", 1, 2, noop); err != errGenerating {
		t.Errorf("same conversation: err = %v, want errGenerating", err)
	}
	if _, err := r.Start("c2", "alice", 1, 2, noop); err != errUserStreams {
		t.Errorf("alice over personal cap: err = %v, want errUserStreams", err)
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
	if _, err := r.Start("c5", "carol", 0, 2, noop); err != errGlobalStreams {
		t.Errorf("carol vs full global cap: err = %v, want errGlobalStreams", err)
	}
	r.Finish(g1)
	r.Finish(g2)
	if len(r.byID) != 0 {
		t.Errorf("registry leaked after Finish: %+v", r.byID)
	}

	// Hammer: 8 parallel starts, distinct conversations, same user, personal
	// cap 3 ⇒ exactly 3 succeed under mutual exclusion (slots are HELD, so
	// later arrivals can never legitimately slip in after a finish).
	var wg sync.WaitGroup
	var mu sync.Mutex
	var held []*generation
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
		t.Errorf("concurrent starts: %d succeeded, want exactly 3 (cap), registry=%v", len(held), r.byID)
	}
	for _, g := range held {
		r.Finish(g)
	}
}
