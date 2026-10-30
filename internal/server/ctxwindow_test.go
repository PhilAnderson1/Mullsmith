package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/store"
)

// newWindowTest builds one Server with a canned windowProbe — installed
// after construction, so nothing is probed until a test refreshes — plus
// a signed-in session for conversation reads. entries override the model
// list when non-nil.
func newWindowTest(t *testing.T, probe func(context.Context) (map[string]int, bool), entries ...config.LLMModel) (*authTest, *Server, *store.DB) {
	t.Helper()
	cfg := chatCfg(t, "http://127.0.0.1:1/v1") // endpoint unused: the probe is canned
	if len(entries) > 0 {
		cfg.LLM.Models = entries
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := New(cfg, db, logger, "test", nil, nil)
	s.windowProbe = probe
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1") // register() replays exactly this code
	if resp := a.register("gate@b.io", "uncommon-password-9", false); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a, s, db
}

func TestContextWindowPinnedBeatsDiscovery(t *testing.T) {
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		t.Error("an all-pinned config must never probe the endpoint")
		return map[string]int{"m1": 9999}, true
	}, config.LLMModel{ID: "m1", Default: true, ContextSize: 5000})
	s.refreshContextWindows()
	if got := s.contextWindow("m1"); got != 5000 {
		t.Errorf("contextWindow(m1) = %d, want the pinned 5000", got)
	}
}

func TestContextWindowAutoDiscoveryPerModel(t *testing.T) {
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		return map[string]int{"m1": 9999, "m2": 4000}, true
	},
		config.LLMModel{ID: "m1", Default: true},
		config.LLMModel{ID: "m2"},
	)
	if got := s.contextWindow("m1"); got != 0 {
		t.Fatalf("fresh auto window = %d, want unknown", got)
	}
	s.refreshContextWindows() // unknown yet: this one waits
	if got := s.contextWindow("m1"); got != 9999 {
		t.Errorf("m1 window = %d, want 9999 after discovery", got)
	}
	if got := s.contextWindow("m2"); got != 4000 {
		t.Errorf("m2 window = %d, want 4000 after discovery", got)
	}
	// Listing spelling differs in case: the cache keys lower-cased.
	if got := s.contextWindow("M1"); got != 9999 {
		t.Errorf("case-insensitive lookup = %d, want 9999", got)
	}
}

func TestContextWindowUnknownIsNull(t *testing.T) {
	// The dumb-proxy case: nothing reported → unlimited, gate open.
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) { return nil, false })
	if got := s.contextWindow("m1"); got != 0 {
		t.Errorf("window = %d, want 0 while unresolved", got)
	}
	if s.conversationFull("whatever", "m1", "plenty of content") {
		t.Error("gate must be open while the window is unknown")
	}
}

func TestContextWindowRefreshIsAsyncWhenKnown(t *testing.T) {
	calls := make(chan struct{}, 8)
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		calls <- struct{}{}
		return map[string]int{"m1": 8000}, true
	})
	s.refreshContextWindows() // unknown yet: this one waits
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh never probed")
	}
	if got := s.contextWindow("m1"); got != 8000 {
		t.Fatalf("window = %d, want 8000", got)
	}
	start := time.Now()
	s.refreshContextWindows() // known now: must not block on the probe
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("refresh with a cached value blocked for %v", time.Since(start))
	}
}

// A conversation's payload carries the window of ITS model — the meter's
// denominator travels with the conversation, not with meta.
func TestConversationCarriesItsModelsWindow(t *testing.T) {
	a, _, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		return map[string]int{"m1": 9999}, true
	},
		config.LLMModel{ID: "m1", Default: true},
		config.LLMModel{ID: "m2", ContextSize: 6000},
	)
	c := createConv(t, a, map[string]any{"title": "win"})
	var proj map[string]any
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj["max_context_tokens"] != float64(9999) {
		t.Fatalf("default-model conversation window = %v, want discovered 9999", proj["max_context_tokens"])
	}
	patchResp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID, map[string]any{"model": "m2"})
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch model: %d", patchResp.StatusCode)
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj["max_context_tokens"] != float64(6000) {
		t.Errorf("m2 conversation window = %v, want the pinned 6000", proj["max_context_tokens"])
	}
}

func TestConversationFullGate(t *testing.T) {
	a, s, db := newWindowTest(t, func(context.Context) (map[string]int, bool) { return map[string]int{"m1": 50}, true },
		config.LLMModel{ID: "m1", Default: true, ContextSize: 50})
	u, err := db.UserByEmail(context.Background(), "gate@b.io")
	if err != nil || u == nil {
		t.Fatalf("owner lookup: %v", err)
	}
	c := createConv(t, a, map[string]any{"title": "gate"})

	seed := func(content string) {
		t.Helper()
		m := &store.Message{ConversationID: c.ID, Role: "assistant", Content: content, Status: "complete"}
		if err := db.AddMessage(context.Background(), u.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	// ~10 tokens of history against a 50-token window: space to spare.
	seed(strings.Repeat("y", 40))
	if s.conversationFull(c.ID, "m1", "hi") {
		t.Error("gate closed with room to spare")
	}
	// Cross the window with one big turn: the gate answers before the send.
	seed(strings.Repeat("x", 240)) // ~60 tokens
	if !s.conversationFull(c.ID, "m1", "hi") {
		t.Error("gate open with history past the window")
	}
	// A conversation the store cannot read never reads as full.
	if s.conversationFull("00NOTAREALID00", "m1", "hi") {
		t.Error("unknown conversation must not read as full")
	}
}
