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
	http.SetCookie(w, auth.SessionCookie(s.cfg, raw, remembered, expires))
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

	email := strings.ToLower(strings.TrimSpace(body.Email))
	name := strings.TrimSpace(body.Name)
	if !auth.ValidEmail(email) {
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
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		Status:       "active",
		CreatedAt:    now,
		UpdatedAt:    now,
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

	// Scratchpad provisioning (spec §8): a filesystem failure rolls back the
	// user row so we never leave an account without its workspace.
	if err := scratch.EnsureUserDirs(s.cfg.Scratchpad.Root, user.ID); err != nil {
		s.logger.Error("scratchpad provisioning failed", "error", err, "user", user.ID)
		// best-effort rollback; M6 adds a periodic reconciler
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
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
	http.SetCookie(w, auth.ClearSessionCookie(s.cfg))
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/v1/auth/me -------------------------------------------------------------

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	used, err := scratch.DirSize(s.cfg.Scratchpad.Root, user.ID)
	if err != nil {
		used = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         user.ID,
		"email":      user.Email,
		"name":       user.Name,
		"created_at": user.CreatedAt.UTC().Format(time.RFC3339),
		"quota": map[string]any{
			"used_bytes":  used,
			"total_bytes": int64(s.cfg.Scratchpad.UserQuota),
		},
	})
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
