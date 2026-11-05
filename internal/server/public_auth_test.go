package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
	"mullsmith/internal/store"
)

func publicAuthRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/auth/"+path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "public-test-token"})
	r.Header.Set(auth.CSRFHeaderName, "public-test-token")
	return r
}

func servePublicAuth(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func holdPasswordHashGate(t *testing.T, s *Server, queue int) (*auth.HashGate, func()) {
	t.Helper()
	g := auth.NewHashGate(1, queue)
	s.hashGate = g
	entered, unblock, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = g.Run(context.Background(), func() error { close(entered); <-unblock; return nil })
	}()
	<-entered
	var once sync.Once
	release := func() { once.Do(func() { close(unblock); <-done }) }
	t.Cleanup(release)
	return g, release
}

// Observe channel admission only in tests, without adding production hooks or
// sleeps that assume how long a user lookup takes. The holder owns one ticket.
func waitForQueuedHashes(t *testing.T, g *auth.HashGate, waiting int) {
	t.Helper()
	quota := reflect.ValueOf(g).Elem().FieldByName("quota")
	deadline := time.Now().Add(5 * time.Second)
	for quota.Len() != waiting+1 {
		if time.Now().After(deadline) {
			t.Fatal("requests did not reach password hash queue")
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitPublicAuth(t *testing.T, results <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-results:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("public request did not finish")
		return nil
	}
}

func signupPayload(email string) map[string]any {
	return map[string]any{"email": email, "name": "Tester", "password": "strong-password-123"}
}

func publicEmailConfig(t *testing.T) *config.Config {
	cfg := authCfg(t)
	cfg.Server.Host = "app.test:3210"
	cfg.Server.TLS.Enabled = true
	cfg.Server.TLS.CertFile = "test-cert.pem"
	cfg.Server.TLS.KeyFile = "test-key.pem"
	cfg.Email = config.Email{Enabled: true, SMTP: config.SMTP{Host: "mail.test", Port: 587, From: "app@example.test"}}
	return cfg
}

func TestQueuedLoginRejectsCompletedPasswordReset(t *testing.T) {
	a, s := pwTestApp(t, authCfg(t))
	signupAs(a, "reset-race@example.test", "reset-race-invite")
	u, err := a.db.UserByEmail(context.Background(), "reset-race@example.test")
	if err != nil || u == nil {
		t.Fatalf("user lookup: %v", err)
	}
	raw, err := a.db.CreateToken(context.Background(), store.TokenResetPassword, u.ID, u.Email, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := auth.HashPassword("new-password-after-reset", s.cfg.Auth.Argon2)
	if err != nil {
		t.Fatal(err)
	}
	g, release := holdPasswordHashGate(t, s, 32)
	results := make(chan *httptest.ResponseRecorder, 1)
	r := publicAuthRequest(t, "login", map[string]any{"email": u.Email, "password": "passw0rd-long-enough"})
	go func() { results <- servePublicAuth(s, r) }()
	waitForQueuedHashes(t, g, 1)
	if _, found, err := a.db.CompletePasswordReset(context.Background(), raw, newHash, time.Now()); err != nil || !found {
		t.Fatalf("reset: %v %v", found, err)
	}
	release()
	w := awaitPublicAuth(t, results)
	if w.Code != 401 {
		t.Fatalf("stale login: %d %s", w.Code, w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("stale login issued cookies")
	}
	w = servePublicAuth(s, publicAuthRequest(t, "login", map[string]any{"email": u.Email, "password": "new-password-after-reset"}))
	if w.Code != 200 {
		t.Fatalf("fresh login: %d %s", w.Code, w.Body.String())
	}
}

func TestPublicKeyChurnCannotResetSignupBudget(t *testing.T) {
	cfg := authCfg(t)
	cfg.Auth.Registration, cfg.Auth.MaxSignupsPerHour = "open", 1
	_, s := pwTestApp(t, cfg)
	w := servePublicAuth(s, publicAuthRequest(t, "register", signupPayload("first@example.test")))
	if w.Code != 201 {
		t.Fatalf("initial signup: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 17000; i++ {
		s.limiter.Allow(fmt.Sprintf("forgotmail:random%d@example.test", i), 3, 15*time.Minute)
	}
	w = servePublicAuth(s, publicAuthRequest(t, "register", signupPayload("second@example.test")))
	if w.Code != 429 {
		t.Fatalf("signup budget reset: %d %s", w.Code, w.Body.String())
	}
}

func TestFailedSignupDoesNotSpendCreationBudget(t *testing.T) {
	cfg := authCfg(t)
	cfg.Auth.Registration, cfg.Auth.MaxSignupsPerHour = "open", 1
	_, s := pwTestApp(t, cfg)
	payload := signupPayload("failed@example.test")
	payload["invite"] = "nonexistent-invite"
	before := auth.HashWorkCount()
	w := servePublicAuth(s, publicAuthRequest(t, "register", payload))
	if w.Code != 400 || auth.HashWorkCount() != before {
		t.Fatalf("unexpected invite was not rejected before hashing: %d", w.Code)
	}
	_, release := holdPasswordHashGate(t, s, 0)
	w = servePublicAuth(s, publicAuthRequest(t, "register", signupPayload("busy@example.test")))
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"server_busy"`) {
		t.Fatalf("hash gate: %d %s", w.Code, w.Body.String())
	}
	release()
	w = servePublicAuth(s, publicAuthRequest(t, "register", signupPayload("valid@example.test")))
	if w.Code != 201 {
		t.Fatalf("failed signup spent capacity: %d %s", w.Code, w.Body.String())
	}
}

func TestCancelledSignupReleasesCreationBudget(t *testing.T) {
	cfg := authCfg(t)
	cfg.Auth.Registration, cfg.Auth.MaxSignupsPerHour = "open", 1
	_, s := pwTestApp(t, cfg)
	g, release := holdPasswordHashGate(t, s, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := publicAuthRequest(t, "register", signupPayload("cancelled@example.test")).WithContext(ctx)
	results := make(chan *httptest.ResponseRecorder, 1)
	go func() { results <- servePublicAuth(s, r) }()
	waitForQueuedHashes(t, g, 1)
	// The in-flight reservation must exclude a second signup even though no
	// account has been created yet.
	w := servePublicAuth(s, publicAuthRequest(t, "register", signupPayload("over-capacity@example.test")))
	if w.Code != 429 {
		t.Fatalf("pending capacity was bypassed: %d", w.Code)
	}
	cancel()
	awaitPublicAuth(t, results)
	release()
	w = servePublicAuth(s, publicAuthRequest(t, "register", signupPayload("after-cancel@example.test")))
	if w.Code != 201 {
		t.Fatalf("cancellation spent capacity: %d %s", w.Code, w.Body.String())
	}
}

func TestConcurrentPendingSignupsRemainUnique(t *testing.T) {
	cfg := publicEmailConfig(t)
	cfg.Auth.Registration, cfg.Auth.MaxSignupsPerHour = "open", 2
	_, s := pwTestApp(t, cfg)
	g, release := holdPasswordHashGate(t, s, 32)
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		r := publicAuthRequest(t, "register", signupPayload("same@example.test"))
		go func() { results <- servePublicAuth(s, r) }()
	}
	waitForQueuedHashes(t, g, 2)
	release()
	counts := map[int]int{}
	for i := 0; i < 2; i++ {
		counts[awaitPublicAuth(t, results).Code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatalf("signup results: %v", counts)
	}
	users, err := s.store.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("pending users: %d %v", len(users), err)
	}
	w := servePublicAuth(s, publicAuthRequest(t, "register", signupPayload("different@example.test")))
	if w.Code != 201 {
		t.Fatalf("duplicate signup spent capacity: %d %s", w.Code, w.Body.String())
	}
}

func TestLoginWarningsDoNotRevealAccountExistence(t *testing.T) {
	a, s := pwTestApp(t, authCfg(t))
	a.invite("warning-invite")
	resp := a.do("POST", "/api/v1/auth/register", map[string]any{"email": "known@example.test", "name": "Tester", "password": "strong-password-123", "invite": "warning-invite", "remember_me": true})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatal("registration failed")
	}
	for _, email := range []string{"known@example.test", "unknown@example.test"} {
		for i, want := range []string{"2 attempts remaining", "1 attempt remaining", "try again after"} {
			w := servePublicAuth(s, publicAuthRequest(t, "login", map[string]any{"email": email, "password": "wrong-password-123"}))
			if w.Code != 401 || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("%s attempt %d: %d %s", email, i, w.Code, w.Body.String())
			}
			if i == 2 {
				seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
				if err != nil || seconds < 1 || seconds > 900 {
					t.Fatalf("invalid lockout retry: %v %v", seconds, err)
				}
			}
		}
		w := servePublicAuth(s, publicAuthRequest(t, "login", map[string]any{"email": email, "password": "strong-password-123"}))
		if w.Code != 429 || !strings.Contains(w.Body.String(), `"account_locked"`) || w.Header().Get("Retry-After") == "" {
			t.Fatalf("lockout response: %d %s", w.Code, w.Body.String())
		}
	}
	// A remembered session remains usable while password login is locked.
	resp = a.do("GET", "/api/v1/auth/me", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("lockout invalidated remembered session: %d", resp.StatusCode)
	}
}

func TestInvalidResetTokensSkipHashing(t *testing.T) {
	for _, kind := range []string{"invented", "expired", "wrong purpose", "used", "address changed"} {
		t.Run(kind, func(t *testing.T) {
			_, s := pwTestApp(t, publicEmailConfig(t))
			ctx, now := context.Background(), time.Now()
			u := &store.User{Email: "reset@example.test", Name: "Tester", PasswordHash: "original", Status: store.StatusEnabled, CreatedAt: now, UpdatedAt: now}
			if err := s.store.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			token, purpose, expiry := "invented", store.TokenResetPassword, now.Add(time.Hour)
			if kind == "expired" {
				expiry = now.Add(-time.Second)
			}
			if kind == "wrong purpose" {
				purpose = store.TokenConfirmEmail
			}
			if kind != "invented" {
				var err error
				token, err = s.store.CreateToken(ctx, purpose, u.ID, u.Email, expiry)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "used" {
				if _, found, err := s.store.CompletePasswordReset(ctx, token, "replacement", now); err != nil || !found {
					t.Fatalf("consume: %v %v", found, err)
				}
			}
			if kind == "address changed" {
				if err := s.store.ExecForTest(`UPDATE users SET email = ? WHERE id = ?`, "new@example.test", u.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := auth.HashWorkCount()
			w := servePublicAuth(s, publicAuthRequest(t, "reset-password", map[string]any{"token": token, "password": "new-password-123"}))
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_token"`) || auth.HashWorkCount() != before {
				t.Fatalf("dead token hashed or accepted: status=%d hashes=%d body=%s", w.Code, auth.HashWorkCount()-before, w.Body.String())
			}
		})
	}
}

func TestConcurrentPasswordResetConsumesTokenOnce(t *testing.T) {
	_, s := pwTestApp(t, publicEmailConfig(t))
	ctx, now := context.Background(), time.Now()
	u := &store.User{Email: "reset-once@example.test", Name: "Tester", PasswordHash: "original", Status: store.StatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	token, err := s.store.CreateToken(ctx, store.TokenResetPassword, u.ID, u.Email, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g, release := holdPasswordHashGate(t, s, 32)
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		r := publicAuthRequest(t, "reset-password", map[string]any{"token": token, "password": fmt.Sprintf("new-password-number-%d", i)})
		go func() { results <- servePublicAuth(s, r) }()
	}
	waitForQueuedHashes(t, g, 2)
	release()
	counts := map[int]int{}
	for i := 0; i < 2; i++ {
		counts[awaitPublicAuth(t, results).Code]++
	}
	if counts[200] != 1 || counts[400] != 1 {
		t.Fatalf("reset results: %v", counts)
	}
}
