package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gabkeep/internal/auth"
	"gabkeep/internal/id"
	"gabkeep/internal/mail"
	"gabkeep/internal/scratch"
	"gabkeep/internal/store"
)

// --- helpers ---------------------------------------------------------------

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
		writeAPIError(w, r, http.StatusUnsupportedMediaType,
			"unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request",
			"request body must be a JSON object with only known fields")
		return false
	}
	return true
}

// newSessionAndCookie creates the DB session row and sets the cookie. Always
// mints a brand-new token — sessions are never reused across logins (fixation,
// spec §9.1).
func (s *Server) newSessionAndCookie(w http.ResponseWriter, r *http.Request, user *store.User, remembered bool) error {
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		return err
	}
	now := time.Now()
	var expires time.Time
	if remembered {
		expires = now.Add(s.cfg.Auth.RememberTTL.D())
	} else {
		expires = now.Add(s.cfg.Auth.SessionTTL.D())
	}
	sess := &store.Session{
		IDHash:     hash,
		UserID:     user.ID,
		CreatedAt:  now,
		ExpiresAt:  expires,
		Remembered: remembered,
		LastSeenAt: now,
		UserAgent:  truncate(r.UserAgent(), 255),
	}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		return err
	}
	http.SetCookie(w, auth.SessionCookie(raw, remembered, expires, secureRequest(s.cfg, r)))
	// L-6 hygiene: crossing INTO authenticated rotates the double-submit
	// token too, so a token planted or sniffed pre-login never outlives
	// the transition. Double-submit keeps no server state, and the SPA
	// re-reads the cookie per request (api.js cookieValue), so rotation
	// costs nothing. A rand failure leaves the middleware-minted token
	// standing — still valid, just unrotated.
	if tok, err := auth.NewCSRFToken(); err == nil {
		http.SetCookie(w, auth.CSRFCookie(tok, secureRequest(s.cfg, r)))
	}
	return nil
}

// rememberRequested honours the login/registration checkbox, disabled when
// auth.remember_login is false (spec §9.1).
func (s *Server) rememberRequested(requested bool) bool {
	return requested && s.cfg.Auth.RememberLogin
}

// hashRun performs Argon2 work through the global gate (audit finding 4).
// It returns true when f ran; on false the HTTP response is already written
// (429 when the gate is saturated; nothing when the client hung up) and the
// caller must return. A hash implementation failure — vanishing entropy,
// say — is a 500.
func (s *Server) hashRun(w http.ResponseWriter, r *http.Request, f func() error) bool {
	err := s.hashGate.Run(r.Context(), f)
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrHashBusy):
		w.Header().Set("Retry-After", "2")
		writeAPIError(w, r, http.StatusTooManyRequests, "server_busy",
			"too many password operations in flight, try again shortly")
	case errors.Is(err, context.Canceled):
		// Queue spot released for a caller that left; no one to answer.
	default:
		s.logger.Error("password hashing failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
	}
	return false
}

