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

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
	"mullsmith/internal/mail"
	"mullsmith/internal/store"
)

// --- harness: an app whose "SMTP" records jobs in a channel -------------------

type fakeMailer struct {
	ch chan mail.Job
}

func (f *fakeMailer) Deliver(_ context.Context, to, subject, body string) error {
	select {
	case f.ch <- mail.Job{To: to, Subject: subject, Body: body}:
	default:
	}
	return nil
}

// next waits (test-poll convention) for the next queued delivery.
func (f *fakeMailer) next(t *testing.T) mail.Job {
	t.Helper()
	select {
	case j := <-f.ch:
		return j
	case <-time.After(5 * time.Second):
		t.Fatal("no mail delivered within 5s")
		return mail.Job{}
	}
}

// expectQuiet asserts nothing new is delivered for a grace period — the
// "no mail queued" side of the no-enumeration contract.
func (f *fakeMailer) expectQuiet(t *testing.T) {
	t.Helper()
	select {
	case j := <-f.ch:
		t.Fatalf("unexpected mail queued: to=%s subject=%q", j.To, j.Subject)
	case <-time.After(150 * time.Millisecond):
	}
}

// drain empties the channel (start a "no new mail" window cleanly).
func (f *fakeMailer) drain() {
	for {
		select {
		case <-f.ch:
		default:
			return
		}
	}
}

// emailTestApp wires the full handler chain with email features ON and a
// recording mailer. Registration is open — these tests never mint invites.
func emailTestApp(t *testing.T) (*authTest, *fakeMailer) {
	t.Helper()
	cfg := authCfg(t)
	cfg.Auth.Registration = "open"
	cfg.Server.Host = "gk.test:3210"
	cfg.Server.TLS.Enabled = true
	cfg.Server.TLS.CertFile = "test-cert.pem"
	cfg.Server.TLS.KeyFile = "test-key.pem"
	cfg.Email = config.Email{
		Enabled: true,
		SMTP:    config.SMTP{Host: "mail.test", Port: 587, From: "mullsmith@test"},
	}
	fm := &fakeMailer{ch: make(chan mail.Job, 64)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := newPreparedServer(t, cfg, db, logger, "test", nil, mail.NewOutbox(fm, logger))
	ts := httptest.NewServer(s.Handler())
	return newAuthTestOn(t, ts, db), fm
}

// emailOffApp is the same surface with the switch off (the default cfg).
func emailOffApp(t *testing.T) *authTest {
	t.Helper()
	cfg := authCfg(t)
	cfg.Auth.Registration = "open"
	return newAuthTest(t, cfg)
}

// tokenFromJob pulls the raw URL token out of a built message body, which
// carries "<public origin>/#confirm?t=…" or "/#reset?t=…" on its own line.
func tokenFromJob(t *testing.T, j mail.Job, frag string) string {
	t.Helper()
	i := strings.Index(j.Body, frag)
	if i < 0 {
		t.Fatalf("no %q in mail body:\n%s", frag, j.Body)
	}
	rest := j.Body[i+len(frag):]
	if k := strings.IndexAny(rest, " \t\n\r"); k >= 0 {
		rest = rest[:k]
	}
	return rest
}

const emailPW = "uncommon-password-9"

// registerPending signs up under email.enabled and returns the response map.
func registerPending(t *testing.T, a *authTest, email string) map[string]any {
	t.Helper()
	resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": email, "name": "Tester", "password": emailPW,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: %d, want 201", email, resp.StatusCode)
	}
	return decodeBody(t, resp)
}

// confirmViaMail activates a pending sign-up through the real endpoint.
func confirmViaMail(t *testing.T, a *authTest, fm *fakeMailer, wantTo string) {
	t.Helper()
	j := fm.next(t)
	if j.To != wantTo {
		t.Fatalf("confirm mail to %q, want %q", j.To, wantTo)
	}
	tok := tokenFromJob(t, j, "/#confirm?t=")
	resp := a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": tok})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm-email: %d, want 200", resp.StatusCode)
	}
}

// activeUser: register + confirm → a user login can find.
func activeUser(t *testing.T, a *authTest, fm *fakeMailer, email string) {
	t.Helper()
	registerPending(t, a, email)
	confirmViaMail(t, a, fm, email)
}

// --- the switch: routes 404 while disabled ------------------------------------

