// Package server wires the HTTP surface: routes, middleware chain, embedded
// static assets (spec §5, §9.2, §9.8).
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gabkeep/internal/agent"
	"gabkeep/internal/auth"
	"gabkeep/internal/config"
	"gabkeep/internal/llm"
	"gabkeep/internal/mail"
	"gabkeep/internal/runner"
	"gabkeep/internal/store"
	"gabkeep/internal/tools"
	"gabkeep/web"
)

// Server is the assembled gabkeep HTTP application.
type Server struct {
	cfg        *config.Config
	store      *store.DB
	logger     *slog.Logger
	appVer     string
	indexHTML  []byte // app shell with build-versioned asset URLs, built once
	limiter    *auth.Limiter
	locks      *auth.LoginLocks
	gens       *genRegistry
	generator  *agent.Generator
	tools      *tools.Registry
	remembered *approvalMemory
	httpSrv    *http.Server
	// hashGate bounds concurrent Argon2 work globally (audit finding 4).
	hashGate *auth.HashGate
	// uploadSem bounds in-flight uploads across ALL users — temp files and
	// quota decisions together; nil = unbounded (limits.concurrent_uploads:
	// 0). commitMu serializes every application-managed quota decision —
	// each upload's fresh-usage-check-then-rename, fs_write's write
	// section, and code_run staging — so concurrent writers can never spend
	// the same bytes twice (audit finding 5, extended by audit2 finding 3).
	uploadSem chan struct{}
	commitMu  sync.Mutex
	// mailer is the async SMTP outbox for confirmation/reset mail; nil
	// when email.enabled is false. Every email handler gates on the config
	// switch first, so the nil mailer is unreachable; sendConfirmMail and
	// friends lean on Outbox.Send's nil-receiver safety anyway.
	mailer *mail.Outbox
}

// hashQueue is how many callers may wait behind the hash gate before new
// arrivals get 429. Small on purpose: hashing is sub-second, so a healthy
// queue drains in a blink and a deep one means a flood worth refusing.
const hashQueue = 32

// buildHashGate maps limits.hash_concurrency (house cap syntax:
// absent→default, -1→disabled, 0/other negatives rejected at load) onto a
// gate. Reading the raw field matters: 0 (struct-literal test configs,
// never passed through Load) selects the safe default, while a negative is
// a disabled — unbounded — gate, so unrelated tests never meet a surprise
// 429.
func buildHashGate(cfg *config.Config) *auth.HashGate {
	n := cfg.Limits.HashConcurrency
	if n == 0 {
		n = config.DefaultHashConcurrency
	}
	if n < 0 {
		return nil
	}
	return auth.NewHashGate(n, hashQueue)
}

