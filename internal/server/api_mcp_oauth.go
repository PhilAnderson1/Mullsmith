package server

import (
	"net/http"
	"net/url"
)

func (s *Server) handleMCPConnections(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.mcpOAuth == nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "MCP OAuth is disabled")
		return
	}
	items, err := s.mcpOAuth.Connections(r.Context(), UserFrom(r.Context()).ID)
	if err != nil {
		s.logger.Error("list MCP connections failed", "error", err)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleMCPConnect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.mcpOAuth == nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "MCP OAuth is disabled")
		return
	}
	authURL, err := s.mcpOAuth.Begin(r.Context(), UserFrom(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		s.logger.Warn("start MCP OAuth failed", "server", r.PathValue("id"))
		writeAPIError(w, r, http.StatusBadGateway, "oauth_failed", "could not start authorization")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorization_url": authURL})
}

func (s *Server) handleMCPDisconnect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.mcpOAuth == nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "MCP OAuth is disabled")
		return
	}
	if err := s.mcpOAuth.Disconnect(r.Context(), UserFrom(r.Context()).ID, r.PathValue("id")); err != nil {
		s.logger.Error("disconnect MCP OAuth failed", "error", err)
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.mcpOAuth == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" || !s.mcpOAuth.Complete(state, q.Get("code"), q.Get("iss"), q.Get("error")) {
		writeAPIError(w, r, http.StatusBadRequest, "oauth_state", "authorization request is invalid or expired")
		return
	}
	result := "returned"
	if q.Get("error") != "" {
		result = "denied"
	}
	target := s.cfg.Origin() + "/#/settings/connections?oauth=" + url.QueryEscape(result)
	http.Redirect(w, r, target, http.StatusSeeOther)
}
