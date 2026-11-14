// Package server wires the HTTP surface: routes, middleware chain, embedded
// static assets (spec §5, §9.2, §9.8).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/chat"
	"mullsmith/internal/config"
	"mullsmith/internal/mail"
	"mullsmith/internal/managed"
	"mullsmith/internal/runner"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
	"mullsmith/web"
)

// Server is the assembled mullsmith HTTP application.
type Server struct {
	cfg          *config.Config
	store        *store.DB
	logger       *slog.Logger
	appVer       string
	indexHTML    []byte // app shell with commit-stamped asset URLs, built once
	assets       *assetBundle
	limiter      *auth.Limiter
	signupBudget *auth.SignupBudget
	locks        *auth.LoginLocks
	chat         *chat.Service
	tools        *tools.Registry
	mcpOAuth     *tools.MCPOAuthManager
	httpSrv      *http.Server
	// hashGate bounds concurrent Argon2 work globally (audit finding 4).
	hashGate *auth.HashGate
	// uploadSem bounds in-flight multipart bodies. files owns the shared
	// quota critical section for all application-managed writes.
	uploadSem chan struct{}
	downloads *userTransferGate
	uploads   *userTransferGate
	files     *managed.Service
	// mailer is the async SMTP outbox for confirmation/reset mail; nil
	// when email.enabled is false. Every email handler gates on the config
	// switch first, so the nil mailer is unreachable; sendConfirmMail and
	// friends lean on Outbox.Send's nil-receiver safety anyway.
	mailer *mail.Outbox
	// discoveredWindows caches the endpoint-reported context windows per
	// model (lower-cased id) while an entry leaves context_size to auto;
	// a pinned entry never consults it (spec §2.2/§6.1).
	discoveredWindows *windowsCache
	// windowProbe is the seam for the /models lookup; tests substitute
	// canned answers.
	windowProbe func(context.Context) (map[string]int, bool)
	// probeMu guards probeDone, the single-flight marker (R5): overlapping
	// refresh intent joins the probe already running instead of starting
	// another GET /models. probeDone is non-nil exactly while one runs.
	probeMu   sync.Mutex
	probeDone chan struct{}
}

// hashQueue is how many callers may wait behind the hash gate before new
// arrivals get 429. Small on purpose: hashing is sub-second, so a healthy
// queue drains in a blink and a deep one means a flood worth refusing.
const hashQueue = 32

// buildHashGate consumes the prepared cap: a negative value disables it.
func buildHashGate(cfg *config.Config) *auth.HashGate {
	n := cfg.AppLimits.HashConcurrency
	if n < 0 {
		return nil
	}
	return auth.NewHashGate(n, hashQueue)
}

