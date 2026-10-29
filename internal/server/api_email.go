package server

// Password reset + email confirmation endpoints (email_features plan §5).
// All four public routes are registered always and answer 404 while
// email.enabled is false (mirrors the registration-mode pattern — the JS
// stays one code path). Enumeration doctrine: forgot-password and
// resend-confirm answer byte-identical 200 for every outcome; dead links
// collapse to one generic invalid_token; per-email rate buckets are
// consumed for every syntactically valid address, whether or not an
// account exists, so bucket state is no oracle either.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/mail"
	"mullsmith/internal/store"
)

// Token lifetimes (plan §5): the confirm TTL deliberately equals the
// pending sign-up expiry window — the account never outlives its link.
const (
	resetTokenTTL   = 30 * time.Minute
	confirmTokenTTL = 24 * time.Hour
	emailRateWindow = 15 * time.Minute
)

// emailEnabled gates the public email endpoints: features off means the
// routes do not exist (404, not 403 — nothing to learn about a switch).
func (s *Server) emailEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.Email.Enabled {
		s.handleNotFound(w, r)
		return false
	}
	return true
}

// mailQueued is the single byte-identical answer every forgot-password and
// resend-confirm request gets, found-user or not (the oracle-killer; no
// dummy Argon2 here — nothing password-shaped is verified).
func mailQueued(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "If that address needs a link, one is on its way.",
	})
}

// Confirmation purposes for sendConfirmMail: same token machinery, but the
// mail must read differently — activating an account vs. moving an existing
// account's address (field feedback: one wording for both confused users).
const (
	confirmPurposeSignup = "signup"
	confirmPurposeChange = "change"
)

// sendConfirmMail mints the address-bound confirmation token and queues the
// mail for a pending address (sign-up or Settings change). Errors are
// logged, never surfaced to the requester: the pending row exists and
// resend-confirm is the documented escape hatch.
func (s *Server) sendConfirmMail(ctx context.Context, userID, email, purpose string) {
	raw, err := s.store.CreateToken(ctx, store.TokenConfirmEmail, userID, email,
		time.Now().Add(confirmTokenTTL))
	if err != nil {
		s.logger.Error("confirm token mint failed", "error", err, "user", userID)
		return
	}
	var subject, body string
	if purpose == confirmPurposeChange {
		subject, body = mail.ConfirmationMailChange(s.cfg.Email.BaseURL, raw)
	} else {
		subject, body = mail.ConfirmationMail(s.cfg.Email.BaseURL, raw)
	}
	s.mailer.Send(email, subject, body)
}

// --- POST /api/v1/auth/forgot-password ---------------------------------------

