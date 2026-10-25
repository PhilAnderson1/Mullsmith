package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"gabkeep/internal/config"
)

// --- request id -------------------------------------------------------------

type ctxKey int

const ctxKeyRequestID ctxKey = 1

// RequestIDFrom returns the request id assigned by the requestID middleware.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := make([]byte, 8)
		if _, err := rand.Read(id); err != nil { // crypto/rand never fails on linux
			id = []byte("00000000")
		}
		rid := hex.EncodeToString(id)
		w.Header().Set("X-Request-Id", rid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, rid)))
	})
}

// --- request logging ---------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush passes through so SSE routes keep working behind this chain.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		lvl := slog.LevelInfo
		switch {
		case sw.status >= 500:
			lvl = slog.LevelError
		case sw.status >= 400:
			lvl = slog.LevelWarn
		}
		logger.Log(r.Context(), lvl, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"ms", time.Since(start).Milliseconds(),
			"rid", RequestIDFrom(r.Context()),
			"ip", ClientIP(r),
		)
	})
}

// ClientIP resolves the caller address. X-Forwarded-For is honored only when
// the TCP peer is a trusted proxy (spec §9.10): the leftmost hop that is not
// itself trusted is the client.
func ClientIP(r *http.Request) string {
	peer := peerAddr(r)
	if len(trustedFrom(r.Context())) == 0 || peer == nil || !ipTrusted(*peer, trustedFrom(r.Context())) {
		return peerString(peer)
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peerString(peer)
	}
	for _, hop := range strings.Split(xff, ",") {
		hop = strings.TrimSpace(hop)
		addr, err := netip.ParseAddr(hop)
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if !ipTrusted(addr, trustedFrom(r.Context())) {
			return addr.String()
		}
	}
	return peerString(peer)
}

func peerAddr(r *http.Request) *netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	a := addr.Unmap()
	return &a
}

func peerString(a *netip.Addr) string {
	if a == nil {
		return "?"
	}
	return a.String()
}

func ipTrusted(a netip.Addr, nets []netip.Prefix) bool {
	for _, p := range nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// trustedNets rides in the request context, injected by securityHeaders so
// ClientIP needs no server struct reference.
type trustedCtxKey struct{}

func trustedFrom(ctx context.Context) []netip.Prefix {
	n, _ := ctx.Value(trustedCtxKey{}).([]netip.Prefix)
	return n
}

// --- panic recovery -----------------------------------------------------------

func recoverPanics(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// Detail server-side only; nothing internal to the client (spec §9.12).
				logger.Error("panic recovered",
					"panic", fmt.Sprint(rec),
					"rid", RequestIDFrom(r.Context()),
					"stack", string(debug.Stack()),
				)
				writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- security headers (spec §9.8) ----------------------------------------------

func securityHeaders(cfg *config.Config) func(http.Handler) http.Handler {
	csp := "default-src 'self'; script-src 'self'; style-src 'self'; " +
		"img-src 'self' blob:; connect-src 'self'; font-src 'self'; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'; " +
		"frame-ancestors 'none'; frame-src 'none'"
	if cfg.Server.TLS.Enabled {
		csp += "; upgrade-insecure-requests"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Permissions-Policy",
				"camera=(), microphone=(), geolocation=(), payment=(), usb=(), clipboard-write=(self)")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			if cfg.Server.TLS.Enabled && cfg.Server.HSTSMaxAge > 0 {
				h.Set("Strict-Transport-Security",
					fmt.Sprintf("max-age=%d; includeSubDomains", cfg.Server.HSTSMaxAge))
			}
			h.Set("Cache-Control", cacheControl(r.URL.Path))

			// Expose trusted-proxy nets to ClientIP via context.
			if len(cfg.TrustedNets) > 0 {
				r = r.WithContext(context.WithValue(r.Context(), trustedCtxKey{}, cfg.TrustedNets))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// cacheControl: API, health and the app shell are never cached; fingerprinted
// static assets may be (immutable once content-hashed in M2).
func cacheControl(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/"), path == "/healthz", path == "/":
		return "no-store"
	case strings.HasPrefix(path, "/assets/"):
		return "public, max-age=3600" // becomes immutable+1y with content hashes (M2)
	default:
		return "no-store"
	}
}

// --- body limit ------------------------------------------------------------------

func bodyLimit(next http.Handler, max int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The upload route enforces its own, larger max_file_size cap while
		// streaming the part to disk — it must not be clamped to
		// max_body_bytes (§9.11).
		upload := r.Method == http.MethodPost && r.URL.Path == "/api/v1/files"
		if r.Body != nil && !upload {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// IsBodyTooLarge reports MaxBytesReader violations surfaced by handlers later.
func IsBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}