func TestEmailRoutesGatedWhenDisabled(t *testing.T) {
	a := emailOffApp(t)
	for _, route := range []struct{ path, body string }{
		{"/api/v1/auth/forgot-password", "email"},
		{"/api/v1/auth/reset-password", "token"},
		{"/api/v1/auth/confirm-email", "token"},
		{"/api/v1/auth/resend-confirm", "email"},
	} {
		resp := a.do(http.MethodPost, route.path, map[string]any{route.body: "x@y.io"})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s with features off = %d, want 404", route.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// cancel-email is authenticated; log in (features off → instant
	// session) so the request reaches the handler's own switch check.
	resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "off@user.io", "name": "Off", "password": emailPW,
	})
	resp.Body.Close()
	resp = a.do(http.MethodPost, "/api/v1/auth/me/cancel-email", map[string]any{})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cancel-email off = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Public auth config advertises the switch both ways.
	off := decodeBody(t, a.do(http.MethodGet, "/api/v1/auth/config", nil))
	if off["email_features"] != false {
		t.Errorf("meta email_features off = %v, want false", off["email_features"])
	}
	on, _ := emailTestApp(t)
	onMeta := decodeBody(t, on.do(http.MethodGet, "/api/v1/auth/config", nil))
	if onMeta["email_features"] != true {
		t.Errorf("meta email_features on = %v, want true", onMeta["email_features"])
	}
}

// --- forgot-password: byte-identical 200 everywhere ----------------------------

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestForgotPasswordNoEnumeration(t *testing.T) {
	a, fm := emailTestApp(t)
	activeUser(t, a, fm, "real@forgot.io")
	fm.drain()

	respReal := a.do(http.MethodPost, "/api/v1/auth/forgot-password", map[string]any{
		"email": "real@forgot.io",
	})
	bodyReal := readAll(t, respReal)
	if respReal.StatusCode != http.StatusOK {
		t.Fatalf("forgot existing = %d, want 200", respReal.StatusCode)
	}

	respUnknown := a.do(http.MethodPost, "/api/v1/auth/forgot-password", map[string]any{
		"email": "nobody@nowhere.io",
	})
	bodyUnknown := readAll(t, respUnknown)
	if respUnknown.StatusCode != http.StatusOK {
		t.Fatalf("forgot unknown = %d, want 200", respUnknown.StatusCode)
	}
	if string(bodyReal) != string(bodyUnknown) {
		t.Errorf("forgot envelopes differ:\n%s\n%s", bodyReal, bodyUnknown)
	}

	// A mail went out for the real account, and only for it.
	j := fm.next(t)
	if j.To != "real@forgot.io" || !strings.Contains(j.Body, "https://gk.test:3210/#reset?t=") {
		t.Errorf("reset mail wrong: to=%q body=%q", j.To, j.Body)
	}
	fm.expectQuiet(t)
}

// --- reset happy path: new hash, revoked sessions, one-shot ---------------------

