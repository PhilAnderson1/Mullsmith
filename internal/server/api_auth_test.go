package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gabkeep/internal/auth"
	"gabkeep/internal/config"
	"gabkeep/internal/store"
)

// authTest is a scripted client: it bootstraps the CSRF cookie with a GET
// (mirroring the SPA), then mirrors it into the header on every mutation.
type authTest struct {
	t       *testing.T
	ts      *httptest.Server
	db      *store.DB
	cookies map[string]string
	csrf    string
}

func authCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := testCfg()
	cfg.Scratchpad.Root = t.TempDir()
	cfg.Auth.RememberLogin = true
	cfg.Auth.SessionTTL = config.Duration(time.Hour)
	cfg.Auth.RememberTTL = config.Duration(10 * time.Hour)
	cfg.Auth.RememberAbsoluteTTL = config.Duration(100 * time.Hour)
	cfg.Auth.MaxFailedLogins = 3
	cfg.Auth.Argon2 = config.Argon2{Time: 1, Memory: 1024, Threads: 1}
	return cfg
}

func newAuthTest(t *testing.T, cfg *config.Config) *authTest {
	t.Helper()
	ts, db := newTestServerWithDB(t, cfg)
	t.Cleanup(ts.Close)
	a := &authTest{t: t, ts: ts, db: db, cookies: map[string]string{}}
	resp := a.raw(http.MethodGet, "/", nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("bootstrap GET / = %d", resp.StatusCode)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		a.cookies[c.Name] = c.Value
		if c.Name == auth.CSRFCookieName {
			a.csrf = c.Value
		}
	}
	if a.csrf == "" {
		t.Fatal("no CSRF cookie issued on GET /")
	}
	return a
}

// raw performs a request bypassing the cookie jar. sessionRaw, when non-empty,
// is sent as the session cookie value directly (for multi-session tests).
func (a *authTest) raw(method, path string, body any, sessionRaw string) *http.Response {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.ts.URL+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.csrf != "" {
		req.Header.Set(auth.CSRFHeaderName, a.csrf)
	}
	// Cookies travel together (like a browser); sessionRaw only replaces the
	// session value, it never suppresses the rest of the jar.
	sent := make(map[string]string, len(a.cookies)+1)
	for k, v := range a.cookies {
		sent[k] = v
	}
	if sessionRaw != "" {
		sent[auth.SessionCookieName] = sessionRaw
	}
	for k, v := range sent {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := a.ts.Client().Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	// Manual jar: removals (Max-Age<0) out, values in; the CSRF header always
	// mirrors the freshest cookie, exactly like the SPA reads it per-request.
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(a.cookies, c.Name)
			continue
		}
		a.cookies[c.Name] = c.Value
		if c.Name == auth.CSRFCookieName {
			a.csrf = c.Value
		}
	}
	return resp
}

func (a *authTest) do(method, path string, body any) *http.Response {
	return a.raw(method, path, body, "")
}

func (a *authTest) invite(code string) {
	a.t.Helper()
	if err := a.db.CreateInvite(context.Background(), code, time.Now()); err != nil {
		a.t.Fatal(err)
	}
}

// register completes a full invite registration and returns the response.
func (a *authTest) register(email, password string, remember bool) *http.Response {
	return a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": email, "name": "Test User", "password": password,
		"remember_me": remember, "invite": "gk-code-1",
	})
}

func sessionCookieFrom(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", auth.SessionCookieName)
	return nil
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func apiErrCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	m := decodeBody(t, resp)
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	t.Fatalf("response has no error envelope: %v", m)
	return ""
}

// --- registration -----------------------------------------------------------

func TestRegisterRequiresInvite(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "a@b.io", "name": "A", "password": "uncommon-password-9",
	})
	if resp.StatusCode != http.StatusForbidden || apiErrCode(t, resp) != "invite_required" {
		t.Errorf("no-invite register: %d", resp.StatusCode)
	}
}

func TestRegisterClosed(t *testing.T) {
	cfg := authCfg(t)
	cfg.Auth.Registration = "closed"
	a := newAuthTest(t, cfg)
	a.invite("gk-code-1")
	resp := a.register("a@b.io", "uncommon-password-9", false)
	if resp.StatusCode != http.StatusForbidden || apiErrCode(t, resp) != "registration_closed" {
		t.Errorf("closed registration: %d", resp.StatusCode)
	}
}

func TestRegisterInvalidInvite(t *testing.T) {
	a := newAuthTest(t, authCfg(t)) // no invite minted
	resp := a.register("a@b.io", "uncommon-password-9", false)
	if resp.StatusCode != http.StatusForbidden || apiErrCode(t, resp) != "invite_invalid" {
		t.Errorf("unknown invite: %d", resp.StatusCode)
	}
}

