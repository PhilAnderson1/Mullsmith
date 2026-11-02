package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/store"
)

func emailProfileFixture(t *testing.T, enabled bool) (*authTest, *Server, *store.User) {
	t.Helper()
	cfg := authCfg(t)
	if enabled {
		cfg = publicEmailConfig(t)
	}
	a, s := pwTestApp(t, cfg)
	now := time.Now()
	hash, err := auth.HashPassword(emailPW, cfg.Auth.Argon2)
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{Email: "profile@email.test", Name: "Owner", PasswordHash: hash, Status: store.StatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := a.db.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	raw, session, err := auth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.CreateLoginSession(context.Background(), &store.Session{IDHash: session, UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}, u); err != nil {
		t.Fatal(err)
	}
	a.cookies[auth.SessionCookieName] = raw
	return a, s, u
}

func TestEmailChangesRequirePasswordOnlyForChangedAddress(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "email off", true: "email on"}[enabled], func(t *testing.T) {
			a, _, u := emailProfileFixture(t, enabled)
			before := auth.HashWorkCount()
			for _, body := range []map[string]any{{"name": "Updated"}, {"default_system_prompt": "Be brief"}, {"email": strings.ToUpper(u.Email)}} {
				r := a.do("PATCH", "/api/v1/auth/me", body)
				r.Body.Close()
				if r.StatusCode != 200 {
					t.Fatalf("ordinary save=%d", r.StatusCode)
				}
			}
			if auth.HashWorkCount() != before {
				t.Fatal("metadata save hashed a password")
			}
			for _, tc := range []struct {
				password, code string
				status         int
			}{{"", "password_required", 400}, {"wrong-password", "invalid_password", 403}, {emailPW, "", 200}} {
				body := map[string]any{"email": "new@email.test", "name": "New Name"}
				if tc.password != "" {
					body["current_password"] = tc.password
				}
				r := a.do("PATCH", "/api/v1/auth/me", body)
				if r.StatusCode != tc.status {
					t.Fatalf("email change=%d %s", r.StatusCode, dumpBody(r))
				}
				if tc.code != "" {
					if code := apiErrCode(t, r); code != tc.code {
						t.Fatalf("code=%s", code)
					}
				} else {
					r.Body.Close()
				}
				fresh, err := a.db.UserByID(context.Background(), u.ID)
				if err != nil {
					t.Fatal(err)
				}
				if tc.status != 200 && (fresh.Email != u.Email || fresh.UnconfirmedEmail != "" || fresh.Name != "Updated") {
					t.Fatalf("refusal changed profile: %+v", fresh)
				}
			}
		})
	}
}

func TestQueuedEmailChangeCannotOvertakeRecoveryOrLogout(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, reset := range []bool{false, true} {
			t.Run(map[bool]string{false: "direct", true: "confirm"}[enabled]+map[bool]string{false: " logout", true: " reset"}[reset], func(t *testing.T) {
				a, s, u := emailProfileFixture(t, enabled)
				ctx := context.Background()
				token, err := a.db.CreateToken(ctx, store.TokenResetPassword, u.ID, u.Email, time.Now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				g, release := holdPasswordHashGate(t, s, 32)
				r := publicAuthRequest(t, "", map[string]any{"email": "attacker@email.test", "current_password": emailPW})
				r.Method = "PATCH"
				r.URL.Path = "/api/v1/auth/me"
				r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: a.cookies[auth.SessionCookieName]})
				results := make(chan *httptest.ResponseRecorder, 1)
				go func() { results <- servePublicAuth(s, r) }()
				waitForQueuedHashes(t, g, 1)
				if reset {
					if _, found, err := a.db.CompletePasswordReset(ctx, token, "replacement-hash", time.Now()); err != nil || !found {
						t.Fatalf("recovery=%v %v", found, err)
					}
				} else {
					if err := a.db.DeleteSession(ctx, auth.HashToken(a.cookies[auth.SessionCookieName])); err != nil {
						t.Fatal(err)
					}
				}
				release()
				w := awaitPublicAuth(t, results)
				if w.Code != 409 || !strings.Contains(w.Body.String(), "credentials_changed") {
					t.Fatalf("stale email change=%d %s", w.Code, w.Body.String())
				}
				fresh, err := a.db.UserByID(ctx, u.ID)
				if err != nil || fresh.Email != u.Email || fresh.UnconfirmedEmail != "" {
					t.Fatalf("stale change survived: %+v %v", fresh, err)
				}
			})
		}
	}
}

func TestRecoveryRejectsEarlierEmailChangeLink(t *testing.T) {
	a, fm := emailTestApp(t)
	activeUser(t, a, fm, "recover@email.test")
	r := a.do("POST", "/api/v1/auth/login", map[string]any{"email": "recover@email.test", "password": emailPW})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	r = a.do("PATCH", "/api/v1/auth/me", map[string]any{"email": "attacker@email.test", "current_password": emailPW})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	confirm := tokenFromJob(t, fm.next(t), "/#confirm?t=")
	_ = fm.next(t)
	owner := newAuthTestOn(t, a.ts, a.db)
	r = owner.do("POST", "/api/v1/auth/forgot-password", map[string]any{"email": "recover@email.test"})
	r.Body.Close()
	reset := tokenFromJob(t, fm.next(t), "/#reset?t=")
	r = owner.do("POST", "/api/v1/auth/reset-password", map[string]any{"token": reset, "password": "fresh-owner-password-987"})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	r = a.do("POST", "/api/v1/auth/confirm-email", map[string]any{"token": confirm})
	if r.StatusCode != 400 || apiErrCode(t, r) != "invalid_token" {
		t.Fatalf("old link=%d", r.StatusCode)
	}
	r = a.do("GET", "/api/v1/auth/me", nil)
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal("old cookie survived")
	}
	r = owner.do("POST", "/api/v1/auth/login", map[string]any{"email": "recover@email.test", "password": "fresh-owner-password-987"})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("owner cannot recover")
	}
	fresh := decodedBody(t, owner.do("GET", "/api/v1/auth/me", nil))
	if fresh["unconfirmed_email"] != "" {
		t.Fatalf("pending change survived: %v", fresh)
	}
}
