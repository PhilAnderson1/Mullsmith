package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mullsmith/internal/config"
)

// C8: the two upstream progress budgets. These are SILENCE budgets, never
// total stream timeouts — a reasoning run that keeps talking must survive
// however long it takes, while a silent endpoint (before or after headers)
// is cut and reported as a typed upstream failure.

// budgetClient wires one handler with explicit budgets. Passing
// -1 for a budget spells the house off-sentinel; 0 would read as
// "absent → code default", which these tests never want.
func budgetClient(t *testing.T, h http.HandlerFunc, header, stall time.Duration) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL + "/v1"
	cfg.LLM.Models = []config.LLMModel{{ID: "m", Default: true}}
	cfg.LLM.MaxTokensResponse = 64
	cfg.LLM.UpstreamHeaderTimeout = config.Duration(header)
	cfg.LLM.UpstreamStallTimeout = config.Duration(stall)
	return NewClient(cfg, nil)
}

const off = time.Duration(-1)

// released gives a never-returning handler a way out. httptest's Close is
// registered when the client is built; t.Cleanup runs LIFO, so a release
// registered AFTER that runs BEFORE it — the handler exits, the server
// closes. (The server does not observe the client's TCP close while a
// handler is parked without an in-flight read, so r.Context().Done() alone
// would deadlock Close.)
func released(t *testing.T) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx.Done()
}

func oneUserMsg() []ChatMessage {
	return []ChatMessage{{Role: "user", Content: "go"}}
}

// An endpoint that accepts the connection but never sends response headers
// must be cut by the header budget, quickly, as a typed upstream failure.
func TestHeaderBudgetTripsWhenHeadersNeverArrive(t *testing.T) {
	var release <-chan struct{}
	c := budgetClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the socket; write nothing at all
	}, 200*time.Millisecond, off)
	release = released(t) // registered after ts.Close → runs before it

	start := time.Now()
	err := c.StreamTools(context.Background(), "", oneUserMsg(), nil, nil,
		func(Delta) error { t.Error("no delta expected from a header-less endpoint"); return nil })
	if !errors.Is(err, ErrStreamHeaderTimeout) || !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrStreamHeaderTimeout wrapping ErrUpstream", err)
	}
	if errors.Is(err, ErrAborted) {
		t.Errorf("a budget trip is not a caller abort: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("trip took %v, want close to the 200ms budget", d)
	}
}

// After headers and one token, silence must trip the stall budget — and
// everything received up to the stall must already have been delivered
// (the server persists exactly what the deltas carried).
func TestStallBudgetTripsAfterTokens(t *testing.T) {
	var release <-chan struct{}
	c := budgetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial \"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // then nothing, forever
	}, off, 300*time.Millisecond)
	release = released(t)

	var got string
	start := time.Now()
	err := c.StreamTools(context.Background(), "", oneUserMsg(), nil, nil,
		func(d Delta) error { got += d.Content; return nil })
	if !errors.Is(err, ErrStreamStalled) || !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrStreamStalled wrapping ErrUpstream", err)
	}
	if got != "partial " {
		t.Errorf("content before the stall = %q, want it delivered to the caller", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("trip took %v, want close to the 300ms budget", d)
	}
}

// The documented policy: SSE COMMENT heartbeats count as transport
// progress. An endpoint that keeps the pipe talking — even with nothing
// but comments — is never stalled: each gap between bytes stays under the
// budget, though the wait for real content spans it many times over. (A
// policy that counted DATA only would trip here on the first gap after
// headers.)
func TestCommentHeartbeatsCountAsTransportProgress(t *testing.T) {
	c := budgetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			fmt.Fprint(w, ": keepalive\n\n")
			if f != nil {
				f.Flush()
			}
			time.Sleep(70 * time.Millisecond) // ~1.4 s of comments, 250 ms stall budget
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"finally\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}, off, 250*time.Millisecond)

	content, _, _, err := collect(t, c, oneUserMsg())
	if err != nil {
		t.Fatalf("comment heartbeats must keep the stream alive: %v", err)
	}
	if content != "finally" {
		t.Fatalf("content = %q, want finally", content)
	}
}

// Slow-but-valid reasoning: gaps under the budget, total runtime well over
// it. A total-timeout policy would kill this run; a progress budget must
// not.
func TestSlowButValidReasoningCompletes(t *testing.T) {
	c := budgetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think%d \"}}]}\n\n", i)
			if f != nil {
				f.Flush()
			}
			time.Sleep(80 * time.Millisecond) // 1.6 s total, every gap under 400 ms
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}, off, 400*time.Millisecond)

	_, reasoning, _, err := collect(t, c, oneUserMsg())
	if err != nil {
		t.Fatalf("a stream that keeps producing must not be cut: %v", err)
	}
	if reasoning == "" {
		t.Fatal("reasoning deltas lost")
	}
}

// The off-sentinel really removes the cap: a silence that a short budget
// would trip sails through when the budget is off.
func TestStallBudgetOffNeverTrips(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(600 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	t.Run("short budget trips", func(t *testing.T) {
		c := budgetClient(t, handler, off, 150*time.Millisecond)
		if _, _, _, err := collect(t, c, oneUserMsg()); !errors.Is(err, ErrStreamStalled) {
			t.Fatalf("err = %v, want ErrStreamStalled", err)
		}
	})
	t.Run("off sails through", func(t *testing.T) {
		c := budgetClient(t, handler, off, off)
		content, _, _, err := collect(t, c, oneUserMsg())
		if err != nil {
			t.Fatalf("off must impose no cap: %v", err)
		}
		if content != "ab" {
			t.Fatalf("content = %q, want ab", content)
		}
	})
}

// The budgets never impersonate the caller: a caller-side cancellation
// stays ErrAborted even while generous budgets are armed.
func TestCallerCancelStaysAbortedUnderBudgets(t *testing.T) {
	var release <-chan struct{}
	c := budgetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"mid\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}, 10*time.Second, 10*time.Second)
	release = released(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	err := c.StreamTools(ctx, "", oneUserMsg(), nil, nil, func(Delta) error { return nil })
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	if errors.Is(err, ErrStreamStalled) || errors.Is(err, ErrStreamHeaderTimeout) {
		t.Errorf("a caller abort must not read as a budget trip: %v", err)
	}
}