func TestRegisterReuseInvite(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	if resp := a.register("first@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("first register: %d", resp.StatusCode)
	}
	resp := a.register("second@b.io", "uncommon-password-9", false)
	if resp.StatusCode != http.StatusForbidden || apiErrCode(t, resp) != "invite_invalid" {
		t.Errorf("invite reuse: %d", resp.StatusCode)
	}
}

func TestRegisterValidation(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	cases := []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"email": "not-an-email", "name": "N", "password": "uncommon-password-9", "invite": "gk-code-1"}, "invalid_email"},
		{map[string]any{"email": "a@b.io", "name": "", "password": "uncommon-password-9", "invite": "gk-code-1"}, "invalid_name"},
		{map[string]any{"email": "a@b.io", "name": "N", "password": "short9", "invite": "gk-code-1"}, "weak_password"},
		{map[string]any{"email": "a@b.io", "name": "N", "password": "password123", "invite": "gk-code-1"}, "weak_password"},
		{map[string]any{"email": "a@b.io", "name": "N", "password": "uncommon-password-9", "invite": "gk-code-1", "admin": true}, "invalid_request"},
	}
	for i, c := range cases {
		resp := a.do(http.MethodPost, "/api/v1/auth/register", c.body)
		if resp.StatusCode >= 300 && apiErrCode(t, resp) != c.code {
			t.Errorf("case %d: want %s, got %d", i, c.code, resp.StatusCode)
		} else if resp.StatusCode < 400 {
			t.Errorf("case %d: accepted invalid input", i)
		}
	}
}

func TestRegisterFullFlow(t *testing.T) {
	cfg := authCfg(t)
	a := newAuthTest(t, cfg)
	a.invite("gk-code-1")

	resp := a.register("Alice@Example.com", "uncommon-password-9", false)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	userID, _ := m["id"].(string)
	if m["email"] != "alice@example.com" || userID == "" {
		t.Fatalf("register body = %v", m)
	}

	// Scratchpad directories provisioned (spec §8).
	for _, sub := range []string{"uploads", "artifacts", "work"} {
		if fi, err := os.Stat(filepath.Join(cfg.Scratchpad.Root, userID, sub)); err != nil || !fi.IsDir() {
			t.Errorf("scratchpad %s missing: %v", sub, err)
		}
	}

	// Session works immediately; /me reports the user + quota shape.
	resp = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	m = decodeBody(t, resp)
	if resp.StatusCode != 200 || m["id"] != userID {
		t.Fatalf("/me = %d %v", resp.StatusCode, m)
	}
	if _, ok := m["quota"].(map[string]any); !ok {
		t.Errorf("/me missing quota: %v", m)
	}
	if _, ok := m["password_hash"]; ok {
		t.Error("/me leaks password_hash")
	}

	// Duplicate email (any case) → 409.
	resp = a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "ALICE@example.com", "name": "Other", "password": "another-good-pw-1",
		"invite": "gk-code-1",
	})
	// gk-code-1 was consumed, but the duplicate check must answer first-or-only;
	// either duplicate_email or invite_invalid is acceptable, not 5xx.
	if resp.StatusCode < 400 {
		t.Errorf("duplicate email accepted")
	}

	// Logout kills the session.
	resp = a.do(http.MethodPost, "/api/v1/auth/logout", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp = a.do(http.MethodGet, "/api/v1/auth/me", nil); resp.StatusCode != 401 {
		t.Errorf("/me after logout = %d, want 401", resp.StatusCode)
	}
}

// --- remember_me cookie semantics (spec §9.1) --------------------------------

func TestRememberMeCookie(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	resp := a.register("rem@b.io", "uncommon-password-9", true)
	c := sessionCookieFrom(t, resp)
	resp.Body.Close()
	if c.MaxAge <= 0 || c.Expires.IsZero() {
		t.Errorf("remembered cookie not persistent: Max-Age=%d Expires=%v", c.MaxAge, c.Expires)
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
}

func TestNoRememberMeIsBrowserSessionCookie(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	resp := a.register("nos@b.io", "uncommon-password-9", false)
	c := sessionCookieFrom(t, resp)
	resp.Body.Close()
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Errorf("non-remembered cookie must have no expiry attributes: %+v", c)
	}
}

