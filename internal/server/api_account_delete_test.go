package server

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mullsmith/internal/auth"
	"mullsmith/internal/store"
)

// POST /auth/me/delete — the self-service irreversible path (spec §5.1):
// the users row goes, every FK cascades with it (conversations → messages →
// tool calls/attachment links, sessions, auth_tokens), the scratchpad is
// removed immediately, and the session cookie is cleared in the response.
// Counts run through a separate READ-ONLY connection — the store's own
// handle keeps its internals private, and a second reader is the honest
// outside view anyway.

func TestDeleteAccountCascadesEverything(t *testing.T) {
	cfg := authCfg(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ro, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := ro.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(newPreparedServer(t, cfg, db, logger, "test", nil, nil).Handler())
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1") // the code a.register() presents
	if resp := a.register("doomed@test.io", "str0ng-passphrase-1", false); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d", resp.StatusCode)
	}
	ctx := context.Background()
	uid := decodeBody(t, a.do(http.MethodGet, "/api/v1/auth/me", nil))["id"].(string)

	// Seed the full footprint. Session row and the account's first
	// "New chat" came free with register (spec §8); a second chat and its
	// message via the store; a scratchpad file nested one level deep.
	conv := &store.Conversation{UserID: uid, Title: "secret chats"}
	if err := a.db.CreateConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if err := a.db.AddMessage(ctx, uid, &store.Message{
		ConversationID: conv.ID, Role: "user", Content: "the evidence",
	}); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(cfg.Scratchpad.Root, uid)
	if err := os.MkdirAll(filepath.Join(userDir, "work", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "work", "deep", "thing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Everything present BEFORE, so the zeros after mean something.
	if n := count(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, uid); n != 1 {
		t.Fatalf("sessions before = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM conversations WHERE user_id = ?`, uid); n != 2 {
		// one seeded by register + one added below
		t.Fatalf("conversations before = %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("messages before = %d", n)
	}

	resp := a.do(http.MethodPost, "/api/v1/auth/me/delete", map[string]any{})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("response must clear the session cookie (row + session are gone)")
	}

	// Nothing left in the database under any name: user, chats, messages,
	// sessions — the cascade chain is the whole point of this test.
	if u, err := a.db.UserByID(ctx, uid); err != nil || u != nil {
		t.Errorf("user survived delete: %v %v", u, err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM conversations WHERE user_id = ?`,
		`SELECT COUNT(*) FROM messages`,
		`SELECT COUNT(*) FROM sessions WHERE user_id = ?`,
		`SELECT COUNT(*) FROM auth_tokens WHERE user_id = ?`,
	} {
		if n := count(q, uid); n != 0 {
			t.Errorf("%s = %d rows after delete, want 0", q, n)
		}
	}

	// Scratchpad gone entirely, nested tree included.
	if _, err := os.Stat(userDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scratchpad dir survived: stat = %v", err)
	}

	// The stale cookie can do nothing further.
	resp = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after delete = %d, want 401", resp.StatusCode)
	}
}
