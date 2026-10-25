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
	"time"

	"gabkeep/internal/agent"
	"gabkeep/internal/auth"
	"gabkeep/internal/config"
	"gabkeep/internal/llm"
	"gabkeep/internal/store"
	"gabkeep/internal/tools"
	"gabkeep/web"
)

// Server is the assembled gabkeep HTTP application.
type Server struct {
	cfg       *config.Config
	store     *store.DB
	logger    *slog.Logger
	appVer    string
	limiter   *auth.Limiter
	locks     *auth.LoginLocks
	gens      *genRegistry
	generator *agent.Generator
	tools     *tools.Registry
	httpSrv   *http.Server
}

// New builds the full handler chain. Middleware order (outermost first):
// requestID → logRequests → recoverPanics → securityHeaders → csrfProtect →
// bodyLimit → mux.
func New(cfg *config.Config, db *store.DB, logger *slog.Logger, appVer string) *Server {
	s := &Server{
		cfg:       cfg,
		store:     db,
		logger:    logger,
		appVer:    appVer,
		limiter:   auth.NewLimiter(),
		locks:     auth.NewLoginLocks(),
		gens:      newGenRegistry(),
		generator: &agent.Generator{Cfg: cfg, LLM: llm.NewClient(cfg, nil)},
		tools:     tools.BuildFor(cfg, logger, nil),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/meta", s.handleMeta)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.Handle("POST /api/v1/auth/logout", s.authRequired(s.handleLogout))
	mux.Handle("GET /api/v1/auth/me", s.authRequired(s.handleMe))
	mux.Handle("POST /api/v1/auth/password", s.authRequired(s.handleChangePassword))
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
	mux.Handle("GET /api/v1/files/{id}/download", s.authRequired(s.handleFileDownload))
	mux.Handle("DELETE /api/v1/files/{id}", s.authRequired(s.handleFileDelete))
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

// Handler exposes the assembled handler chain (used by tests).
func (s *Server) Handler() http.Handler { return s.httpSrv.Handler }

func (s *Server) ServeTLS(ln net.Listener, certFile, keyFile string) error {
	return s.httpSrv.ServeTLS(ln, certFile, keyFile)
}

func (s *Server) Shutdown(ctx context.Context) error { return s.httpSrv.Shutdown(ctx) }

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
		"app":            "gabkeep",
		"version":        s.appVer,
		"models":         s.cfg.LLM.Models,
		"registration":   s.cfg.Auth.Registration,
		"remember_login": s.cfg.Auth.RememberLogin,
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
// directory request never yields a listing (spec §9.2).
func (s *Server) staticAssetsHandler() http.HandlerFunc {
	files := http.FileServerFS(web.FS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			s.handleNotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// sinkHandler dispatches everything the specific patterns did not match.
func (s *Server) sinkHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
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