// buildUploadGate maps limits.concurrent_uploads (house cap syntax like the
// other knobs; default 8) onto a channel gate. 8 comfortably exceeds what a
// browser fires in parallel per host (~6), so honest sessions never see it,
// while in-flight upload temp bytes stay bounded at 8 × max_file_size no
// matter what the per-minute request bucket allows.
func buildUploadGate(cfg *config.Config) chan struct{} {
	n := cfg.Limits.ConcurrentUploads
	if n == 0 {
		n = config.DefaultConcurrentUploads
	}
	if n < 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// New builds the full handler chain. Middleware order (outermost first):
// requestID → logRequests → recoverPanics → securityHeaders → csrfProtect →
// bodyLimit → mux. mailer may be nil (email features disabled).
func New(cfg *config.Config, db *store.DB, logger *slog.Logger, appVer string,
	code runner.Runner, mailer *mail.Outbox,
) *Server {
	s := &Server{
		cfg:        cfg,
		store:      db,
		logger:     logger,
		appVer:     appVer,
		limiter:    auth.NewLimiter(),
		locks:      auth.NewLoginLocks(),
		gens:       newGenRegistry(),
		generator:  &agent.Generator{Cfg: cfg, LLM: llm.NewClient(cfg, nil)},
		remembered: newApprovalMemory(),
		hashGate:   buildHashGate(cfg),
		uploadSem:  buildUploadGate(cfg),
		indexHTML:  versionedIndex(appVer),
		mailer:     mailer,
	}
	// Built after s so the tool layer shares commitMu: upload commits,
	// fs_write, files_share copies and code_run staging make their quota
	// decisions under one lock (audit2 finding 3).
	s.tools = tools.BuildFor(cfg, logger, nil, code, &s.commitMu)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/meta", s.handleMeta)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.Handle("POST /api/v1/auth/logout", s.authRequired(s.handleLogout))
	mux.Handle("GET /api/v1/auth/me", s.authRequired(s.handleMe))
	mux.Handle("PATCH /api/v1/auth/me", s.authRequired(s.handleMeUpdate))
	mux.Handle("POST /api/v1/auth/password", s.authRequired(s.handleChangePassword))
	// email_features routes (plan §5): always registered; the handlers 404
	// while email.enabled is false.
	mux.HandleFunc("POST /api/v1/auth/forgot-password", s.handleForgotPassword)
	mux.HandleFunc("POST /api/v1/auth/reset-password", s.handleResetPassword)
	mux.HandleFunc("POST /api/v1/auth/confirm-email", s.handleConfirmEmail)
	mux.HandleFunc("POST /api/v1/auth/resend-confirm", s.handleResendConfirm)
	mux.Handle("POST /api/v1/auth/me/cancel-email", s.authRequired(s.handleCancelEmail))
	// Self-service account deletion: always available (not an email-feature),
	// CSRF-guarded like every mutation; the UI adds a confirmation dialog.
	mux.Handle("POST /api/v1/auth/me/delete", s.authRequired(s.handleDeleteAccount))
	mux.Handle("GET /api/v1/conversations", s.authRequired(s.handleConversationsList))
	mux.Handle("POST /api/v1/conversations", s.authRequired(s.handleConversationCreate))
	mux.Handle("GET /api/v1/conversations/{id}", s.authRequired(s.handleConversationGet))
	mux.Handle("PATCH /api/v1/conversations/{id}", s.authRequired(s.handleConversationPatch))
	mux.Handle("DELETE /api/v1/conversations/{id}", s.authRequired(s.handleConversationDelete))
	mux.Handle("GET /api/v1/conversations/{id}/messages", s.authRequired(s.handleConversationMessages))
	mux.Handle("POST /api/v1/conversations/{id}/messages", s.authRequired(s.handleChatSend))
	mux.Handle("POST /api/v1/messages/{id}/regenerate", s.authRequired(s.handleRegenerate))
	mux.Handle("POST /api/v1/generations/{id}/abort", s.authRequired(s.handleAbort))
	mux.Handle("POST /api/v1/generations/{id}/approvals", s.authRequired(s.handleApproval))
	mux.Handle("POST /api/v1/files", s.authRequired(s.handleFileUpload))
	mux.Handle("GET /api/v1/files", s.authRequired(s.handleFileList))
	mux.Handle("GET /api/v1/files/download", s.authRequired(s.handleFileDownload))
	mux.Handle("DELETE /api/v1/files", s.authRequired(s.handleFileDelete))
	mux.HandleFunc("GET /assets/", s.staticAssetsHandler())
	// Bare "/" is the lowest-priority sink: it catches "/" (index), unknown
	// paths (JSON 404) and wrong verbs on known paths (JSON 405). Go's
	// ServeMux exposes no NotFound override, so we own this explicitly.
	mux.HandleFunc("/", s.sinkHandler())

	var h http.Handler = mux
	h = bodyLimit(h, int64(cfg.Limits.MaxBodyBytes))
	h = csrfProtect(cfg, logger)(h)
	h = securityHeaders(cfg)(h)
	h = recoverPanics(h, logger)
	h = logRequests(h, logger)
	h = requestID(h) // outermost: the id must exist in ctx before logging/recovery read it

	s.httpSrv = &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second, // slowloris floor (spec §9.11)
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(), // SSE routes will disable per-route later
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s
}

func (s *Server) Serve(ln net.Listener) error { return s.httpSrv.Serve(ln) }

// artifactTTL / workTTL: the configured expiry ages with a defensive
// floor — struct-literal configs (tests, embedders) that never ran
// config.Load must not mean "expire instantly" or "never expires"
// (fail-closed doctrine: generated output always expires).
func (s *Server) artifactTTL() time.Duration {
	if d := time.Duration(s.cfg.Scratchpad.ArtifactTTL); d > 0 {
		return d
	}
	return 168 * time.Hour
}

func (s *Server) workTTL() time.Duration {
	if d := time.Duration(s.cfg.Scratchpad.WorkTTL); d > 0 {
		return d
	}
	return 24 * time.Hour
}

// Handler exposes the assembled handler chain (used by tests).
func (s *Server) Handler() http.Handler { return s.httpSrv.Handler }

func (s *Server) ServeTLS(ln net.Listener, certFile, keyFile string) error {
	return s.httpSrv.ServeTLS(ln, certFile, keyFile)
}

func (s *Server) Shutdown(ctx context.Context) error { return s.httpSrv.Shutdown(ctx) }

// AbortGenerations cancels all live generations (SIGTERM path in main):
// SSE handlers observe ctx.Done, persist partial answers like an abort and
// return, which lets Shutdown finish inside the grace period instead of
// timing out on still-open streams.
func (s *Server) AbortGenerations() int { return s.gens.abortAll() }

// WaitIdle blocks until in-flight generations have finished (or ctx is
// done). Called after Shutdown so streams can persist partial answers
// before the database closes.
func (s *Server) WaitIdle(ctx context.Context) { s.gens.waitIdle(ctx) }

// --- handlers ---------------------------------------------------------------

// handleHealth is information-free on purpose (spec §5.4).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleMeta exposes only what the login/register screens need.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"app":     "gabkeep",
		"version": s.appVer,
		"models":  s.cfg.LLM.Models,
		// default_model lets the picker name the model a conversation with
		// no explicit choice actually runs on.
		"default_model":  s.cfg.LLM.Model,
		"registration":   s.cfg.Auth.Registration,
		"remember_login": s.cfg.Auth.RememberLogin,
		// email_features drives the SPA's forgot-link / confirmation copy
		// exactly like remember_login and registration drive theirs.
		"email_features": s.cfg.Email.Enabled,
		"show_reasoning": s.cfg.LLM.ShowReasoning,
		"limits": map[string]any{
			"max_message_chars": s.cfg.Limits.MaxMessageChars,
		},
		"tools": map[string]bool{
			"web_search": s.cfg.Tools.WebSearch.Enabled,
			"web_fetch":  s.cfg.Tools.WebFetch.Enabled,
			"code_run":   s.cfg.Scratchpad.CodeExec.Enabled,
		},
		"files": map[string]any{
			"max_file_size":      int64(s.cfg.Scratchpad.MaxFileSize),
			"max_attachments":    s.cfg.Limits.MaxAttachments,
			"blocked_extensions": s.cfg.Scratchpad.BlockExtensions,
		},
	})
}