// --- POST /api/v1/auth/register (spec §5.1, §8) --------------------------------

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ip := ClientIP(r)
	if !s.limiter.Allow("register:"+ip, 5, time.Hour) {
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many registration attempts")
		return
	}

	var body struct {
		Email      string `json:"email"`
		Name       string `json:"name"`
		Password   string `json:"password"`
		RememberMe bool   `json:"remember_me"`
		Invite     string `json:"invite"`
	}
	if !readJSON(w, r, &body) {
		return
	}

	switch s.cfg.Auth.Registration {
	case "closed":
		writeAPIError(w, r, http.StatusForbidden, "registration_closed", "accounts are created by the administrator")
		return
	case "invite-only":
		if strings.TrimSpace(body.Invite) == "" {
			writeAPIError(w, r, http.StatusForbidden, "invite_required", "an invite code is required")
			return
		}
	}

	email, ok := auth.NormalizeEmail(body.Email)
	name := strings.TrimSpace(body.Name)
	if !ok {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_email", "invalid email address")
		return
	}
	if name == "" || utf8.RuneCountInString(name) > 120 {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_name", "name must be 1-120 characters")
		return
	}
	if err := auth.CheckPasswordPolicy(body.Password); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "weak_password", err.Error())
		return
	}

	// Invite eligibility pre-flight (audit finding 4): a used/unknown code
	// fails here without burning a hash. store.Register keeps consuming
	// atomically, so two registrations racing on one code still reject
	// exactly one — the loser just pays for a wasted hash.
	if s.cfg.Auth.Registration == "invite-only" {
		ok, err := s.store.InviteAvailable(r.Context(), strings.TrimSpace(body.Invite))
		if err != nil {
			s.logger.Error("invite pre-check failed", "error", err, "rid", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if !ok {
			writeAPIError(w, r, http.StatusForbidden, "invite_invalid", "invite code missing, used or unknown")
			return
		}
	}

	// Availability pre-flight: with email on, the address lands in
	// unconfirmed_email, which carries no UNIQUE index — EmailAvailable's
	// both-column check is the arbiter (plan §5). It runs in both modes so
	// the global budget below and the hash can never be spent on a request
	// whose row is certain to lose to the duplicate constraint; the store's
	// UNIQUE index stays the final arbiter for the racing-identical-email
	// case either way.
	avail, err := s.store.EmailAvailable(r.Context(), email)
	if err != nil {
		s.logger.Error("email availability check failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !avail {
		writeAPIError(w, r, http.StatusConflict, "duplicate_email", "an account with this email already exists")
		return
	}

	// Global creation budget (audit1 M-2), open mode only: the per-IP bucket
	// above is worthless to a proxy pool, and every accepted request means a
	// DB row plus — with email on — a confirmation mail from the operator's
	// SMTP host. Consumed HERE, after every validation and pre-flight and
	// before the hash: one unit spent equals one row actually created, so
	// garbage or duplicate attempts cannot burn the budget and lock out
	// legitimate sign-ups. Invite-only mode needs no budget — the codes are
	// the budget. -1 disables the check.
	if s.cfg.Auth.Registration == "open" {
		if limit := s.cfg.Auth.SignupsPerHour(); limit > 0 &&
			!s.limiter.Allow("signup:global", limit, time.Hour) {
			s.logger.Warn("global sign-up budget exhausted", "limit", limit, "ip", ip)
			writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited",
				"sign-ups are temporarily rate limited, try again later")
			return
		}
	}

	var hash string
	if !s.hashRun(w, r, func() error {
		var err error
		hash, err = auth.HashPassword(body.Password, s.cfg.Auth.Argon2)
		return err
	}) {
		return
	}

	now := time.Now()
	user := &store.User{
		ID:           id.New(),
		Name:         name,
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if s.cfg.Email.Enabled {
		// Pending sign-up (plan §5): the address lives ONLY in
		// unconfirmed_email until the confirm click; nothing
		// credential-facing can see this account yet.
		user.UnconfirmedEmail = email
		user.Status = "disabled"
	} else {
		user.Email = email
		user.Status = "active"
	}
	if err := s.store.Register(r.Context(), user, strings.TrimSpace(body.Invite)); err != nil {
		switch {
		case errors.Is(err, store.ErrUserExists):
			writeAPIError(w, r, http.StatusConflict, "duplicate_email", "an account with this email already exists")
		case errors.Is(err, store.ErrInviteInvalid):
			writeAPIError(w, r, http.StatusForbidden, "invite_invalid", "invite code missing, used or unknown")
		default:
			s.logger.Error("register failed", "error", err, "rid", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	// Scratchpad provisioning (spec §8). On filesystem failure the row
	// SURVIVES — no rollback runs here, and none is promised (audit1 I-2:
	// the old "best-effort rollback; M6 adds a reconciler" comment described
	// machinery that never existed). The state is benign and self-healing:
	// the account logs in normally, and both the upload path and code-exec
	// re-run EnsureUserDirs, so the workspace appears on first use. In
	// email mode the only casualty is the confirm mail never queued — the
	// pending row is swept after 24 h (§9.17) and register works again.
	if err := scratch.EnsureUserDirs(s.cfg.Scratchpad.Root, user.ID); err != nil {
		s.logger.Error("scratchpad provisioning failed", "error", err, "user", user.ID)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// Confirm-before-login (plan §5): mail goes out only after the row and
	// its workspace exist; no session, no cookie — the account has proven
	// nothing about its mailbox yet. A token-mint failure here is logged,
	// not fatal: resend-confirm from the login screen is the escape hatch.
	if s.cfg.Email.Enabled {
		s.sendConfirmMail(r.Context(), user.ID, email, confirmPurposeSignup)
		s.logger.Info("user registered, confirmation pending", "user", user.ID, "ip", ip)
		writeJSON(w, http.StatusCreated, map[string]any{
			"email":                       email, // echoed from the request
			"email_confirmation_required": true,
		})
		return
	}

	if err := s.newSessionAndCookie(w, r, user, s.rememberRequested(body.RememberMe)); err != nil {
		s.logger.Error("session create failed", "error", err)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.logger.Info("user registered", "user", user.ID, "ip", ip)
	writeJSON(w, http.StatusCreated, map[string]any{"id": user.ID, "email": user.Email, "name": user.Name})
}

// --- POST /api/v1/auth/login -------------------------------------------------------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := ClientIP(r)
	if !s.limiter.Allow("login:"+ip, 10, time.Minute) {
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
		return
	}

	var body struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		RememberMe bool   `json:"remember_me"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	// Length gate BEFORE the lock check, DB lookup, hashing and lock-key
	// construction: the retained lock key is built from this string, and
	// unbounded inputs accumulated attacker-sized entries in the lockout
	// map (audit2 finding 1). Length only at this stage — a syntax verdict
	// here would be an "is-that-an-address" oracle beside the §9.1
	// equal-cost failure paths.
	if len(email) > auth.MaxEmailLen {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "email is too long")
		return
	}
	// Canonicalize for the lookup (audit1 L-3): stored addresses are
	// normalized, so a variant spelling must find the same row AND feed the
	// same "acct:" lockout key. An unparseable string keeps its raw form:
	// no stored row can match it (rows are always normalized), so it
	// answers exactly like an unknown address — no oracle, and the lock key
	// stays length-capped by the gate above.
	if norm, normOK := auth.NormalizeEmail(email); normOK {
		email = norm
	}
	lockKey := "acct:" + email

	if s.locks.Locked(lockKey) {
		writeAPIError(w, r, http.StatusTooManyRequests, "account_locked", "too many failed attempts, try later")
		return
	}

	user, err := s.store.UserByEmail(r.Context(), email)
	if err != nil {
		s.logger.Error("login lookup failed", "error", err)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if user == nil {
		// Both failure paths take exactly one gate-wrapped hash, so a
		// saturated gate answers 429 identically whether or not the email
		// exists — timing equalisation holds under load too (spec §9.1).
		if !s.hashRun(w, r, func() error { auth.DummyVerify(s.cfg.Auth.Argon2); return nil }) {
			return
		}
		s.locks.Fail(lockKey, s.cfg.Auth.MaxFailedLogins, 15*time.Minute)
		s.logger.Warn("login failed", "reason", "unknown_email", "ip", ip)
		invalidCredentials(w, r)
		return
	}
	ok := false
	if !s.hashRun(w, r, func() error { ok = auth.VerifyPassword(body.Password, user.PasswordHash); return nil }) {
		return
	}
	if !ok || user.Status != "active" {
		s.locks.Fail(lockKey, s.cfg.Auth.MaxFailedLogins, 15*time.Minute)
		s.logger.Warn("login failed", "reason", "bad_password", "ip", ip, "user", user.ID)
		invalidCredentials(w, r)
		return
	}

	s.locks.Reset(lockKey)
	if err := s.newSessionAndCookie(w, r, user, s.rememberRequested(body.RememberMe)); err != nil {
		s.logger.Error("session create failed", "error", err)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.logger.Info("login ok", "user", user.ID, "ip", ip)
	writeJSON(w, http.StatusOK, map[string]any{"id": user.ID, "email": user.Email, "name": user.Name})
}

// invalidCredentials is the single generic 401 for every failure mode that
// could distinguish accounts (spec §9.1).
func invalidCredentials(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
}

// --- POST /api/v1/auth/logout ---------------------------------------------------------

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	if sess != nil {
		if err := s.store.DeleteSession(r.Context(), sess.IDHash); err != nil {
			s.logger.Warn("logout delete failed", "error", err)
		}
	}
	http.SetCookie(w, auth.ClearSessionCookie(secureRequest(s.cfg, r)))
	// L-6: rotate on the way OUT as well — the token that authenticated
	// this session's mutations is discarded, and the SPA still has a
	// usable token for the next login (rotation, not clearing).
	if tok, err := auth.NewCSRFToken(); err == nil {
		http.SetCookie(w, auth.CSRFCookie(tok, secureRequest(s.cfg, r)))
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- GET / PATCH /api/v1/auth/me ---------------------------------------------------

func (s *Server) mePayload(user *store.User) map[string]any {
	used, err := scratch.DirSize(s.cfg.Scratchpad.Root, user.ID)
	if err != nil {
		used = 0
	}
	return map[string]any{
		"id":                    user.ID,
		"email":                 user.Email,
		"unconfirmed_email":     user.UnconfirmedEmail, // "" = nothing pending
		"name":                  user.Name,
		"created_at":            user.CreatedAt.UTC().Format(time.RFC3339),
		"default_system_prompt": user.DefaultSystemPrompt,
		"quota": map[string]any{
			"used_bytes":  used,
			"total_bytes": int64(s.cfg.Scratchpad.UserQuota),
		},
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mePayload(UserFrom(r.Context())))
}

// handleMeUpdate lets the settings page change name, email and the default
// system prompt in one save. Every field is optional (pointer); password
// rotation is a separate endpoint (/auth/password) because it revokes
// sessions. Email rules mirror registration exactly so the two paths can
// never disagree.
func (s *Server) handleMeUpdate(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		Name                *string `json:"name"`
		Email               *string `json:"email"`
		DefaultSystemPrompt *string `json:"default_system_prompt"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name, email, prompt := user.Name, user.Email, user.DefaultSystemPrompt
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if name == "" || utf8.RuneCountInString(name) > 120 {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_name", "name must be 1-120 characters")
			return
		}
	}
	if body.Email != nil {
		norm, ok := auth.NormalizeEmail(*body.Email)
		if !ok {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_email", "invalid email address")
			return
		}
		email = norm
	}
	if body.DefaultSystemPrompt != nil {
		prompt = strings.TrimSpace(*body.DefaultSystemPrompt)
		if utf8.RuneCountInString(prompt) > maxSystemPromptLen {
			writeAPIError(w, r, http.StatusBadRequest, "system_prompt_too_long", "system prompt too long")
			return
		}
	}

	// Email-change routing (plan §5). Features ON: a changed address never
	// writes the email column — it becomes the pending target plus a bound
	// confirm mail, and the OLD (confirmed) mailbox gets a courtesy notice,
	// so a stolen cookie cannot quietly reroute the account. Features OFF:
	// the direct write also clears any stale pending target a previous
	// enabled run left behind.
	pendingAddr := ""
	if email != user.Email {
		if s.cfg.Email.Enabled {
			if !s.limiter.Allow("confirmset:"+ClientIP(r), 3, emailRateWindow) {
				writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
				return
			}
			avail, err := s.store.EmailAvailable(r.Context(), email)
			if err != nil {
				s.logger.Error("email availability check failed", "error", err, "rid", RequestIDFrom(r.Context()))
				writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			if !avail {
				writeAPIError(w, r, http.StatusConflict, "duplicate_email", "an account with this email already exists")
				return
			}
			pendingAddr = email
			email = user.Email // the confirmed address is not moved until the click
		}
	}

	if err := s.store.UpdateProfile(r.Context(), user.ID, name, email, prompt, time.Now()); err != nil {
		if errors.Is(err, store.ErrUserExists) {
			writeAPIError(w, r, http.StatusConflict, "duplicate_email", "an account with this email already exists")
			return
		}
		s.logger.Error("profile update failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	switch {
	case pendingAddr != "":
		if err := s.store.SetUnconfirmedEmail(r.Context(), user.ID, pendingAddr, time.Now()); err != nil {
			s.logger.Error("pending email set failed", "error", err, "user", user.ID)
			writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		s.sendConfirmMail(r.Context(), user.ID, pendingAddr, confirmPurposeChange)
		if user.Email != "" {
			subject, msg := mail.AddressChangedMail()
			s.mailer.Send(user.Email, subject, msg)
		}
	case email != user.Email: // features off, address rewritten directly
		if err := s.store.SetUnconfirmedEmail(r.Context(), user.ID, "", time.Now()); err != nil {
			s.logger.Warn("stale pending email clear failed", "error", err, "user", user.ID)
		}
	}
	fresh, err := s.store.UserByID(r.Context(), user.ID)
	if err != nil || fresh == nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.logger.Info("profile updated", "user", user.ID,
		"email_changed", fresh.Email != user.Email, "email_pending", pendingAddr != "")
	writeJSON(w, http.StatusOK, s.mePayload(fresh))
}

// --- POST /api/v1/auth/password ----------------------------------------------------------

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	// Dedicated password-change throttle (audit finding 4): every call is
	// two Argon2 operations, so the generic per-user API budget (120/min)
	// is far too loose here. Per-user is the primary key — the endpoint
	// proves identity — with a looser per-IP layer behind it.
	const pwWindow = 15 * time.Minute
	if !s.limiter.Allow("pw:"+user.ID, 5, pwWindow) ||
		!s.limiter.Allow("pwip:"+ClientIP(r), 20, pwWindow) {
		w.Header().Set("Retry-After", "900")
		writeAPIError(w, r, http.StatusTooManyRequests, "pw_change_limited",
			"too many password changes, try again later")
		return
	}
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	pwOK := false
	if !s.hashRun(w, r, func() error { pwOK = auth.VerifyPassword(body.Current, user.PasswordHash); return nil }) {
		return
	}
	if !pwOK {
		writeAPIError(w, r, http.StatusForbidden, "invalid_password", "current password is incorrect")
		return
	}
	if err := auth.CheckPasswordPolicy(body.New); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	var hash string
	if !s.hashRun(w, r, func() error {
		var err error
		hash, err = auth.HashPassword(body.New, s.cfg.Auth.Argon2)
		return err
	}) {
		return
	}
	if err := s.store.SetPassword(r.Context(), user.ID, hash, time.Now()); err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Revoke every other session; rotate the current one (spec §9.1).
	if _, err := s.store.DeleteSessionsForUser(r.Context(), user.ID, sessionFrom(r.Context()).IDHash); err != nil {
		s.logger.Warn("session revocation failed", "error", err)
	}
	if err := s.store.DeleteSession(r.Context(), sessionFrom(r.Context()).IDHash); err != nil {
		s.logger.Warn("session rotate delete failed", "error", err)
	}
	remembered := sessionFrom(r.Context()).Remembered
	if err := s.newSessionAndCookie(w, r, user, remembered); err != nil {
		s.logger.Error("session rotate create failed", "error", err)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.logger.Info("password changed; sessions revoked", "user", user.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /api/v1/auth/me/delete ------------------------------------------------

// handleDeleteAccount ends the account, now, at the user's own request. The
// row deletion cascades the entire database footprint through the FKs
// (0001–0008: sessions, conversations → messages → tool calls & attachment
// links, auth_tokens — all ON DELETE CASCADE, FK enforcement always on in
// the DSN). The scratchpad directory goes immediately as well: unlike the
// admin CLI — whose bytes age out through the orphan sweep — self-service
// must not leave a gone user's files lying around until the next pass.
// Sessions died with the row, so the cookie is explicitly cleared: the
// browser should not walk around with a phantom that 401s on the next click.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	deleted, err := s.store.DeleteUser(r.Context(), user.ID)
	if err != nil {
		s.logger.Error("account deletion failed", "error", err, "user", user.ID,
			"rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !deleted {
		// Raced an admin delete; nothing left to remove — same empty answer.
		s.logger.Warn("account delete: row already gone", "user", user.ID)
	}
	http.SetCookie(w, auth.ClearSessionCookie(secureRequest(s.cfg, r)))
	if err := scratch.RemoveUserDirs(s.cfg.Scratchpad.Root, user.ID); err != nil {
		// The account is already gone; the orphan sweep collects the bytes.
		s.logger.Error("scratchpad removal after account delete failed", "error", err, "user", user.ID)
	}
	s.logger.Info("account deleted", "user", user.ID)
	w.WriteHeader(http.StatusNoContent)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
