package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gabkeep/internal/store"
)

// signedIn returns an authTest with one registered+logged-in user.
func signedIn(t *testing.T) *authTest {
	t.Helper()
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	if resp := a.register("owner@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d", resp.StatusCode)
	}
	return a
}

type convResp struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
}

func createConv(t *testing.T, a *authTest, body map[string]any) convResp {
	t.Helper()
	resp := a.do(http.MethodPost, "/api/v1/conversations", body)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, dumpBody(resp))
	}
	var c convResp
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return c
}

func getJSON(t *testing.T, a *authTest, path string, dst any) {
	t.Helper()
	resp := a.do(http.MethodGet, path, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, dumpBody(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestConversationsRequireAuth(t *testing.T) {
	a := newAuthTest(t, authCfg(t)) // no login
	resp := a.do(http.MethodGet, "/api/v1/conversations", nil)
	if resp.StatusCode != 401 || apiErrCode(t, resp) != "unauthenticated" {
		t.Errorf("unauth list: %d", resp.StatusCode)
	}
	resp = a.do(http.MethodPost, "/api/v1/conversations", map[string]any{})
	if resp.StatusCode != 401 {
		t.Errorf("unauth create: %d", resp.StatusCode)
	}
}

func TestConversationLifecycle(t *testing.T) {
	a := signedIn(t)

	c := createConv(t, a, map[string]any{"title": "Trip planning"})
	if c.ID == "" || c.Title != "Trip planning" {
		t.Fatalf("create body = %+v", c)
	}
	// Omitted fields take defaults.
	d := createConv(t, a, map[string]any{})
	if d.Title != "New chat" || d.Model != "" {
		t.Errorf("defaults = %+v", d)
	}

	var list struct {
		Items      []convResp `json:"items"`
		NextCursor string     `json:"next_cursor"`
	}
	getJSON(t, a, "/api/v1/conversations", &list)
	if len(list.Items) != 2 {
		t.Fatalf("list = %+v", list.Items)
	}

	// Rename via PATCH.
	var patched convResp
	resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID, map[string]any{"title": "Renamed trip"})
	patchedResp := decodeBody(t, resp)
	patched.ID, _ = patchedResp["id"].(string)
	patched.Title, _ = patchedResp["title"].(string)
	if resp.StatusCode != 200 || patched.Title != "Renamed trip" {
		t.Fatalf("patch = %d %v", resp.StatusCode, patchedResp)
	}

	// Delete → 204; gone from GET, list and re-delete.
	resp = a.do(http.MethodDelete, "/api/v1/conversations/"+c.ID, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp = a.do(http.MethodGet, "/api/v1/conversations/"+c.ID, nil)
	if resp.StatusCode != 404 || apiErrCode(t, resp) != "not_found" {
		t.Errorf("get after delete: %d", resp.StatusCode)
	}
	resp = a.do(http.MethodDelete, "/api/v1/conversations/"+c.ID, nil)
	if resp.StatusCode != 404 {
		t.Errorf("re-delete: %d", resp.StatusCode)
	}
	getJSON(t, a, "/api/v1/conversations", &list)
	if len(list.Items) != 1 || list.Items[0].ID != d.ID {
		t.Errorf("list after delete = %+v", list.Items)
	}
}

func TestConversationValidation(t *testing.T) {
	a := signedIn(t)

	cases := []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"model": "not-a-real-model"}, "invalid_model"},
		{map[string]any{"title": strings.Repeat("t", 201)}, "title_too_long"},
		{map[string]any{"system_prompt": strings.Repeat("s", 8001)}, "system_prompt_too_long"},
		{map[string]any{"bogus": 1}, "invalid_request"},
	}
	for i, tc := range cases {
		resp := a.do(http.MethodPost, "/api/v1/conversations", tc.body)
		if resp.StatusCode >= 300 && apiErrCode(t, resp) != tc.code {
			t.Errorf("create case %d: want %s, got %d", i, tc.code, resp.StatusCode)
		} else if resp.StatusCode < 400 {
			t.Errorf("create case %d accepted: %v", i, tc.body)
		}
	}

	// Valid model from config passes (testCfg's LLM.Models = ["m1"]).
	if c := createConv(t, a, map[string]any{"model": "m1"}); c.Model != "m1" {
		t.Errorf("model roundtrip = %+v", c)
	}

	// PATCH: empty object → no_fields; blank title rejected; unknown id 404.
	resp := a.do(http.MethodPatch, "/api/v1/conversations/doesnotexist000000000000", map[string]any{"title": "x"})
	if resp.StatusCode != 404 {
		t.Errorf("patch unknown: %d", resp.StatusCode)
	}
	c := createConv(t, a, map[string]any{"title": "Ok"})
	resp = a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID, map[string]any{})
	if resp.StatusCode != 400 || apiErrCode(t, resp) != "no_fields" {
		t.Errorf("empty patch: %d", resp.StatusCode)
	}
	resp = a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID, map[string]any{"title": "   "})
	if resp.StatusCode != 400 {
		t.Errorf("blank title patch: %d", resp.StatusCode)
	}
}

