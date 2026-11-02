package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"mullsmith/internal/store"
)

func TestExpiredSignupHTTPDoesNotBlockRegistrationOrResend(t *testing.T) {
	a, fm := emailTestApp(t)
	registerPending(t, a, "reclaim@expiry.test")
	oldToken := tokenFromJob(t, fm.next(t), "/#confirm?t=")
	users, err := a.db.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatal("missing signup", err)
	}
	old := users[0]
	if err := a.db.ExecForTest(`UPDATE users SET created_at = ? WHERE id = ?`, time.Now().Add(-store.ConfirmationTTL).Unix(), old.ID); err != nil {
		t.Fatal(err)
	}
	r := a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": oldToken})
	if r.StatusCode != 400 || apiErrCode(t, r) != "invalid_token" {
		t.Fatalf("expired activation=%d", r.StatusCode)
	}
	r = a.do(http.MethodPost, "/api/v1/auth/resend-confirm", map[string]any{"email": old.UnconfirmedEmail})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	fm.expectQuiet(t)
	registerPending(t, a, old.UnconfirmedEmail)
	confirmViaMail(t, a, fm, old.UnconfirmedEmail)
	fresh, err := a.db.UserByEmail(context.Background(), old.UnconfirmedEmail)
	if err != nil || fresh == nil || fresh.ID == old.ID {
		t.Fatalf("reclaimed account=%+v %v", fresh, err)
	}
}

func TestPublicResendUsesRemainingSignupTime(t *testing.T) {
	a, fm := emailTestApp(t)
	registerPending(t, a, "remaining@expiry.test")
	_ = fm.next(t)
	users, err := a.db.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatal(err)
	}
	u := users[0]
	now := time.Now()
	if err := a.db.ExecForTest(`UPDATE users SET created_at = ? WHERE id = ?`, now.Add(-23*time.Hour).Unix(), u.ID); err != nil {
		t.Fatal(err)
	}
	r := a.do(http.MethodPost, "/api/v1/auth/resend-confirm", map[string]any{"email": u.UnconfirmedEmail})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	token := tokenFromJob(t, fm.next(t), "/#confirm?t=")
	if found, err := a.db.ConfirmEmail(context.Background(), token, now.Add(2*time.Hour)); err != nil || found {
		t.Fatalf("resend extended signup: %v %v", found, err)
	}
}

func TestExpiredEmailChangeAllowsImmediateSignup(t *testing.T) {
	a, fm := emailTestApp(t)
	activeUser(t, a, fm, "owner@expiry.test")
	r := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{"email": "owner@expiry.test", "password": emailPW})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	r = a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"email": "available@expiry.test", "current_password": emailPW})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	token := tokenFromJob(t, fm.next(t), "/#confirm?t=")
	_ = fm.next(t)
	owner, err := a.db.UserByEmail(context.Background(), "owner@expiry.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.ExecForTest(`UPDATE auth_tokens SET expires_at = ? WHERE user_id = ? AND kind = ?`, time.Now().Unix(), owner.ID, store.TokenConfirmEmail); err != nil {
		t.Fatal(err)
	}
	registerPending(t, a, "available@expiry.test")
	confirmViaMail(t, a, fm, "available@expiry.test")
	r = a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": token})
	if r.StatusCode != 400 || apiErrCode(t, r) != "invalid_token" {
		t.Fatalf("expired link=%d", r.StatusCode)
	}
	if changed, err := a.db.DeleteExpiredEmailChanges(context.Background(), time.Now()); err != nil || changed != 1 {
		t.Fatalf("expired pending cleanup=%d %v", changed, err)
	}
	owner, err = a.db.UserByID(context.Background(), owner.ID)
	if err != nil || owner.Email != "owner@expiry.test" || owner.UnconfirmedEmail != "" {
		t.Fatalf("owner damaged: %+v %v", owner, err)
	}
}
