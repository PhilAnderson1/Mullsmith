package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
	"mullsmith/internal/store"
)

// --- CSRF (spec §9.5) ----------------------------------------------------

// csrfProtect implements double-submit: every mutating /api/v1 request must
// carry X-Mullsmith-CSRF matching the mullsmith_csrf cookie. The cookie is issued
// on any GET (the SPA reads it on load), rotated away from anything the
// server trusts implicitly.
func csrfProtect(cfg *config.Config, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			csrfCookie, _ := r.Cookie(auth.CSRFCookieName)

			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				// Issue a token when absent or malformed.
				if csrfCookie == nil || len(csrfCookie.Value) < 32 {
					if tok, err := auth.NewCSRFToken(); err == nil {
						http.SetCookie(w, auth.CSRFCookie(tok, secureRequest(cfg, r)))
					}
				}
				next.ServeHTTP(w, r)
				return
			}

			if !strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}

			header := r.Header.Get(auth.CSRFHeaderName)
			if csrfCookie == nil || header == "" || !auth.ConstantTimeEqual(csrfCookie.Value, header) {
				logger.Warn("csrf rejection",
					"path", r.URL.Path, "ip", ClientIP(r), "rid", RequestIDFrom(r.Context()))
				writeAPIError(w, r, http.StatusForbidden, "csrf_failed", "missing or invalid CSRF token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// --- session auth (spec §9.1) ----------------------------------------------

type authCtxKey struct{}

type authInfo struct {
	user    *store.User
	session *store.Session
}

// UserFrom returns the authenticated user, or nil.
func UserFrom(ctx context.Context) *store.User {
	if a, ok := ctx.Value(authCtxKey{}).(authInfo); ok {
		return a.user
	}
	return nil
}

func sessionFrom(ctx context.Context) *store.Session {
	if a, ok := ctx.Value(authCtxKey{}).(authInfo); ok {
		return a.session
	}
	return nil
}

// requireAuth validates the session cookie against the database. The cookie's
// presence is never trusted on its own; expiry and user status are re-checked
// per request. Remembered sessions get throttled sliding renewal: while more
// than an hour has passed since last contact and less than half the TTL
// remains, expires_at is extended (never past the absolute cap) and the
// cookie re-issued.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookieName)
		if err != nil || cookie.Value == "" {
			unauthenticated(w, r)
			return
		}
		lookup, err := s.store.SessionByHash(r.Context(), auth.HashToken(cookie.Value))
		if err != nil {
			s.logger.Error("session lookup failed", "error", err, "rid", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if lookup == nil || time.Now().After(lookup.ExpiresAt) {
			unauthenticated(w, r)
			return
		}
		user, err := s.store.UserByID(r.Context(), lookup.UserID)
		if err != nil {
			s.logger.Error("user lookup failed", "error", err, "rid", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if user == nil || user.Status != store.StatusEnabled {
			unauthenticated(w, r)
			return
		}

		// Per-user API token bucket (§9.11 app_limits.requests_per_minute —
		// configured but never enforced, audit finding 3). Keyed by user,
		// not IP: the identity is already proven here, and IP-keying is
		// hostage to NAT and proxy topology. Chat streaming routes are the
		// documented exception ("non-chat endpoints") — they answer to the
		// hourly chat bucket and the generation caps instead.
		if max := s.cfg.AppLimits.RequestsPerMinute; max > 0 && !isChatStreamRoute(r) &&
			!s.limiter.Allow("api:"+user.ID, max, time.Minute) {
			w.Header().Set("Retry-After", "1")
			writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited",
				"too many requests, please slow down")
			return
		}

		now := time.Now()
		if lookup.Remembered {
			half := s.cfg.Auth.RememberTTL.D() / 2
			remaining := time.Until(lookup.ExpiresAt)
			if remaining < half && now.Sub(lookup.LastSeenAt) > time.Hour {
				newExpiry := now.Add(s.cfg.Auth.RememberTTL.D())
				if cap := lookup.CreatedAt.Add(s.cfg.Auth.RememberAbsoluteTTL.D()); newExpiry.After(cap) {
					newExpiry = cap
				}
				if err := s.store.TouchSession(r.Context(), lookup.IDHash, newExpiry, now); err != nil {
					s.logger.Warn("session renewal failed", "error", err)
				} else {
					lookup.ExpiresAt = newExpiry
					http.SetCookie(w, auth.SessionCookie(cookie.Value, true, newExpiry, secureRequest(s.cfg, r)))
				}
			}
		}

		ctx := context.WithValue(r.Context(), authCtxKey{}, authInfo{user: user, session: lookup})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isChatStreamRoute matches the two POST routes that start an SSE
// generation (send / regenerate). They are exempt from the per-minute API
// bucket by spec design; see the hourly bucket in runGeneration.
func isChatStreamRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	return (strings.HasPrefix(p, "/api/v1/conversations/") && strings.HasSuffix(p, "/messages")) ||
		(strings.HasPrefix(p, "/api/v1/messages/") && strings.HasSuffix(p, "/regenerate"))
}

// authRequired wraps a handler with session auth.
func (s *Server) authRequired(h http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(h).ServeHTTP
}

func unauthenticated(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required")
}