func TestResetPasswordFlow(t *testing.T) {
	a, fm := emailTestApp(t)
	activeUser(t, a, fm, "reset@flow.io")
	fm.drain()

	// A live session that the reset must annihilate.
	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "reset@flow.io", "password": emailPW,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login before reset: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = a.do(http.MethodPost, "/api/v1/auth/forgot-password", map[string]any{"email": "reset@flow.io"})
	resp.Body.Close()
	j := fm.next(t)
	tok := tokenFromJob(t, j, "/#reset?t=")

	// Weak password: rejected BEFORE the token burns (usable again below).
	resp = a.do(http.MethodPost, "/api/v1/auth/reset-password", map[string]any{
		"token": tok, "password": "short",
	})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "weak_password" {
		t.Fatalf("weak reset password: %d, want 400 weak_password", resp.StatusCode)
	}

	const newPW = "brand-new-password-1"
	resp = a.do(http.MethodPost, "/api/v1/auth/reset-password", map[string]any{
		"token": tok, "password": newPW,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset: %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Old session is dead.
	resp = a.do(http.MethodGet, "/api/v1/auth/me", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("pre-reset session after reset = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Old password fails, new password works.
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "reset@flow.io", "password": emailPW,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old password after reset = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "reset@flow.io", "password": newPW,
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("new password login = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// One-shot: the consumed token is dead; a bogus one says the same thing.
	for _, bad := range []string{tok, "bogus-token-42"} {
		resp = a.do(http.MethodPost, "/api/v1/auth/reset-password", map[string]any{
			"token": bad, "password": newPW,
		})
		if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "invalid_token" {
			t.Errorf("token %q: %d, want 400 invalid_token", bad, resp.StatusCode)
		}
	}
}

// A leaked reset link must not outlive the owner's own securing actions:
// confirming an address move kills the link bound to the abandoned mailbox,
// and a Settings password change kills the account's outstanding link
// (astra2 review).
func TestResetLinkDiesWhenOwnerSecuresAccount(t *testing.T) {
	a, fm := emailTestApp(t)
	activeUser(t, a, fm, "leak@secure.io")
	fm.drain()

	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "leak@secure.io", "password": emailPW,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	}

	// The leak: a forgot-password link for the current address.
	resp = a.do(http.MethodPost, "/api/v1/auth/forgot-password", map[string]any{"email": "leak@secure.io"})
	resp.Body.Close()
	leaked := tokenFromJob(t, fm.next(t), "/#reset?t=")

	// The owner flees the mailbox: PATCH email, then click the confirmation
	// (two mails go out — confirm to the new address, notice to the old).
	resp = a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"current_password": emailPW, "email": "safe@secure.io"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH email = %d, want 200", resp.StatusCode)
	}
	confirmTok := ""
	for i := 0; i < 2; i++ {
		j := fm.next(t)
		if j.To == "safe@secure.io" {
			confirmTok = tokenFromJob(t, j, "/#confirm?t=")
		}
	}
	if confirmTok == "" {
		t.Fatal("no confirmation mail for the new address")
	}
	resp = a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": confirmTok})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", resp.StatusCode)
	}

	// The leaked link was addressed to the abandoned mailbox: dead link.
	resp = a.do(http.MethodPost, "/api/v1/auth/reset-password", map[string]any{
		"token": leaked, "password": "attacker-password-1",
	})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "invalid_token" {
		t.Errorf("leaked link after the address move = %d, want 400 invalid_token", resp.StatusCode)
	}

	// And a Settings password change revokes a fresh link the same way.
	resp = a.do(http.MethodPost, "/api/v1/auth/forgot-password", map[string]any{"email": "safe@secure.io"})
	resp.Body.Close()
	leaked2 := tokenFromJob(t, fm.next(t), "/#reset?t=")
	resp = a.do(http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current": emailPW, "new": "owner-chosen-password-1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("password change = %d, want 204", resp.StatusCode)
	}
	resp = a.do(http.MethodPost, "/api/v1/auth/reset-password", map[string]any{
		"token": leaked2, "password": "attacker-password-2",
	})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "invalid_token" {
		t.Errorf("link after the password change = %d, want 400 invalid_token", resp.StatusCode)
	}
}

// --- sign-up confirmation: the whole pending lifecycle ---------------------------

func TestSignupConfirmPendingLifecycle(t *testing.T) {
	a, fm := emailTestApp(t)

	resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "fresh@pending.io", "name": "Newbie", "password": emailPW,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d, want 201", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	if m["email_confirmation_required"] != true {
		t.Errorf("register body = %v, want email_confirmation_required", m)
	}
	if m["email"] != "fresh@pending.io" {
		t.Errorf("register echo = %v", m["email"])
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			t.Fatal("pending sign-up must not receive a session cookie")
		}
	}
	// Consume the registration mail now (its token dies to the resend
	// below; consuming early also keeps later expectQuiet calls honest).
	regJob := fm.next(t)
	oldTok := tokenFromJob(t, regJob, "/#confirm?t=")
	if !strings.Contains(regJob.Body, "activate your mullsmith account") {
		t.Errorf("sign-up confirm mail must read as an activation:\n%s", regJob.Body)
	}

	// DB shape: unconfirmed + NULL email + address in unconfirmed_email.
	ctx := context.Background()
	users, err := a.db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("users = %d, want 1", len(users))
	}
	pending := &users[0]
	if pending.Status != "unconfirmed" || pending.Email != "" || pending.UnconfirmedEmail != "fresh@pending.io" {
		t.Errorf("pending row = %+v, want unconfirmed/NULL/confirmed-address", pending)
	}

	// Login is impossible before confirming.
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "fresh@pending.io", "password": emailPW,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("login pre-confirm = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// A second sign-up on the pending address: 409 (both-column pre-flight).
	resp = a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "fresh@pending.io", "name": "Copycat", "password": emailPW,
	})
	if resp.StatusCode != http.StatusConflict || apiErrCode(t, resp) != "duplicate_email" {
		t.Errorf("dup pending register = %d, want 409 duplicate_email", resp.StatusCode)
	}

	// forgot-password on a pending address: generic 200, NO mail (invisible).
	resp = a.do(http.MethodPost, "/api/v1/auth/forgot-password", map[string]any{"email": "fresh@pending.io"})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("forgot pending = %d, want generic 200", resp.StatusCode)
	}
	resp.Body.Close()
	fm.expectQuiet(t)

	// resend-confirm: same generic 200, one fresh mail, the old token dies.
	respPending := a.do(http.MethodPost, "/api/v1/auth/resend-confirm", map[string]any{"email": "fresh@pending.io"})
	bodyPending := readAll(t, respPending)
	if respPending.StatusCode != http.StatusOK {
		t.Fatalf("resend = %d, want 200", respPending.StatusCode)
	}
	resendJob := fm.next(t)
	if resendJob.To != "fresh@pending.io" {
		t.Errorf("resend mail to %q", resendJob.To)
	}
	newTok := tokenFromJob(t, resendJob, "/#confirm?t=")
	if newTok == oldTok {
		t.Error("resend must reissue a fresh token")
	}

	resp = a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": oldTok})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "invalid_token" {
		t.Errorf("superseded confirm token = %d, want 400 invalid_token", resp.StatusCode)
	}
	respUnknown := a.do(http.MethodPost, "/api/v1/auth/resend-confirm", map[string]any{"email": "who@cares.io"})
	bodyUnknown := readAll(t, respUnknown)
	if string(bodyPending) != string(bodyUnknown) {
		t.Errorf("resend envelopes differ:\n%s\n%s", bodyPending, bodyUnknown)
	}
	fm.expectQuiet(t) // nothing for the unknown address

	// Confirm with the live token → active, address moved, login works.
	resp = a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": newTok})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	got, _ := a.db.UserByID(ctx, pending.ID)
	if got.Email != "fresh@pending.io" || got.UnconfirmedEmail != "" || got.Status != "enabled" {
		t.Errorf("after confirm: %+v", got)
	}
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "fresh@pending.io", "password": emailPW,
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login post-confirm = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Settings email change --------------------------------------------------------

