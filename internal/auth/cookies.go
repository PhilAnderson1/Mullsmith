package auth

import (
	"net/http"
	"time"
)

// Cookie and header names (spec §9.1, §9.5).
const (
	SessionCookieName = "gabkeep_session"
	CSRFCookieName    = "gabkeep_csrf"
	CSRFHeaderName    = "X-Gabkeep-CSRF"
)

// Cookies carry the Secure flag derived PER REQUEST by the server
// (server.secureRequest): set when the connection is TLS, or when a trusted
// proxy declares the browser leg was https. There is deliberately no
// cookie_secure config knob — a per-request derivation is never stale and
// never forgotten (§9.1). The secure argument is required so every call site
// states which connection it is answering.

// SessionCookie builds the session cookie. When remembered, it is persistent
// (Expires/Max-Age matching the DB row — surviving browser close, spec §9.1);
// otherwise it is a browser-session cookie with no expiry attribute.
func SessionCookie(rawToken string, remembered bool, expiresAt time.Time, secure bool) *http.Cookie {
	c := &http.Cookie{
		Name:     SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	if remembered {
		c.Expires = expiresAt
		c.MaxAge = int(time.Until(expiresAt).Seconds())
	}
	return c
}

// ClearSessionCookie expires the cookie immediately.
func ClearSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	}
}

// CSRFCookie is readable by same-origin JS on purpose — it carries no
// authority; its value must be mirrored into the CSRF header (double-submit).
func CSRFCookie(token string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCSRFCookie expires the CSRF cookie.
func ClearCSRFCookie(secure bool) *http.Cookie {
	c := CSRFCookie("", secure)
	c.Expires = time.Unix(0, 0)
	c.MaxAge = -1
	return c
}
