package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mullsmith/internal/config"
)

// stalledExit lets a parked fake upstream return before httptest's Close.
// t.Cleanup is LIFO: the release registers AFTER newFakeLLM's ts.Close, so
// it runs BEFORE it. (The server cannot see the client's TCP close while a
// handler is parked without an in-flight read.)
func stalledExit(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	t.Cleanup(func() { close(ch) })
	return ch
}

// An endpoint that sends headers and one token, then goes
// silent, is cut by the stall budget and travels the EXISTING failure path
// end to end — partial output persisted, sanitized upstream_error event,
// done/status error, and the conversation slot free for the next send.
func TestChatStalledUpstreamCutsAndReleasesSlot(t *testing.T) {
	var requests int32
	var release <-chan struct{}
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			sseUp(w, deltaJSON(map[string]any{"content": "half an "}))
			<-release // then silence forever: the budget does the rest
			return
		}
		sseUp(w, deltaJSON(map[string]any{"content": "second answer"}))
		sseUp(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		sseUp(w, "[DONE]")
	})
	release = stalledExit(t)

	cfg := chatCfg(t, fake.URL)
	cfg.LLM.UpstreamStallTimeout = config.Duration(400 * time.Millisecond)
	a := signedInChat(t, cfg)
	c := createConv(t, a, map[string]any{"title": "Stall"})
	u := ownerOf(t, a)

	start := time.Now()
	resp := sendChat(t, a, c.ID, "hi")
	rd := newSSEReader(t, resp)
	_, ev0 := rd.next()
	msgID, _ := ev0["message_id"].(string)

	// Token(s) may arrive before the trip; what must end the stream is the
	// sanitized upstream_error, then done/error.
	var got strings.Builder
	for {
		ev, data := rd.next()
		switch ev {
		case "token":
			got.WriteString(pieStr(data))
		case "error":
			if data["code"] != "upstream_error" {
				t.Fatalf("error code = %v, want upstream_error", data["code"])
			}
			ev, done := rd.next()
			if ev != "done" || done["status"] != "error" {
				t.Fatalf("last event = %q %v, want done/error", ev, done)
			}
			resp.Body.Close()
			goto tripped
		case "done":
			t.Fatalf("stream finished %v without tripping the stall budget", data)
		}
	}
tripped:
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("stall cut took %v, want close to the 400ms budget", d)
	}
	if got.String() != "half an " {
		t.Errorf("client saw %q before the cut, want the pre-stall token", got.String())
	}

	// The partial answer is durable, under error status.
	m := waitForStatus(t, a.db, u.ID, msgID, "error")
	if m.Content != "half an " {
		t.Errorf("stored content = %q, want the partial answer", m.Content)
	}

	// The slot was released with the trip: a second send on the SAME
	// conversation completes normally.
	resp2 := sendChat(t, a, c.ID, "again")
	rd2 := newSSEReader(t, resp2)
	content, _, done2 := rd2.untilDone()
	resp2.Body.Close()
	if content != "second answer" || done2["status"] != "complete" {
		t.Errorf("second send = %q / %v, want second answer / complete", content, done2)
	}
}

func pieStr(data map[string]any) string {
	s, _ := data["t"].(string)
	return s
}

// The header budget rides the same rails before any byte arrives: no
// response headers, then a cut within the budget.
func TestChatSilentUpstreamHeaderBudget(t *testing.T) {
	var release <-chan struct{}
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // accept, then write nothing at all
	})
	release = stalledExit(t)

	cfg := chatCfg(t, fake.URL)
	cfg.LLM.UpstreamHeaderTimeout = config.Duration(300 * time.Millisecond)
	a := signedInChat(t, cfg)
	c := createConv(t, a, map[string]any{"title": "Silent"})
	u := ownerOf(t, a)

	resp := sendChat(t, a, c.ID, "hi")
	rd := newSSEReader(t, resp)
	_, ev0 := rd.next() // the local start event precedes any upstream byte
	msgID, _ := ev0["message_id"].(string)

	ev, data := rd.next()
	if ev != "error" || data["code"] != "upstream_error" {
		t.Fatalf("event = %q %v, want error/upstream_error", ev, data)
	}
	ev, done := rd.next()
	if ev != "done" || done["status"] != "error" {
		t.Fatalf("last event = %q %v, want done/error", ev, done)
	}
	resp.Body.Close()

	m := waitForStatus(t, a.db, u.ID, msgID, "error")
	if m.Content != "" {
		t.Errorf("stored content = %q, want empty", m.Content)
	}
}