func TestSettingsEmailChangeFlow(t *testing.T) {
	a, fm := emailTestApp(t)
	activeUser(t, a, fm, "old@change.io")
	fm.drain()

	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "old@change.io", "password": emailPW,
	})
	resp.Body.Close()

	newAddr := "new@change.io"
	resp = a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"current_password": emailPW, "email": newAddr})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH email = %d, want 200", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	if m["email"] != "old@change.io" || m["unconfirmed_email"] != newAddr {
		t.Errorf("PATCH response = %v, want old confirmed + pending new", m)
	}

	// Two mails: confirmation to the NEW address, courtesy notice to the OLD.
	confirmTok := ""
	var toOld bool
	for i := 0; i < 2; i++ {
		j := fm.next(t)
		switch j.To {
		case newAddr:
			confirmTok = tokenFromJob(t, j, "/#confirm?t=")
			// Field feedback: the Settings mail must NOT read like a
			// sign-up activation (no account-creation, no deletion promise).
			if !strings.Contains(j.Body, "email address change") ||
				strings.Contains(j.Body, "activate your mullsmith account") ||
				strings.Contains(j.Body, "deleted") {
				t.Errorf("settings confirm mail wording wrong:\n%s", j.Body)
			}
		case "old@change.io":
			// Requested, not done — the notice fires BEFORE any change.
			toOld = strings.Contains(j.Subject, "change was requested") &&
				!strings.Contains(j.Body, "was changed")
		default:
			t.Errorf("unexpected mail to %q", j.To)
		}
	}
	if confirmTok == "" || !toOld {
		t.Fatalf("need confirm→new and notice→old, got confirm=%q old=%v", confirmTok, toOld)
	}

	// Login still rides the OLD confirmed address.
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "old@change.io", "password": emailPW,
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login on old address mid-change = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// A second PATCH re-points the pending target: the first PATCH's token
	// is now just another dead link (token-email binding, plan §3).
	resp = a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"current_password": emailPW, "email": "third@change.io"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second PATCH = %d", resp.StatusCode)
	}
	if m = decodeBody(t, resp); m["email"] != "old@change.io" || m["unconfirmed_email"] != "third@change.io" {
		t.Errorf("second PATCH response = %v", m)
	}
	resp = a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": confirmTok})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "invalid_token" {
		t.Errorf("superseded settings token = %d, want 400 invalid_token", resp.StatusCode)
	}

	// Cancel clears the pending target (self-service escape).
	resp = a.do(http.MethodPost, "/api/v1/auth/me/cancel-email", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel-email = %d, want 200", resp.StatusCode)
	}
	m = decodeBody(t, resp)
	if m["unconfirmed_email"] != "" {
		t.Errorf("after cancel = %v, want pending cleared", m)
	}
}

