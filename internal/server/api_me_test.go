package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"gabkeep/internal/id"
	"gabkeep/internal/store"
)

// Settings page backend contract (spec §5): the account row owns a default
// system prompt; /auth/me exposes and updates it; new chats inherit it.

func TestMeUpdateProfileRoundTrip(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	if resp := a.register("me@test.io", "str0ng-passphrase-1", false); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d", resp.StatusCode)
	}

	body := decodeBody(t, a.do(http.MethodGet, "/api/v1/auth/me", nil))
	if body["default_system_prompt"] != "" {
		t.Fatalf("fresh user default prompt = %v, want empty", body["default_system_prompt"])
	}

	resp := a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{
		"name":                  "Philip A",
		"email":                 "PHIL@Example.IO",
		"default_system_prompt": "Answer in iambic pentameter.",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH me = %d", resp.StatusCode)
	}
	updated := decodeBody(t, resp)
	if updated["name"] != "Philip A" || updated["email"] != "phil@example.io" {
		t.Errorf("PATCH me = %v/%v, want renamed + lowercased email", updated["name"], updated["email"])
	}
	if updated["default_system_prompt"] != "Answer in iambic pentameter." {
		t.Errorf("default prompt not stored: %v", updated["default_system_prompt"])
	}
	// The session survived an email change and still resolves the user.
	again := decodeBody(t, a.do(http.MethodGet, "/api/v1/auth/me", nil))
	if again["id"] != body["id"] || again["default_system_prompt"] != "Answer in iambic pentameter." {
		t.Errorf("GET me after update = %v", again)
	}
}

func TestMeUpdateValidatesAndReservesEmails(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	a.register("first@test.io", "str0ng-passphrase-1", false)

	resp := a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"email": "not-an-email"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad email = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if resp := a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"name": " "}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blank name = %d, want 400", resp.StatusCode)
		resp.Body.Close()
	}
	if resp := a.do(http.MethodPatch, "/api/v1/auth/me",
		map[string]any{"default_system_prompt": strings.Repeat("x", maxSystemPromptLen+1)}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized prompt = %d, want 400", resp.StatusCode)
		resp.Body.Close()
	}
	// A partial save must not clobber fields that were not sent.
	if resp := a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"name": "Only Name"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("partial PATCH = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	body := decodeBody(t, a.do(http.MethodGet, "/api/v1/auth/me", nil))
	if body["name"] != "Only Name" || body["email"] != "first@test.io" {
		t.Errorf("partial PATCH damaged the row: %v", body)
	}
}

func TestMeUpdateRejectsTakenEmail(t *testing.T) {
	cfg := authCfg(t)
	ts, db := newTestServerWithDB(t, cfg)
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("owner@test.io", "str0ng-passphrase-1", false); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register owner = %d", resp.StatusCode)
	}
	// Second account (fresh jar shares the same server/db pair).
	b := newAuthTestOn(t, ts, db)
	b.invite("gk-code-2")
	resp2 := b.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "intruder@test.io", "name": "Intruder",
		"password": "str0ng-passphrase-2", "invite": "gk-code-2",
	})
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("register intruder = %d", resp2.StatusCode)
	}
	resp := b.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"email": "OWNER@test.io"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("steal email = %d, want 409", resp.StatusCode)
	}
	decodeBody(t, resp)
}

func TestNewConversationsSeedDefaultPrompt(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	a.register("seed@test.io", "str0ng-passphrase-1", false)
	if resp := a.do(http.MethodPatch, "/api/v1/auth/me",
		map[string]any{"default_system_prompt": "Be terse."}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH me = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// No explicit prompt → inherits the user default.
	created := decodeBody(t, a.do(http.MethodPost, "/api/v1/conversations", map[string]any{}))
	seededID := created["id"].(string)
	if created["system_prompt"] != "Be terse." {
		t.Errorf("new chat prompt = %v, want the default", created["system_prompt"])
	}
	// Explicit prompt wins — the default never overwrites a real choice.
	created = decodeBody(t, a.do(http.MethodPost, "/api/v1/conversations",
		map[string]any{"system_prompt": "Be verbose."}))
	if created["system_prompt"] != "Be verbose." {
		t.Errorf("explicit prompt = %v, want it kept", created["system_prompt"])
	}
	// Editing the default later must not rewrite the already-seeded chat.
	if resp := a.do(http.MethodPatch, "/api/v1/auth/me",
		map[string]any{"default_system_prompt": "Be silent."}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH me 2 = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	first := decodeBody(t, a.do(http.MethodGet, "/api/v1/conversations/"+seededID, nil))
	if first["system_prompt"] != "Be terse." {
		t.Errorf("seeded chat prompt changed: %v", first["system_prompt"])
	}
}

func TestTouchConversationReordersList(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	a.register("touch@test.io", "str0ng-passphrase-1", false)
	me := decodeBody(t, a.do(http.MethodGet, "/api/v1/auth/me", nil))
	uid := me["id"].(string)

	ctx := context.Background()
	now := time.Now()
	older := &store.Conversation{ID: id.New(), UserID: uid, Title: "older",
		CreatedAt: now.Add(-3 * time.Hour)}
	newer := &store.Conversation{ID: id.New(), UserID: uid, Title: "newer",
		CreatedAt: now.Add(-2 * time.Hour)}
	if err := a.db.CreateConversation(ctx, older); err != nil {
		t.Fatal(err)
	}
	if err := a.db.CreateConversation(ctx, newer); err != nil {
		t.Fatal(err)
	}
	// Activity ordering: the more recently touched chat leads.
	list := decodeBody(t, a.do(http.MethodGet, "/api/v1/conversations", nil))
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("list = %v", list)
	}
	if items[0].(map[string]any)["id"] != newer.ID {
		t.Errorf("newer activity should lead: %v", items[0])
	}
	// Touching the older chat moves it to the top without any message.
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+older.ID+"/touch", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("touch = %d, want 204", resp.StatusCode)
	}
	list = decodeBody(t, a.do(http.MethodGet, "/api/v1/conversations", nil))
	items = list["items"].([]any)
	if items[0].(map[string]any)["id"] != older.ID {
		t.Errorf("touch did not promote the chat: %v", items[0])
	}
	// Unknown id → 404 (and never another user's row: touch rides user_id).
	if resp := a.do(http.MethodPost, "/api/v1/conversations/nope/touch", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("touch unknown = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}