// buildUploadGate maps app_limits.concurrent_uploads (house cap syntax like the
// other knobs; default 8) onto a channel gate. 8 comfortably exceeds what a
// browser fires in parallel per host (~6), so honest sessions never see it,
// while in-flight upload temp bytes stay bounded at 8 × max_file_size no
// matter what the per-minute request bucket allows.
func buildUploadGate(cfg *config.Config) chan struct{} {
	n := cfg.AppLimits.ConcurrentUploads
	if n < 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// New builds the full handler chain. Middleware order (outermost first):
// requestID → trustedProxyContext → logRequests → recoverPanics →
// securityHeaders → request admission → csrfProtect → bodyLimit → routing. mailer may be nil (email features disabled).
func New(cfg *config.Config, db *store.DB, logger *slog.Logger, appVer string,
	code runner.Runner, mailer *mail.Outbox, assetCommit string,
) (*Server, error) {
	if err := cfg.Prepare(); err != nil {
		return nil, err
	}
	for _, warning := range cfg.Thinking.Warnings() {
		logger.Warn("thinking control", "warning", warning)
	}
	db = db.WithRecordLimits(cfg.AppLimits.MaxConversationsPerUser, cfg.AppLimits.MaxMessagesPerUser)
	assets, err := newAssetBundle(web.FS, assetCommit)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:       cfg,
		store:     db,
		logger:    logger,
		appVer:    appVer,
		limiter:   auth.NewLimiter(logger),
		locks:     auth.NewLoginLocks(logger),
		hashGate:  buildHashGate(cfg),
		uploadSem: buildUploadGate(cfg),
		downloads: newUserTransferGate(cfg.AppLimits.ConcurrentDownloadsPerUser),
		uploads:   newUserTransferGate(cfg.AppLimits.ConcurrentUploadsPerUser),
		indexHTML: assets.index,
		assets:    assets,
		mailer:    mailer,
	}
	s.files = managed.New(int64(cfg.Scratchpad.UserQuota), cfg.Scratchpad.MaxEntries)
	if cfg.Auth.Registration == "open" {
		s.signupBudget = auth.NewSignupBudget(cfg.Auth.SignupsPerHour())
	}
	s.tools = tools.BuildFor(cfg, logger, nil, code, s.files)
	oauthManager, err := tools.NewMCPOAuthManager(cfg, db, logger)
	if err != nil {
		return nil, err
	}
	s.mcpOAuth = oauthManager
	if s.mcpOAuth != nil {
		s.mcpOAuth.SetReservedToolNames(s.tools.Names())
		s.tools.SetUserToolProvider(s.mcpOAuth)
		s.mcpOAuth.StartRestore()
	}
	s.chat = chat.New(cfg, db, logger, s.tools)
	s.chat.Window = s.overflowLine

	// Auto window (§2.2 llm.max_context_tokens: -1): the seam is set here;
	// the first probe itself belongs to StartContextWindowProbe so tests
	// can install canned answers before anything runs.
	s.windowProbe = s.probeEndpointWindows
	s.discoveredWindows = newWindowsCache()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/auth/config", s.handleAuthConfig)
	mux.Handle("GET /api/v1/client-config", s.authRequired(s.handleClientConfig))
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
	mux.Handle("GET /api/v1/mcp/connections", s.authRequired(s.handleMCPConnections))
	mux.Handle("POST /api/v1/mcp/connections/{id}/connect", s.authRequired(s.handleMCPConnect))
	mux.Handle("DELETE /api/v1/mcp/connections/{id}", s.authRequired(s.handleMCPDisconnect))
	mux.HandleFunc("GET /api/v1/mcp/oauth/callback", s.handleMCPOAuthCallback)
	mux.Handle("GET /api/v1/conversations", s.authRequired(s.handleConversationsList))
	mux.Handle("POST /api/v1/conversations", s.authRequired(s.handleConversationCreate))
	// Bulk housekeeping ("Delete older than…") on the collection itself,
	// distinguished from the list by method alone.
	mux.Handle("DELETE /api/v1/conversations", s.authRequired(s.handleConversationsPrune))
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
	mux.Handle("GET /api/v1/files/image", s.authRequired(s.handleFileImage))
	mux.Handle("DELETE /api/v1/files", s.authRequired(s.handleFileDelete))
	mux.HandleFunc("GET /assets/", s.staticAssetsHandler())
	mux.HandleFunc("GET /{$}", s.handleIndex)

	var h http.Handler = routingErrors(mux)

	h = bodyLimit(h, int64(cfg.Server.MaxBodyBytes))
	h = csrfProtect(cfg, logger)(h)
	h = limitRequests(h, cfg.Server.ConcurrentRequests)
	h = securityHeaders(cfg)(h)
	h = recoverPanics(h, logger)
	h = logRequests(h, logger)
	h = trustedProxyContext(cfg)(h)
	h = requestID(h) // outermost: the id must exist in ctx before logging/recovery read it

	s.httpSrv = &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second, // slowloris floor (spec §9.11)
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.HTTPDeadline(), // default 60s; -1 = off (astra2 finding 1); SSE & downloads self-manage
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    maxHeaderBytes,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: config.DefaultConcurrentRequests,
			SendPingTimeout:      10 * time.Second,
			PingTimeout:          5 * time.Second,
			WriteByteTimeout:     30 * time.Second,
		},
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

func (s *Server) Serve(ln net.Listener) error {
	return s.httpSrv.Serve(limitConnections(ln, s.cfg.Server.MaxConnections))
}

// Expiry ages have already been defaulted and validated by config.Prepare.
func (s *Server) artifactTTL() time.Duration { return s.cfg.Scratchpad.ArtifactTTL.D() }
func (s *Server) workTTL() time.Duration     { return s.cfg.Scratchpad.WorkTTL.D() }

// Handler exposes the assembled handler chain (used by tests).
func (s *Server) Handler() http.Handler { return s.httpSrv.Handler }

func (s *Server) ServeTLS(ln net.Listener, certFile, keyFile string) error {
	return s.httpSrv.ServeTLS(limitConnections(ln, s.cfg.Server.MaxConnections), certFile, keyFile)
}

func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpSrv.Shutdown(ctx)
	if closeErr := s.tools.Close(); closeErr != nil {
		s.logger.Warn("tool transport shutdown failed", "error", closeErr)
	}
	if s.mcpOAuth != nil {
		err = errors.Join(err, s.mcpOAuth.Close())
	}
	return err
}

