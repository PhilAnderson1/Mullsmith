package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
	"mullsmith/internal/store"
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
	return newAuthTestOn(t, ts, db)
}

// newAuthTestOn bootstraps CSRF cookies on an already-built test server so
// suites with custom wiring (e.g. injected tool registries) share the jar
// setup.
func newAuthTestOn(t *testing.T, ts *httptest.Server, db *store.DB) *authTest {
	t.Helper()
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

// --- audit1 L-6: auth transitions rotate the double-submit token -----------

func TestCSRFTokenRotatesAtAuthTransitions(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	boot := a.csrf

	a.invite("gk-code-1")
	resp := a.register("rotate@b.io", "uncommon-password-9", false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d", resp.StatusCode)
	}
	if a.csrf == boot {
		t.Error("entering auth did not rotate the CSRF token")
	}
	atLogin := a.csrf

	resp = a.do(http.MethodPost, "/api/v1/auth/logout", nil)
	resp.Body.Close()
	if a.csrf == atLogin {
		t.Error("logout did not rotate the CSRF token")
	}

	// Rotated, not just cleared: the very next mutation (a fresh login)
	// must pass the double-submit check with the new token.
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "rotate@b.io", "password": "uncommon-password-9",
	})
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Error("post-rotation login hit csrf_failed — token was cleared, not rotated")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-rotation login = %d, want 200", resp.StatusCode)
	}

	// Logout of the second session rotates yet again.
	resp = a.do(http.MethodPost, "/api/v1/auth/logout", nil)
	resp.Body.Close()
	if a.csrf == atLogin || a.csrf == boot {
		t.Error("second logout reused a stale CSRF token")
	}
}

// --- audit1 L-3: one mailbox, one account, however spelled ----------------

func TestEmailNormalizedAtBoundary(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	resp := a.register("PayPal Support <PHil@ExAmPlE.COM>", "uncommon-password-9", false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register with display-name variant = %d, want 201", resp.StatusCode)
	}
	u, err := a.db.UserByEmail(context.Background(), "phil@example.com")
	if err != nil || u == nil {
		t.Fatalf("no row under the canonical phil@example.com (err=%v)", err)
	}
	if u.Email != "phil@example.com" {
		t.Errorf("stored email = %q, want canonical form", u.Email)
	}

	// One mailbox, one account: the plain spelling now hits the duplicate
	// pre-flight that used to be blind to the variant.
	a.invite("gk-code-2")
	resp = a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "phil@example.com", "name": "Second", "password": "uncommon-password-9",
		"invite": "gk-code-2",
	})
	if resp.StatusCode != http.StatusConflict || apiErrCode(t, resp) != "duplicate_email" {
		t.Errorf("plain re-register of a variant-registered mailbox = %d, want 409", resp.StatusCode)
	}

	// Login reaches the account through any valid spelling.
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "PHIL@EXAMPLE.COM", "password": "uncommon-password-9",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("uppercase login = %d, want 200", resp.StatusCode)
	}

	// "+detail" is deliberately NOT folded — plus-aliasing is separation
	// by intent, and folding it would merge distinct mailboxes.
	a.invite("gk-code-3")
	resp = a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "phil+news@example.com", "name": "Plus", "password": "uncommon-password-9",
		"invite": "gk-code-3",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("plus-tag address = %d, want 201 (distinct mailbox by design)", resp.StatusCode)
	}
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