func TestSettingsEmailConfirmSwapsAndTakenReports(t *testing.T) {
	a, fm := emailTestApp(t)
	activeUser(t, a, fm, "mov@swap.io")
	fm.drain()

	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "mov@swap.io", "password": emailPW,
	})
	resp.Body.Close()

	target := "prize@swap.io"
	resp = a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"current_password": emailPW, "email": target})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Fish out the confirmation mail for the target address.
	var tok string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case j := <-fm.ch:
			if j.To == target {
				tok = tokenFromJob(t, j, "/#confirm?t=")
			}
		case <-time.After(100 * time.Millisecond):
		}
		if tok != "" {
			break
		}
	}
	if tok == "" {
		t.Fatal("no confirmation mail for the new address")
	}

	// Someone claims the address first (direct DB write — the race state).
	ctx := context.Background()
	rival := &store.User{
		Email: target, Name: "Rival", PasswordHash: "x", Status: "enabled",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := a.db.CreateUser(ctx, rival); err != nil {
		t.Fatal(err)
	}

	resp = a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": tok})
	if resp.StatusCode != http.StatusConflict || apiErrCode(t, resp) != "email_taken" {
		t.Fatalf("taken confirm = %d, want 409 email_taken", resp.StatusCode)
	}

	// Pending cleared, account otherwise untouched.
	me := decodeBody(t, a.do(http.MethodGet, "/api/v1/auth/me", nil))
	if me["unconfirmed_email"] != "" || me["email"] != "mov@swap.io" {
		t.Errorf("after taken: %v, want pending cleared, email unchanged", me)
	}
}

func TestSettingsEmailChangeDirectWhenDisabled(t *testing.T) {
	// Features off (the default): today's behaviour — the PATCH rewrites
	// email in place, login follows it immediately.
	a := emailOffApp(t)
	resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "plain@off.io", "name": "Off", "password": emailPW,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register off = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	resp = a.do(http.MethodPatch, "/api/v1/auth/me", map[string]any{"current_password": emailPW, "email": "moved@off.io"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH off = %d", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	if m["email"] != "moved@off.io" {
		t.Errorf("off PATCH = %v, want direct write", m)
	}
	resp = a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "moved@off.io", "password": emailPW,
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login on moved address = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- rate limits --------------------------------------------------------------------

func TestForgotRateLimits(t *testing.T) {
	a, _ := emailTestApp(t)

	// Per-email bucket (3/15 min) fires while the per-IP bucket (5/15 min)
	// still admits — proving both exist and the email one is the tighter.
	for i := 0; i < 3; i++ {
		resp := a.do(http.MethodPost, "/api/v1/auth/forgot-password",
			map[string]any{"email": "spaced@limit.io"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("forgot #%d = %d, want 200", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := a.do(http.MethodPost, "/api/v1/auth/forgot-password",
		map[string]any{"email": "spaced@limit.io"})
	if resp.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp) != "rate_limited" {
		t.Errorf("forgot #4 same email = %d, want 429 rate_limited", resp.StatusCode)
	}
	resp.Body.Close()

	// Fresh address still fine (IP bucket has 1 of 5 left).
	resp = a.do(http.MethodPost, "/api/v1/auth/forgot-password",
		map[string]any{"email": "other@limit.io"})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("forgot different email = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Sixth request (fifth admitted): the IP bucket (5/15 min, shared by
	// all four addresses above) closes on whatever comes next.
	resp = a.do(http.MethodPost, "/api/v1/auth/forgot-password",
		map[string]any{"email": "beyond@limit.io"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("forgot past IP bucket = %d, want 429", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestResetAndConfirmRejectGarbageTokens(t *testing.T) {
	a, _ := emailTestApp(t)
	resp := a.do(http.MethodPost, "/api/v1/auth/reset-password",
		map[string]any{"token": strings.Repeat("t", 5000), "password": emailPW})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "invalid_token" {
		t.Errorf("oversized reset token = %d, want 400 invalid_token", resp.StatusCode)
	}
	resp = a.do(http.MethodPost, "/api/v1/auth/confirm-email", map[string]any{"token": "never-minted"})
	if resp.StatusCode != http.StatusBadRequest || apiErrCode(t, resp) != "invalid_token" {
		t.Errorf("unknown confirm token = %d, want 400 invalid_token", resp.StatusCode)
	}
}