func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !s.emailEnabled(w, r) {
		return
	}
	ip := ClientIP(r)
	if !s.limiter.Allow("forgotip:"+ip, 5, emailRateWindow) {
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
		return
	}

	var body struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	// Length gate before the address becomes a limiter key (audit2 finding 1
	// doctrine, same as login); syntax nonsense simply answers the generic
	// 200 — there is nothing to mail and nothing to reveal.
	if len(email) > auth.MaxEmailLen {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "email is too long")
		return
	}
	norm, ok := auth.NormalizeEmail(email)
	if !ok {
		mailQueued(w)
		return
	}
	email = norm // lookups and limiter keys use the canonical form (L-3)
	// Per-email bucket consumed for EVERY valid address, existing or not:
	// were it touched only for real accounts, a rotated-IP attacker could
	// read existence off the 4th request's 429.
	if !s.limiter.Allow("forgotmail:"+email, 3, emailRateWindow) {
		w.Header().Set("Retry-After", "900")
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
		return
	}

	user, err := s.store.UserByEmail(r.Context(), email)
	if err != nil {
		// Pre-existence failure: identical on every address, safe to surface.
		s.logger.Error("forgot-password lookup failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Only confirmed, active accounts get mail: pending sign-ups hold NULL
	// email and miss the lookup entirely (their escape is resend-confirm);
	// admin-disabled ones stay disabled — an inbox cannot overrule that.
	if user != nil && user.Status == "active" {
		raw, err := s.store.CreateToken(r.Context(), store.TokenResetPassword,
			user.ID, user.Email, time.Now().Add(resetTokenTTL))
		if err != nil {
			// Post-existence errors must not answer 500 — that would be an
			// existence oracle for whoever can trip them.
			s.logger.Error("reset token mint failed", "error", err, "user", user.ID)
			mailQueued(w)
			return
		}
		subject, msg := mail.ResetMail(s.cfg.Email.BaseURL, raw)
		s.mailer.Send(user.Email, subject, msg)
		s.logger.Info("password reset mail queued", "user", user.ID, "ip", ip)
	}
	mailQueued(w)
}

// --- POST /api/v1/auth/reset-password ------------------------------------------

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.emailEnabled(w, r) {
		return
	}
	if !s.limiter.Allow("resetip:"+ClientIP(r), 10, emailRateWindow) {
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
		return
	}

	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	// Policy BEFORE consumption: a weak new password must not burn the one
	// shot and force a fresh forgot-password round.
	if err := auth.CheckPasswordPolicy(body.Password); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	if body.Token == "" || len(body.Token) > auth.MaxEmailLen {
		invalidToken(w, r)
		return
	}
	userID, _, found, err := s.store.ConsumeToken(r.Context(), store.TokenResetPassword,
		body.Token, time.Now())
	if err != nil {
		s.logger.Error("reset token consume failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !found {
		// Absent, expired, used — one message, no flavours.
		invalidToken(w, r)
		return
	}
	var hash string
	if !s.hashRun(w, r, func() error {
		var err error
		hash, err = auth.HashPassword(body.Password, s.cfg.Auth.Argon2)
		return err
	}) {
		return
	}
	// One transaction: new hash + status='active' + revoke ALL sessions.
	if err := s.store.CompletePasswordReset(r.Context(), userID, hash, time.Now()); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			invalidToken(w, r) // account deleted between mail and click
			return
		}
		s.logger.Error("password reset completion failed", "error", err, "user", userID)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Deliberately no session here: completing a reset proves mailbox
	// control, not browser continuity — the user logs in with the new
	// password like everyone else.
	s.logger.Info("password reset completed; all sessions revoked", "user", userID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- POST /api/v1/auth/confirm-email ---------------------------------------------

func (s *Server) handleConfirmEmail(w http.ResponseWriter, r *http.Request) {
	if !s.emailEnabled(w, r) {
		return
	}
	if !s.limiter.Allow("confirmip:"+ClientIP(r), 10, emailRateWindow) {
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Token == "" || len(body.Token) > auth.MaxEmailLen {
		invalidToken(w, r)
		return
	}
	found, err := s.store.ConfirmEmail(r.Context(), body.Token, time.Now())
	switch {
	case errors.Is(err, store.ErrEmailTakenNow):
		writeAPIError(w, r, http.StatusConflict, "email_taken",
			"that address is already in use; ask the administrator for help")
	case err != nil:
		s.logger.Error("confirm failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
	case !found:
		invalidToken(w, r)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// --- POST /api/v1/auth/resend-confirm ----------------------------------------------

func (s *Server) handleResendConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.emailEnabled(w, r) {
		return
	}
	ip := ClientIP(r)
	if !s.limiter.Allow("resendip:"+ip, 5, time.Hour) {
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if len(email) > auth.MaxEmailLen {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "email is too long")
		return
	}
	norm, ok := auth.NormalizeEmail(email)
	if !ok {
		mailQueued(w)
		return
	}
	email = norm // canonical form for the lookup and limiter key (L-3)
	if !s.limiter.Allow("resendmail:"+email, 3, time.Hour) {
		w.Header().Set("Retry-After", "3600")
		writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try later")
		return
	}

	// Only never-confirmed sign-ups (disabled + NULL email) can resend:
	// a confirmed address is forgot-password's business, and a mid-Settings
	// change is re-pointed by a second PATCH, not by this public route.
	user, err := s.store.UserByPendingEmail(r.Context(), email)
	if err != nil {
		s.logger.Error("resend lookup failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if user != nil {
		// Reissue invalidates the older link (plan §3). The 24 h
		// pending-account window is NOT extended — link and account die
		// together at 24 h from sign-up.
		s.sendConfirmMail(r.Context(), user.ID, email, confirmPurposeSignup)
		s.logger.Info("confirmation mail re-queued", "user", user.ID, "ip", ip)
	}
	mailQueued(w) // identical whether or not anything matched
}

// --- POST /api/v1/auth/me/cancel-email (authenticated) ---------------------------

func (s *Server) handleCancelEmail(w http.ResponseWriter, r *http.Request) {
	if !s.emailEnabled(w, r) {
		return
	}
	user := UserFrom(r.Context())
	now := time.Now()
	if err := s.store.SetUnconfirmedEmail(r.Context(), user.ID, "", now); err != nil {
		s.logger.Error("cancel pending email failed", "error", err, "user", user.ID)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.store.DeleteUnusedTokens(r.Context(), store.TokenConfirmEmail, user.ID); err != nil {
		s.logger.Warn("confirm token cleanup failed", "error", err, "user", user.ID)
	}
	fresh, err := s.store.UserByID(r.Context(), user.ID)
	if err != nil || fresh == nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.logger.Info("pending email change cancelled", "user", user.ID)
	writeJSON(w, http.StatusOK, s.mePayload(fresh))
}

// invalidToken is the single generic answer for every dead-link flavour
// (absent, expired, used, re-pointed, cancelled) of both kinds.
func invalidToken(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, http.StatusBadRequest, "invalid_token",
		"that link is invalid or has already been used")
}