// AbortGenerations closes admission and cancels all live generations (SIGTERM path in main):
// SSE handlers observe ctx.Done, persist partial answers like an abort and
// return, which lets Shutdown finish inside the grace period instead of
// timing out on still-open streams.
func (s *Server) AbortGenerations() int { return s.chat.Generations.Shutdown() }

// WaitIdle blocks until in-flight generations have finished (or ctx is
// done). Called after Shutdown so streams can persist partial answers
// before the database closes.
func (s *Server) WaitIdle(ctx context.Context) { s.chat.Generations.WaitIdle(ctx) }

// --- handlers ---------------------------------------------------------------

// handleHealth is information-free on purpose (spec §5.4).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleAuthConfig is public and exposes only what the login/register screens need.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"registration":   s.cfg.Auth.Registration,
		"remember_login": s.cfg.Auth.RememberLogin,
		"email_features": s.cfg.Email.Enabled,
	})
}

// handleClientConfig returns configuration used only by the authenticated
// application. Keeping it behind auth avoids advertising the deployment's
// model catalogue, thinking controls and file policy to anonymous scanners.
func (s *Server) handleClientConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"models": s.cfg.LLM.ModelNames(),
		// default_model lets the picker name the model a conversation with
		// no explicit choice actually runs on.
		"default_model":  s.cfg.LLM.DefaultModelID(),
		"show_reasoning": s.cfg.LLM.ShowReasoning,
		"tool_cards":     s.cfg.Tools.Agent.ToolCards,
		"mcp_oauth":      s.cfg.MCP.EnableOAuth,
		// Thinking selector: the choice list per picker model (a model's
		// dropdown describes its model), or null when the feature is off —
		// the SPA hides the selector on null and per-model rows on empty.
		// Only id+label cross the wire: request
		// templates and credentials stay server-side.
		"thinking": s.thinkingMeta(),
		// (The context window does not ride client config: it is a model
		// property, so each conversation carries its own — §6.1.)
		"files": map[string]any{
			"max_file_size":      int64(s.cfg.Scratchpad.MaxFileSize),
			"max_attachments":    s.cfg.AppLimits.MaxAttachments,
			"blocked_extensions": s.cfg.Scratchpad.BlockExtensions,
		},
	})
}

// staticAssetsHandler serves embedded files under /assets/. A trailing-slash
// directory request never yields a listing (spec §9.2). The app modules are
// served from a startup-rewritten copy so their imports use commit stamps;
// everything else rides the plain embedded server.
func (s *Server) staticAssetsHandler() http.HandlerFunc {
	files := http.FileServerFS(web.FS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, module := s.assets.modules[r.URL.Path]
		application := module || r.URL.Path == "/assets/styles.css"
		stamps := r.URL.Query()["v"]
		if application && len(stamps) > 0 && (len(stamps) != 1 || stamps[0] != s.assets.stamp) {
			// Never serve new bytes at an old commit URL. A page left open
			// across a deployment must reload rather than mix module graphs.
			s.handleNotFound(w, r)
			return
		}
		immutable := (application && s.assets.stamp != "" && len(stamps) == 1) ||
			strings.HasPrefix(r.URL.Path, "/assets/vendor/katex-0.19.1/")
		w = &assetCacheWriter{ResponseWriter: w,
			manifest: strings.HasSuffix(r.URL.Path, ".webmanifest"), immutable: immutable}
		if strings.HasSuffix(r.URL.Path, "/") {
			s.handleNotFound(w, r)
			return
		}
		if body, ok := s.assets.modules[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = w.Write(body)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".webmanifest") {
			// Go's mime table predates the extension; without this the
			// manifest is sniffed as text/plain and browsers may ignore it.
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.indexHTML != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(s.indexHTML)
		return
	}
	http.ServeFileFS(w, r, web.FS, "index.html")
}

// routingErrors keeps Go's method matching and Allow generation, replacing
// only its default 404/405 text bodies with the API error format.
func routingErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "*" {
			mux.ServeHTTP(w, r)
			return
		}
		h, pattern := mux.Handler(r)
		if pattern == "" {
			h.ServeHTTP(&routeErrorWriter{ResponseWriter: w, request: r}, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type routeErrorWriter struct {
	http.ResponseWriter
	request  *http.Request
	replaced bool
}

func (w *routeErrorWriter) WriteHeader(status int) {
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.replaced = true
	code, message := "not_found", "resource not found"
	if status == http.StatusMethodNotAllowed {
		code, message = "method_not_allowed", "method not allowed"
	}
	writeAPIError(w.ResponseWriter, w.request, status, code, message)
}
func (w *routeErrorWriter) Write(p []byte) (int, error) {
	if w.replaced {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, http.StatusNotFound, "not_found", "resource not found")
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
