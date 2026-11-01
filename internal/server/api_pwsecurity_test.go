package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
)

// --- audit finding 4: bound the Argon2 work, equalise its cost ---------------

// pwTestApp builds the app exactly like newTestServerWithDB but hands back
// the *Server, so a test can hold the hash gate open deterministically
// instead of racing sub-millisecond hashes.
func pwTestApp(t *testing.T, cfg *config.Config) (*authTest, *Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := newPreparedServer(t, cfg, db, logger, "test", nil, nil)
	ts := httptest.NewServer(s.Handler())
	return newAuthTestOn(t, ts, db), s
}

// The old DummyVerify computed TWO argon2 hashes while a wrong-password
// attempt computed one: unknown emails answered ~2x slower (an enumeration
// oracle against spec §9.1's indistinguishability promise) at double the
// hashing load. Now both paths cost exactly one.
func TestUnknownEmailCostsOneHash(t *testing.T) {
	a, _ := pwTestApp(t, authCfg(t))
	signupAs(a, "onehash@test.io", "gk-code-1")

	before := auth.HashWorkCount()
	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "nobody@nowhere.io", "password": "whatever-there",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown email: %d, want 401", resp.StatusCode)
	}
	unknownOps := auth.HashWorkCount() - before

	before = auth.HashWorkCount()
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "onehash@test.io", "password": "wrong-but-long-enough",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d, want 401", resp.StatusCode)
	}
	wrongOps := auth.HashWorkCount() - before

	if unknownOps != 1 || wrongOps != 1 {
		t.Errorf("hash ops: unknown-email=%d wrong-password=%d, want 1 and 1", unknownOps, wrongOps)
	}
}

// With the single permitted slot held, the next password operation must be
// refused outright (429 server_busy + Retry-After) — and must work again
// the moment the slot frees.
func TestHashGateSaturation429(t *testing.T) {
	cfg := authCfg(t)
	a, s := pwTestApp(t, cfg)
	signupAs(a, "gate@test.io", "gk-code-1")

	// queue 0: saturation is observable with a single held slot.
	g := auth.NewHashGate(1, 0)
	s.hashGate = g

	started, block, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = g.Run(context.Background(), func() error { close(started); <-block; return nil })
	}()
	<-started

	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "gate@test.io", "password": "passw0rd-long-enough",
	})
	if resp.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp) != "server_busy" {
		t.Fatalf("held gate: %d, want 429 server_busy", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("429 without Retry-After")
	}
	resp.Body.Close()

	close(block)
	<-done // queue 0: retry only after the slot provably freed
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "gate@test.io", "password": "passw0rd-long-enough",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after release: %d, want 200", resp.StatusCode)
	}
}

// A dead invite must fail BEFORE any hashing (audit finding 4): invalid and
// already-used codes leave the hash counter untouched.
func TestBadInviteSkipsHash(t *testing.T) {
	a, _ := pwTestApp(t, authCfg(t))

	before := auth.HashWorkCount()
	resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "ghost@test.io", "name": "Ghost",
		"password": "passw0rd-long-enough", "invite": "never-minted",
	})
	if resp.StatusCode != http.StatusForbidden || apiErrCode(t, resp) != "invite_invalid" {
		t.Fatalf("unknown invite: %d, want 403 invite_invalid", resp.StatusCode)
	}
	if got := auth.HashWorkCount() - before; got != 0 {
		t.Errorf("unknown invite burned %d hashes, want 0", got)
	}

	signupAs(a, "first@test.io", "gk-used") // signupAs mints the code itself
	before = auth.HashWorkCount()
	resp = a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "second@test.io", "name": "Second",
		"password": "passw0rd-long-enough", "invite": "gk-used",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("used invite: %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	if got := auth.HashWorkCount() - before; got != 0 {
		t.Errorf("used invite burned %d hashes, want 0", got)
	}
}

// The password-change endpoint runs two argon2 ops per call, so it gets its
// own bucket (5/15 min per user). The generic API budget stays untouched:
// other endpoints still work while the change throttle is armed.
func TestPasswordChangeThrottle(t *testing.T) {
	a, _ := pwTestApp(t, authCfg(t))
	signupAs(a, "pwc@test.io", "gk-code-1")

	cur := "passw0rd-long-enough"
	for i := 0; i < 5; i++ {
		next := fmt.Sprintf("passw0rd-long-enough-%d", i)
		resp := a.do(http.MethodPost, "/api/v1/auth/password",
			map[string]any{"current": cur, "new": next})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("change %d: %d, want 204", i+1, resp.StatusCode)
		}
		resp.Body.Close()
		cur = next
	}

	resp := a.do(http.MethodPost, "/api/v1/auth/password",
		map[string]any{"current": cur, "new": "passw0rd-long-enough-x"})
	if resp.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp) != "pw_change_limited" {
		t.Fatalf("6th change: %d, want 429 pw_change_limited", resp.StatusCode)
	}
	resp.Body.Close()

	// Dedicated bucket: everything else is unaffected.
	resp = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("auth/me while pw-throttled: %d, want 200", resp.StatusCode)
	}
}
