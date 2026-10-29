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

	"mullsmith/internal/store"
)

// newWindowTest builds one Server with a canned windowProbe — installed
// after construction, so nothing is probed until a test calls
// refreshContextWindow — plus a signed-in session for meta reads.
func newWindowTest(t *testing.T, pinned int, probe func(context.Context) (int, bool)) (*authTest, *Server, *store.DB) {
	t.Helper()
	cfg := chatCfg(t, "http://127.0.0.1:1/v1") // endpoint unused: the probe is canned
	cfg.LLM.MaxContextTokens = pinned
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
	_, s, _ := newWindowTest(t, 5000, func(context.Context) (int, bool) {
		t.Error("a pinned config must never probe the endpoint")
		return 9999, true
	})
	s.refreshContextWindow()
	if got := s.contextWindow(); got != 5000 {
		t.Errorf("contextWindow() = %d, want the pinned 5000", got)
	}
	if got := s.contextWindowMeta(); got != int64(5000) {
		t.Errorf("meta window = %v, want 5000", got)
	}
}

func TestContextWindowAutoDiscovery(t *testing.T) {
	_, s, _ := newWindowTest(t, -1, func(context.Context) (int, bool) { return 9999, true })
	if got := s.contextWindow(); got != 0 {
		t.Fatalf("fresh auto window = %d, want unknown", got)
	}
	// meta waits (grace-capped) for the first probe, so even the first
	// read after startup carries the discovered number.
	if got := s.contextWindowMeta(); got != int64(9999) {
		t.Errorf("meta window = %v, want 9999 after discovery", got)
	}
}

func TestContextWindowUnknownIsNullMeta(t *testing.T) {
	// The dumb-proxy case: nothing reported → JSON null, gate open.
	_, s, _ := newWindowTest(t, -1, func(context.Context) (int, bool) { return 0, false })
	if got := s.contextWindowMeta(); got != nil {
		t.Errorf("meta window = %v, want nil while unresolved", got)
	}
	if s.conversationFull("whatever", "plenty of content") {
		t.Error("gate must be open while the window is unknown")
	}
}

func TestContextWindowRefreshIsAsyncWhenKnown(t *testing.T) {
	calls := make(chan struct{}, 8)
	_, s, _ := newWindowTest(t, -1, func(context.Context) (int, bool) {
		calls <- struct{}{}
		return 8000, true
	})
	s.refreshContextWindow() // unknown yet: this one waits
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh never probed")
	}
	if got := s.contextWindow(); got != 8000 {
		t.Fatalf("window = %d, want 8000", got)
	}
	start := time.Now()
	s.refreshContextWindow() // known now: must not block on the probe
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("refresh with a cached value blocked for %v", time.Since(start))
	}
}

func TestConversationFullGate(t *testing.T) {
	a, s, db := newWindowTest(t, 50, func(context.Context) (int, bool) { return 50, true })
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
	if s.conversationFull(c.ID, "hi") {
		t.Error("gate closed with room to spare")
	}
	// Cross the window with one big turn: the gate answers before the send.
	seed(strings.Repeat("x", 240)) // ~60 tokens
	if !s.conversationFull(c.ID, "hi") {
		t.Error("gate open with history past the window")
	}
	// A conversation the store cannot read never reads as full.
	if s.conversationFull("00NOTAREALID00", "hi") {
		t.Error("unknown conversation must not read as full")
	}
}