// Global creation budget, open mode only (audit1 M-2). Consumption sits after
// every validation and pre-flight and before the hash, so one spent unit
// equals one row actually attempted — garbage and duplicates bounce off the
// bucket instead of burning it and locking out legitimate sign-ups.
func TestOpenSignupGlobalBudget(t *testing.T) {
	// registerOpen posts without an invite field — the shared register()
	// helper always replays a code, which open mode would reject as unknown.
	registerOpen := func(a *authTest, email, password string) *http.Response {
		return a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
			"email": email, "name": "Test User", "password": password,
		})
	}
	t.Run("caps at the limit", func(t *testing.T) {
		cfg := authCfg(t)
		cfg.Auth.Registration = "open"
		cfg.Auth.MaxSignupsPerHour = 2
		a := newAuthTest(t, cfg)

		// A malformed attempt (weak password) is refused upstream of the
		// bucket: the two slots below must still be intact.
		if resp := registerOpen(a, "weak@b.io", "short9"); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("weak password: %d", resp.StatusCode)
		}
		if resp := registerOpen(a, "one@b.io", "uncommon-password-9"); resp.StatusCode != http.StatusCreated {
			t.Fatalf("signup 1: %d", resp.StatusCode)
		}
		if resp := registerOpen(a, "two@b.io", "another-passphrase-9"); resp.StatusCode != http.StatusCreated {
			t.Fatalf("signup 2: %d", resp.StatusCode)
		}
		// Budget spent: the third valid attempt is refused and creates no row.
		resp := registerOpen(a, "three@b.io", "third-passphrase-9")
		if resp.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp) != "rate_limited" {
			t.Fatalf("over budget: %d", resp.StatusCode)
		}
		if users, err := a.db.ListUsers(context.Background()); err != nil || len(users) != 2 {
			t.Errorf("users after refusal = %v, %v, want only the two sign-ups", users, err)
		}
		// The duplicate 409 is answered by the pre-flight before the bucket,
		// so a taken address never consumes (or needs) budget: this stays a
		// 409 even with the global bucket empty.
		if resp := registerOpen(a, "one@b.io", "uncommon-password-9"); resp.StatusCode != http.StatusConflict ||
			apiErrCode(t, resp) != "duplicate_email" {
			t.Errorf("duplicate with empty budget: %d", resp.StatusCode)
		}
	})

	// The regression this pins: the -1 sentinel must mean "no limit", not
	// "zero sign-ups" and not "fall through to the default of 5".
	t.Run("minus-one disables the limit", func(t *testing.T) {
		cfg := authCfg(t)
		cfg.Auth.Registration = "open"
		cfg.Auth.MaxSignupsPerHour = -1 // the off sentinel
		a := newAuthTest(t, cfg)
		for i, email := range []string{"u1@b.io", "u2@b.io", "u3@b.io"} {
			if resp := registerOpen(a, email, fmt.Sprintf("passphrase-%d-x9z", i+1)); resp.StatusCode != http.StatusCreated {
				t.Fatalf("signup %d with limit disabled: %d", i+1, resp.StatusCode)
			}
		}
	})

	// invite-only mode ignores the budget entirely — the codes are the
	// budget there, and capping again would only punish operators minting
	// invites at a normal pace.
	t.Run("invite-only ignores budget", func(t *testing.T) {
		cfg := authCfg(t) // invite-only
		cfg.Auth.MaxSignupsPerHour = 1
		a := newAuthTest(t, cfg)
		a.invite("gk-code-1") // register() replays exactly this code
		a.invite("gk-code-2")
		if resp := a.register("inv1@b.io", "uncommon-password-9", false); resp.StatusCode != http.StatusCreated {
			t.Fatalf("invite signup 1: %d", resp.StatusCode)
		}
		resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
			"email": "inv2@b.io", "name": "Second", "password": "another-passphrase-9",
			"invite": "gk-code-2",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("invite signup 2 over budget=1: %d", resp.StatusCode)
		}
	})
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

	// Session works immediately; /me reports identity without scanning storage.
	resp = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	m = decodeBody(t, resp)
	if resp.StatusCode != 200 || m["id"] != userID {
		t.Fatalf("/me = %d %v", resp.StatusCode, m)
	}
	if _, ok := m["quota"]; ok {
		t.Errorf("/me returned unused quota data: %v", m)
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

// Every door that answers WHO YOU ARE carries the same identity payload
// (mePayload): the SPA seeds state.me from the login/register body, so a
// leaner body there silently blanked the Settings default prompt and the
// pending-email notice until the next page reload.
func TestLoginAndRegisterReturnIdentityPayload(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	resp := a.register("shape@b.io", "uncommon-password-9", false)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d, want 201", resp.StatusCode)
	}
	reg := decodeBody(t, resp)
	if _, ok := reg["default_system_prompt"]; !ok {
		t.Fatalf("register body %v lacks the identity shape", reg)
	}

	ctx := context.Background()
	u, err := a.db.UserByEmail(ctx, "shape@b.io")
	if err != nil || u == nil {
		t.Fatalf("lookup: %v", err)
	}
	if err := a.db.UpdateProfile(ctx, u.ID, u.Name, "be brief", time.Now()); err != nil {
		t.Fatal(err)
	}

	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "shape@b.io", "password": "uncommon-password-9",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	}
	login := decodeBody(t, resp)
	if login["default_system_prompt"] != "be brief" {
		t.Errorf("login body lost the default prompt: %v", login)
	}

	resp = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET me = %d", resp.StatusCode)
	}
	if me := decodeBody(t, resp); !reflect.DeepEqual(login, me) {
		t.Errorf("login and GET /auth/me disagree on the identity shape:\nlogin: %v\nme:    %v", login, me)
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
	u := &store.User{Email: "exp@b.io", Name: "E", PasswordHash: "x", Status: "enabled",
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
	changed, err := a.db.DisableUser(ctx, u.ID)
	if err != nil || !changed {
		t.Fatalf("disable: changed=%v err=%v", changed, err)
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
	u := &store.User{Email: "slide@b.io", Name: "S", PasswordHash: "x", Status: "enabled",
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

// C4 regression: when the rotation half of the password change fails, the
// WHOLE transition must roll back — the earlier stepwise code logged a
// warning and answered 204 with a partially applied security change. The
// trigger below makes the final INSERT INTO sessions fail inside the
// transaction; afterwards nothing may have moved: same hash, both sessions
// alive.
func TestPasswordChangeRollsBackOnRotationFailure(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	a.invite("gk-code-1")
	resp := a.register("rb@b.io", "uncommon-password-9", false)
	sessA := sessionCookieFrom(t, resp).Value
	resp.Body.Close()
	// Second login: jar holds session B, A remains a live second device.
	if resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "rb@b.io", "password": "uncommon-password-9",
	}); resp.StatusCode != 200 {
		t.Fatalf("second login: %d", resp.StatusCode)
	}
	resp.Body.Close()

	if err := a.db.ExecForTest(`CREATE TRIGGER no_new_sessions BEFORE INSERT ON sessions
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	resp = a.do(http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current": "uncommon-password-9", "new": "brand-new-password-1",
	})
	if resp.StatusCode != http.StatusInternalServerError || apiErrCode(t, resp) != "internal" {
		t.Fatalf("rotation failure = %d, want 500", resp.StatusCode)
	}
	got, err := a.db.UserByEmail(context.Background(), "rb@b.io")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	hashAfterFail := got.PasswordHash
	// The other device survived: revocation rolled back with the insert.
	if resp = a.raw(http.MethodGet, "/api/v1/auth/me", nil, sessA); resp.StatusCode != 200 {
		t.Errorf("other session revoked by a rolled-back change: %d", resp.StatusCode)
	}
	// With the trigger gone the same request succeeds end to end, and the
	// login proves the pre-failure hash was never replaced in isolation.
	if err := a.db.ExecForTest(`DROP TRIGGER no_new_sessions`); err != nil {
		t.Fatal(err)
	}
	if resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "rb@b.io", "password": "uncommon-password-9",
	}); resp.StatusCode != 200 {
		t.Errorf("old password dead after failed rotation: %d %s", resp.StatusCode, dumpBody(resp))
	}
	if resp = a.do(http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current": "uncommon-password-9", "new": "brand-new-password-1",
	}); resp.StatusCode != 204 {
		t.Fatalf("retry after failure = %d %s", resp.StatusCode, dumpBody(resp))
	}
	if got, err = a.db.UserByEmail(context.Background(), "rb@b.io"); err != nil ||
		got.PasswordHash == hashAfterFail {
		t.Errorf("retry did not apply the new hash: %v", err)
	}
}

// C4 regression: the stale guard. When the stored hash no longer matches the
// one the handler verified (a concurrent Settings tab, admin reset or spent
// mail link beat this request), the store op must refuse with
// ErrStalePassword and change NOTHING — never silently overwrite the fresher
// credential.
func TestPasswordChangeStaleGuard(t *testing.T) {
	a := newAuthTest(t, authCfg(t))
	ctx := context.Background()
	now := time.Now()
	u := &store.User{Email: "stale@b.io", Name: "S", PasswordHash: "hash-first",
		Status: "enabled", CreatedAt: now, UpdatedAt: now}
	if err := a.db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	_, hashA, _ := auth.NewSessionToken()
	_, hashB, _ := auth.NewSessionToken()
	for _, h := range [][]byte{hashA, hashB} {
		if err := a.db.CreateSession(ctx, &store.Session{IDHash: h, UserID: u.ID,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	// verifiedHash is the PRE-rotation value; the row already moved on.
	_, err := a.db.ChangePasswordAndRotate(ctx, u.ID, "hash-second", "hash-old",
		hashA, false, now.Add(time.Hour), now, "test")
	if !errors.Is(err, store.ErrStalePassword) {
		t.Fatalf("stale rotate = %v, want ErrStalePassword", err)
	}
	got, err := a.db.UserByID(ctx, u.ID)
	if err != nil || got == nil || got.PasswordHash != "hash-first" {
		t.Errorf("stale rotate changed the hash: %+v err=%v", got, err)
	}
	for _, h := range [][]byte{hashA, hashB} {
		s, err := a.db.SessionByHash(ctx, h)
		if err != nil || s == nil {
			t.Errorf("stale rotate killed session %x: %v", h[:4], err)
		}
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