// staticAssetsHandler serves embedded files under /assets/. A trailing-slash
// directory request never yields a listing (spec §9.2). The app modules are
// served from a startup-rewritten copy (stampedModules) so their import
// specifiers rotate with the build stamp too; everything else rides the
// plain embedded server.
func (s *Server) staticAssetsHandler() http.HandlerFunc {
	files := http.FileServerFS(web.FS)
	stamped := stampedModules(buildStamp(s.appVer))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			s.handleNotFound(w, r)
			return
		}
		if body, ok := stamped[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = w.Write(body)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// moduleSpecs are the absolute import specifiers the app's ES modules use
// between each other (app.js imports all three, sse.js imports api.js).
var moduleSpecs = []string{"/assets/app/api.js", "/assets/app/sse.js", "/assets/app/markdown.js"}

// stampedModules returns the /assets/app/*.js bodies with every quoted
// import specifier carrying ?v=stamp, so module imports rotate with each
// deploy. Without it the shell's stamp reaches app.js but app.js's own
// imports ride bare URLs, and the one-hour asset cache pairs NEW app.js
// with OLD api.js after a contract-changing deploy — it bit once as the
// 405-delete incident and again as "api.deleteAccount is not a function"
// (2026-10-28), which is what this rewriting closes. Rewriting the import
// strings (rather than an inline import map) keeps CSP at script-src 'self'.
// An unusable stamp (dev build) yields nil: the bare embedded files serve.
func stampedModules(stamp string) map[string][]byte {
	if stamp == "" {
		return nil
	}
	out := make(map[string][]byte, 4)
	for _, name := range []string{"app.js", "api.js", "sse.js", "markdown.js"} {
		b, err := web.FS.ReadFile("assets/app/" + name)
		if err != nil {
			return nil
		}
		h := string(b)
		for _, spec := range moduleSpecs {
			h = strings.ReplaceAll(h, `"`+spec+`"`, `"`+spec+`?v=`+stamp+`"`)
		}
		out["/assets/app/"+name] = []byte(h)
	}
	return out
}

// buildStamp sanitizes the ldflags version for embedding in URLs; "" means
// no usable stamp (nothing may rotate, shell and modules serve bare).
func buildStamp(ver string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return -1
	}, ver)
}

// versionedIndex is the app shell with a build stamp appended to the
// /assets/* URLs. Assets cache for an hour (middleware cacheControl), so
// without rotating the URLs a deploy leaves browsers on the PREVIOUS
// app.js until expiry. app.js's own module imports are rotated separately,
// by the request-time rewriting in stampedModules.
func versionedIndex(ver string) []byte {
	b, err := web.FS.ReadFile("index.html")
	if err != nil {
		return nil
	}
	safe := buildStamp(ver)
	if safe == "" {
		return b
	}
	h := string(b)
	h = strings.ReplaceAll(h, `href="/assets/styles.css"`, `href="/assets/styles.css?v=`+safe+`"`)
	h = strings.ReplaceAll(h, `src="/assets/app/app.js"`, `src="/assets/app/app.js?v=`+safe+`"`)
	return []byte(h)
}

// sinkHandler dispatches everything the specific patterns did not match.
func (s *Server) sinkHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if s.indexHTML != nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write(s.indexHTML)
				return
			}
			http.ServeFileFS(w, r, web.FS, "index.html")
			return
		}
		if isKnownPath(r.URL.Path) {
			s.handleMethodNotAllowed(w, r)
			return
		}
		s.handleNotFound(w, r)
	}
}

// isKnownPath reports whether the path belongs to a registered route (under a
// different verb than the caller used). Kept in sync with the mux table above.
func isKnownPath(p string) bool {
	switch {
	case p == "/", p == "/healthz", p == "/api/v1/meta", p == "/assets", p == "/api/v1/files":
		return true
	case strings.HasPrefix(p, "/assets/"):
		return true
	case strings.HasPrefix(p, "/api/v1/files/"):
		return true
	}
	return false
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, http.StatusNotFound, "not_found", "resource not found")
}

func (s *Server) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

// --- response helpers --------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    message,
			"request_id": RequestIDFrom(r.Context()),
		},
	})
}