func TestConversationCrossUserIsolation(t *testing.T) {
	a := signedIn(t)
	c := createConv(t, a, map[string]any{"title": "Private"})

	// Second user, separate server-side account but same store: created
	// directly; the API must treat user A's id as nonexistent for user B.
	ctx := context.Background()
	b := mkStoreUser(t, a, "intruder@b.io")
	resp := a.do(http.MethodGet, "/api/v1/conversations/"+c.ID, nil)
	if resp.StatusCode != 200 { // owner still fine
		t.Fatalf("owner get: %d", resp.StatusCode)
	}
	// Ask as B using forged requests is impossible without B's session;
	// instead assert the store-level guard the handlers rely on.
	if _, err := a.db.ConversationForUser(ctx, b.ID, c.ID); err != store.ErrConversationNotFound {
		t.Errorf("isolation: got %v, want ErrConversationNotFound", err)
	}
	if err := a.db.SoftDeleteConversation(ctx, b.ID, c.ID); err != store.ErrConversationNotFound {
		t.Errorf("isolation delete: got %v", err)
	}
}

func mkStoreUser(t *testing.T, a *authTest, email string) *store.User {
	t.Helper()
	now := time.Now()
	u := &store.User{Email: email, Name: "X", PasswordHash: "x", Status: "active",
		CreatedAt: now, UpdatedAt: now}
	if err := a.db.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestConversationsPaginationAPI(t *testing.T) {
	a := signedIn(t)
	for i := 0; i < 5; i++ {
		createConv(t, a, map[string]any{"title": "c"})
	}
	var page1 struct {
		Items      []convResp `json:"items"`
		NextCursor string     `json:"next_cursor"`
	}
	getJSON(t, a, "/api/v1/conversations?limit=2", &page1)
	if len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("page1 = %+v", page1)
	}
	var page2 struct {
		Items      []convResp `json:"items"`
		NextCursor string     `json:"next_cursor"`
	}
	getJSON(t, a, "/api/v1/conversations?limit=2&cursor="+page1.NextCursor, &page2)
	if len(page2.Items) != 2 || page2.NextCursor == "" {
		t.Fatalf("page2 = %+v", page2)
	}
	if page2.Items[0].ID == page1.Items[0].ID {
		t.Error("cursor returned the same page")
	}
	resp := a.do(http.MethodGet, "/api/v1/conversations?cursor=zzzz!!bad", nil)
	if resp.StatusCode != 400 || apiErrCode(t, resp) != "invalid_cursor" {
		t.Errorf("bad cursor: %d", resp.StatusCode)
	}
}

func TestConversationMessagesAPI(t *testing.T) {
	a := signedIn(t)
	c := createConv(t, a, map[string]any{"title": "Thread"})
	ctx := context.Background()

	owner, err := a.db.UserByEmail(ctx, "owner@b.io")
	if err != nil || owner == nil {
		t.Fatal("owner lookup")
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 4; i++ {
		m := &store.Message{ConversationID: c.ID, Role: []string{"user", "assistant"}[i%2],
			Content: string(rune('A' + i)), CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := a.db.AddMessage(ctx, owner.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	var page struct {
		Items []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages?limit=2", &page)
	if len(page.Items) != 2 || page.Items[0].Content != "C" || page.Items[1].Content != "D" {
		t.Fatalf("messages page = %+v", page.Items)
	}
	if page.Items[0].Role != "user" || page.Items[0].Status != "complete" {
		t.Errorf("message fields = %+v", page.Items[0])
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages?limit=2&cursor="+page.NextCursor, &page)
	if len(page.Items) != 2 || page.Items[0].Content != "A" || page.NextCursor != "" {
		t.Fatalf("older page = %+v %q", page.Items, page.NextCursor)
	}

	// Unknown conversation → 404 envelope, not an auth error.
	resp := a.do(http.MethodGet, "/api/v1/conversations/0000000000000000000000/messages", nil)
	if resp.StatusCode != 404 || apiErrCode(t, resp) != "not_found" {
		t.Errorf("messages unknown conv: %d", resp.StatusCode)
	}
}

func TestStrictContentType(t *testing.T) {
	a := signedIn(t)
	req, _ := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/conversations",
		strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Gabkeep-CSRF", a.csrf)
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnsupportedMediaType || apiErrCode(t, resp) != "unsupported_media_type" {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
	// With charset suffix it is accepted.
	req, _ = http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/conversations",
		strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-Gabkeep-CSRF", a.csrf)
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err = a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("charset CT: %d, want 201", resp.StatusCode)
	}
}
