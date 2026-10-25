package auth

import (
	"net/http"
	"time"

	"gabkeep/internal/config"
)

// Cookie and header names (spec §9.1, §9.5).
const (
	SessionCookieName = "gabkeep_session"
	CSRFCookieName    = "gabkeep_csrf"
	CSRFHeaderName    = "X-Gabkeep-CSRF"
)

// SessionCookie builds the session cookie. When remembered, it is persistent
// (Expires/Max-Age matching the DB row — surviving browser close, spec §9.1);
// otherwise it is a browser-session cookie with no expiry attribute.
func SessionCookie(cfg *config.Config, rawToken string, remembered bool, expiresAt time.Time) *http.Cookie {
	// #nosec G124 -- Secure is config-driven: auth.cookie_secure, default
	// true (§9.1); plain-http dev boxes opt out deliberately.
	c := &http.Cookie{
		Name:     SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   cfg.Auth.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	}
	if remembered {
		c.Expires = expiresAt
		c.MaxAge = int(time.Until(expiresAt).Seconds())
	}
	return c
}

// ClearSessionCookie expires the cookie immediately.
func ClearSessionCookie(cfg *config.Config) *http.Cookie {
	// #nosec G124 -- see SessionCookie: Secure is config-driven (§9.1).
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   cfg.Auth.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	}
}

// CSRFCookie is readable by same-origin JS on purpose — it carries no
// authority; its value must be mirrored into the CSRF header (double-submit).
func CSRFCookie(cfg *config.Config, token string) *http.Cookie {
	// #nosec G124 -- see SessionCookie: Secure is config-driven (§9.1).
	return &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		Secure:   cfg.Auth.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCSRFCookie expires the CSRF cookie.
func ClearCSRFCookie(cfg *config.Config) *http.Cookie {
	// #nosec G124 -- see SessionCookie: Secure is config-driven (§9.1).
	c := CSRFCookie(cfg, "")
	c.Expires = time.Unix(0, 0)
	c.MaxAge = -1
	return c
}