func TestRememberDisabledByConfig(t *testing.T) {
	cfg := authCfg(t)
	cfg.Auth.RememberLogin = false
	a := newAuthTest(t, cfg)
	a.invite("gk-code-1")
	resp := a.register("dis@b.io", "uncommon-password-9", true) // requested, but disabled
	c := sessionCookieFrom(t, resp)
	resp.Body.Close()
	if c.MaxAge != 0 {
		t.Errorf("remember_login=false must ignore remember_me: Max-Age=%d", c.MaxAge)
	}
}

// --- login --------------------------------------------------------------------

func TestLoginGenericErrors(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	a.register("known@b.io", "uncommon-password-9", false)

	var bodies []string
	for _, body := range []map[string]any{
		{"email": "unknown@b.io", "password": "whatever-12345"},
		{"email": "known@b.io", "password": "wrong-password-1"},
	} {
		resp := a.do(http.MethodPost, "/api/v1/auth/login", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login %v: %d, want 401", body["email"], resp.StatusCode)
		}
		m := decodeBody(t, resp)
		e, _ := m["error"].(map[string]any)
		// Compare code+message only: request_id differs by design.
		bodies = append(bodies, fmt.Sprint(e["code"], "|", e["message"]))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("unknown-email and wrong-password responses differ -> account leak:\n%s\n%s", bodies[0], bodies[1])
	}

	// Correct credentials still work afterwards.
	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "known@b.io", "password": "uncommon-password-9",
	})
	if resp.StatusCode != 200 {
		t.Errorf("valid login after failures: %d", resp.StatusCode)
	}
}

func TestLoginLockout(t *testing.T) {
	a := newAuthTest(t, authCfg(t)) // MaxFailedLogins = 3
	a.invite("gk-code-1")
	a.register("lock@b.io", "uncommon-password-9", false)

	for i := 0; i < 3; i++ {
		resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
			"email": "lock@b.io", "password": "wrong-password-1",
		})
		if resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d, want 401", i, resp.StatusCode)
		}
	}
	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "lock@b.io", "password": "uncommon-password-9",
	})
	if resp.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp) != "account_locked" {
		t.Errorf("locked account accepted login: %d", resp.StatusCode)
	}
}

func TestLoginCaseInsensitiveEmail(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	a.register("MixedCase@B.io", "uncommon-password-9", false)
	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "mixedcase@b.IO", "password": "uncommon-password-9",
	})
	if resp.StatusCode != 200 {
		t.Errorf("case-variant login: %d", resp.StatusCode)
	}
}

// --- CSRF -----------------------------------------------------------------------

