package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAbortGenerationsLetsShutdownFinish is the regression test for the
// SIGTERM overrun seen in the field: an in-flight SSE generation kept the
// request handler alive, http.Server.Shutdown timed out for the full grace
// period, and WaitIdle spent another full grace period waiting for a stream
// nobody was cancelling. AbortGenerations must drive every generation down
// the same finalize path as a user abort so both phases complete quickly.
func TestAbortGenerationsLetsShutdownFinish(t *testing.T) {
	llmStreaming := make(chan struct{})
	fake := newFakeLLM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if streaming, _ := m["stream"].(bool); !streaming { // titler
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hanging"}}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sseUp(w, deltaJSON(map[string]any{"content": "partial answer "}))
		close(llmStreaming)
		<-r.Context().Done() // hang until the generation itself is cancelled
	}))

	cfg := chatCfg(t, fake.URL)
	cfg.Tools.Agent.MaxToolRounds = 2
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := newPreparedServer(t, cfg, db, logger, "test", nil, nil)
	ts := httptest.NewServer(s.Handler())
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1") // register() replays exactly this code
	if resp := a.register("shut@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}

	c := createConv(t, a, map[string]any{"title": "hang"})
	resp := sendChat(t, a, c.ID, "go")
	rd := newSSEReader(t, resp)

	// Park the generation mid-stream: read events until one token landed.
	<-llmStreaming
	if got := s.chat.Generations.Count(); got != 1 {
		t.Fatalf("live generations = %d, want 1", got)
	}

	if n := s.AbortGenerations(); n != 1 {
		t.Fatalf("AbortGenerations = %d, want 1", n)
	}

	// The SSE stream must terminate on its own (done event), like Stop.
	deadline := time.After(10 * time.Second)
	next := make(chan string, 8)
	go func() {
		for {
			ev, _ := rd.next()
			select {
			case next <- ev:
			}
			if ev == "done" || ev == "error" {
				return
			}
		}
	}()
	for {
		select {
		case ev := <-next:
			if ev == "done" {
				goto drained
			}
			if ev == "error" {
				t.Fatalf("error event on abort: %v", ev)
			}
		case <-deadline:
			t.Fatal("stream never finished after AbortGenerations")
		}
	}
drained:
	if got := s.chat.Generations.Count(); got != 0 {
		t.Fatalf("generation still registered after abort: %d", got)
	}

	// With the stream gone, Shutdown has nothing to wait for.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown after aborts = %v, want nil", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Shutdown took %v, want near-instant after aborts", d)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	s.WaitIdle(wctx) // must return immediately, not on the deadline
}

func TestShutdownRefusesLateChatWithoutCreatingTurn(t *testing.T) {
	release := make(chan struct{})
	close(release)
	a, s := newBlockingChat(t, release, make(chan struct{}, 1))
	conv := createConv(t, a, map[string]any{"title": "late request"})
	s.AbortGenerations()
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/messages",
		map[string]any{"content": "must not be accepted"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("late chat status=%d body=%s", resp.StatusCode, dumpBody(resp))
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	if failure.Error.Code != "server_shutdown" {
		t.Fatalf("unexpected error: %+v", failure)
	}
	history, err := s.store.ContextHistory(t.Context(), conv.ID)
	if err != nil || len(history) != 0 {
		t.Fatalf("shutdown refusal created history: %+v %v", history, err)
	}
	if s.chat.Generations.Count() != 0 {
		t.Fatal("late chat retained a generation slot")
	}
}