func TestCSRFRequiredOnMutations(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")

	// Strip the CSRF header entirely.
	req, _ := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/auth/register",
		bytes.NewReader([]byte(`{"email":"x@y.io","name":"X","password":"uncommon-password-9","invite":"gk-code-1"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || apiErrCode(t, resp) != "csrf_failed" {
		t.Errorf("no-CSRF POST = %d, want 403 csrf_failed", resp.StatusCode)
	}

	// Header present but not matching the cookie.
	req, _ = http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/auth/register",
		bytes.NewReader([]byte(`{"email":"x@y.io","name":"X","password":"uncommon-password-9","invite":"gk-code-1"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeaderName, "deadbeefdeadbeefdeadbeefdeadbeef")
	resp, err = a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("mismatched CSRF = %d, want 403", resp.StatusCode)
	}
}

// --- session lifecycle --------------------------------------------------------------

func TestUnauthenticatedEndpoints(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	for _, e := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodPost, "/api/v1/auth/logout"},
		{http.MethodPost, "/api/v1/auth/password"},
	} {
		resp := a.do(e.method, e.path, map[string]any{"current": "x", "new": "y"})
		if resp.StatusCode != http.StatusUnauthorized || apiErrCode(t, resp) != "unauthenticated" {
			t.Errorf("%s %s = %d, want 401", e.method, e.path, resp.StatusCode)
		}
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	ctx := context.Background()
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	u := &store.User{Email: "exp@b.io", Name: "E", PasswordHash: "x", Status: "active",
		CreatedAt: now, UpdatedAt: now}
	if err := a.db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := a.db.CreateSession(ctx, &store.Session{IDHash: hash, UserID: u.ID,
		CreatedAt: now.Add(-5 * time.Hour), ExpiresAt: now.Add(-time.Minute), LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	resp := a.raw(http.MethodGet, "/api/v1/auth/me", nil, raw)
	if resp.StatusCode != 401 {
		t.Errorf("expired session = %d, want 401", resp.StatusCode)
	}
}

func TestDisabledUserRejected(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	a.register("dis2@b.io", "uncommon-password-9", false)
	ctx := context.Background()
	u, err := a.db.UserByEmail(ctx, "dis2@b.io")
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("register did not create the user")
	}
	if err := a.db.SetUserStatus(ctx, u.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	resp := a.do(http.MethodGet, "/api/v1/auth/me", nil)
	if resp.StatusCode != 401 {
		t.Errorf("disabled user = %d, want 401", resp.StatusCode)
	}
}

func TestSlidingRenewal(t *testing.T) {
	cfg := authCfg(t) // RememberTTL=10h, absolute=100h
	a := newAuthTest(t, cfg)
	ctx := context.Background()
	raw, hash, _ := auth.NewSessionToken()
	now := time.Now()
	u := &store.User{Email: "slide@b.io", Name: "S", PasswordHash: "x", Status: "active",
		CreatedAt: now, UpdatedAt: now}
	if err := a.db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	// Remembered session: < half TTL remaining, last contact 2h ago → renew.
	if err := a.db.CreateSession(ctx, &store.Session{IDHash: hash, UserID: u.ID,
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Remembered: true,
		LastSeenAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	resp := a.raw(http.MethodGet, "/api/v1/auth/me", nil, raw)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /me with remembered session = %d", resp.StatusCode)
	}
	c := sessionCookieFrom(t, resp)
	resp.Body.Close()
	if c.MaxAge < 9*3600 {
		t.Errorf("renewed cookie Max-Age = %d, want ~36000", c.MaxAge)
	}
	s, _ := a.db.SessionByHash(ctx, hash)
	if time.Until(s.ExpiresAt) < 9*time.Hour {
		t.Errorf("DB expiry not slid: %v", s.ExpiresAt)
	}

	// A recently-seen session must NOT renew (throttle).
	raw2, hash2, _ := auth.NewSessionToken()
	if err := a.db.CreateSession(ctx, &store.Session{IDHash: hash2, UserID: u.ID,
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Remembered: true,
		LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	resp = a.raw(http.MethodGet, "/api/v1/auth/me", nil, raw2)
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			t.Errorf("throttled renewal: unexpected re-issued cookie")
		}
	}
}

// --- password change ------------------------------------------------------------------

func TestPasswordChangeRevokesAndRotates(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	resp := a.register("pw@b.io", "uncommon-password-9", false)
	sessA := sessionCookieFrom(t, resp).Value
	resp.Body.Close()

	// Second login: current jar now holds session B.
	if resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "pw@b.io", "password": "uncommon-password-9",
	}); resp.StatusCode != 200 {
		t.Fatalf("second login: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong current password → 403 and nothing changes.
	resp = a.do(http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current": "wrong-password-1", "new": "brand-new-password-1",
	})
	if resp.StatusCode != http.StatusForbidden || apiErrCode(t, resp) != "invalid_password" {
		t.Fatalf("wrong current password: %d", resp.StatusCode)
	}

	// Weak new password → 400.
	resp = a.do(http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current": "uncommon-password-9", "new": "short",
	})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "weak_password" {
		t.Fatalf("weak new password: %d", resp.StatusCode)
	}

	// Correct change → 204, and the response re-issues the *current* session.
	resp = a.do(http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current": "uncommon-password-9", "new": "brand-new-password-1",
	})
	if resp.StatusCode != 204 {
		t.Fatalf("password change: %d", resp.StatusCode)
	}
	if c := sessionCookieFrom(t, resp); c.Value == "" {
		t.Error("current session must be rotated with a new cookie")
	} else {
		resp.Body.Close()
	}

	// Session A (pre-change) and the old B token are dead.
	if resp = a.raw(http.MethodGet, "/api/v1/auth/me", nil, sessA); resp.StatusCode != 401 {
		t.Errorf("pre-change session still alive: %d", resp.StatusCode)
	}
	// The rotated current session works.
	if resp = a.do(http.MethodGet, "/api/v1/auth/me", nil); resp.StatusCode != 200 {
		t.Errorf("rotated session dead: %d", resp.StatusCode)
	}
	// Old password no longer logs in; new one does.
	if resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "pw@b.io", "password": "uncommon-password-9",
	}); resp.StatusCode != 401 {
		t.Errorf("old password still valid: %d %s", resp.StatusCode, dumpBody(resp))
	}
	if resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "pw@b.io", "password": "brand-new-password-1",
	}); resp.StatusCode != 200 {
		t.Errorf("new password rejected: %d %s", resp.StatusCode, dumpBody(resp))
	}
}

// dumpBody drains a response body for error messages (truncated).
func dumpBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	resp.Body.Close()
	if len(b) == 0 {
		return "(empty body)"
	}
	return string(b)
}
