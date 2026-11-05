package tools

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/oauth2"

	"mullsmith/internal/config"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
)

var ErrMCPAuthorizationRequired = errors.New("MCP authorization required")

const (
	mcpOAuthMaxResponseBytes = 1 << 20
	mcpOAuthMaxAuthURLBytes  = 16 << 10
)

type mcpOAuthResponseLimitTransport struct{ base http.RoundTripper }

func (t *mcpOAuthResponseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > mcpOAuthMaxResponseBytes {
		_ = resp.Body.Close()
		return nil, errors.New("OAuth response is too large")
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, mcpOAuthMaxResponseBytes)
	return resp, nil
}

func (t *mcpOAuthResponseLimitTransport) CloseIdleConnections() {
	if idle, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		idle.CloseIdleConnections()
	}
}

type MCPConnectionStatus struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type oauthStoredToken struct {
	Token        *oauth2.Token `json:"token"`
	ClientID     string        `json:"client_id,omitempty"`
	ClientSecret string        `json:"client_secret,omitempty"`
	Issuer       string        `json:"issuer"`
	AuthURL      string        `json:"auth_url"`
	TokenURL     string        `json:"token_url"`
	Scopes       []string      `json:"scopes,omitempty"`
	RedirectURL  string        `json:"redirect_url"`
}

type oauthUserConnection struct {
	tools  map[string]Tool
	policy string
	close  func() error
	marker *byte
}

type oauthPending struct {
	userID, serverID string
	result           chan oauthCallback
}

type oauthFlow struct{ cancel context.CancelFunc }

type oauthCallback struct {
	code, state, issuer string
	err                 error
}

type MCPOAuthManager struct {
	cfg    *config.Config
	db     *store.DB
	logger *slog.Logger
	aead   cipher.AEAD
	client *http.Client

	mu       sync.RWMutex
	conns    map[string]*oauthUserConnection
	reserved map[string]bool
	pending  map[string]*oauthPending
	flows    map[string]*oauthFlow
	closed   bool
	wg       sync.WaitGroup
	lifetime context.Context
	stop     context.CancelFunc
}

func NewMCPOAuthManager(cfg *config.Config, db *store.DB, logger *slog.Logger) (*MCPOAuthManager, error) {
	if !cfg.MCP.EnableOAuth {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(cfg.MCP.TokenEncryptionKey)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	lifetime, stop := context.WithCancel(context.Background())
	m := &MCPOAuthManager{cfg: cfg, db: db, logger: logger, aead: aead, client: newMCPOAuthHTTPClient(), lifetime: lifetime, stop: stop,
		conns: map[string]*oauthUserConnection{}, reserved: map[string]bool{}, pending: map[string]*oauthPending{}, flows: map[string]*oauthFlow{}}
	return m, nil
}

func newMCPOAuthHTTPClient() *http.Client {
	client := NewGuardedClient(mcpResponseHeaderTimeout)
	client.Transport = &mcpOAuthResponseLimitTransport{base: client.Transport}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || len(via) == 0 || !sameMCPOrigin(req.URL, via[0].URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return client
}

func oauthConnKey(userID, serverID string) string { return userID + "\x00" + serverID }

func (m *MCPOAuthManager) oauthServer(id string) (config.MCPServer, bool) {
	for _, server := range m.cfg.MCP.Servers {
		if server.ID == id && server.OAuth != nil {
			return server, true
		}
	}
	return config.MCPServer{}, false
}

func (m *MCPOAuthManager) encrypt(userID, serverID string, value *oauthStoredToken) ([]byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, m.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return m.aead.Seal(nonce, nonce, plain, []byte(oauthConnKey(userID, serverID))), nil
}

func (m *MCPOAuthManager) decrypt(c *store.MCPOAuthConnection) (*oauthStoredToken, error) {
	n := m.aead.NonceSize()
	if len(c.TokenCipher) < n {
		return nil, errors.New("invalid OAuth token ciphertext")
	}
	plain, err := m.aead.Open(nil, c.TokenCipher[:n], c.TokenCipher[n:], []byte(oauthConnKey(c.UserID, c.ServerID)))
	if err != nil {
		return nil, errors.New("decrypt OAuth token")
	}
	var out oauthStoredToken
	if err := json.Unmarshal(plain, &out); err != nil || out.Token == nil {
		return nil, errors.New("decode OAuth token")
	}
	return &out, nil
}

func (m *MCPOAuthManager) save(ctx context.Context, userID, serverID string, value *oauthStoredToken) error {
	ciphertext, err := m.encrypt(userID, serverID, value)
	if err != nil {
		return err
	}
	return m.db.PutMCPOAuthConnection(ctx, &store.MCPOAuthConnection{UserID: userID, ServerID: serverID, TokenCipher: ciphertext})
}

type savingTokenSource struct {
	mu       sync.Mutex
	base     oauth2.TokenSource
	last     *oauth2.Token
	metadata oauthStoredToken
	save     func(*oauthStoredToken) error
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if s.last == nil || tok.AccessToken != s.last.AccessToken || tok.RefreshToken != s.last.RefreshToken || !tok.Expiry.Equal(s.last.Expiry) {
		s.metadata.Token = tok
		if err := s.save(&s.metadata); err != nil {
			return nil, fmt.Errorf("persist refreshed OAuth token: %w", err)
		}
		s.last = tok
	}
	return tok, nil
}

func (m *MCPOAuthManager) handler(userID string, server config.MCPServer, initial *oauthStoredToken,
	fetch mcpauth.AuthorizationCodeFetcher,
) (*mcpauth.AuthorizationCodeHandler, error) {
	cfg := &mcpauth.AuthorizationCodeHandlerConfig{
		RedirectURL: m.cfg.MCPOAuthCallbackURL(), AuthorizationCodeFetcher: fetch,
		RequestRefreshToken: true, Client: m.client,
	}
	clientID, clientSecret := server.OAuth.ClientID, server.OAuth.ClientSecret
	if initial != nil && initial.ClientID != "" {
		clientID, clientSecret = initial.ClientID, initial.ClientSecret
	}
	if clientID != "" {
		credentials := &oauthex.ClientCredentials{ClientID: clientID, Issuer: server.OAuth.Issuer}
		if clientSecret != "" {
			credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: clientSecret}
		}
		cfg.PreregisteredClient = credentials
	} else if server.OAuth.DynamicClientRegistration {
		cfg.DynamicClientRegistrationConfig = &mcpauth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{m.cfg.MCPOAuthCallbackURL()}, TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			ClientName: "Mullsmith", ClientURI: m.cfg.Origin(), Scope: strings.Join(server.OAuth.Scopes, " "),
		}}
	}
	if initial != nil {
		oauthCfg := &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret,
			Endpoint:    oauth2.Endpoint{AuthURL: initial.AuthURL, TokenURL: initial.TokenURL},
			RedirectURL: initial.RedirectURL, Scopes: initial.Scopes}
		refreshCtx := context.WithValue(context.Background(), oauth2.HTTPClient, m.client)
		cfg.InitialTokenSource = &savingTokenSource{base: oauthCfg.TokenSource(refreshCtx, initial.Token),
			last: initial.Token, metadata: *initial, save: func(v *oauthStoredToken) error {
				return m.save(context.Background(), userID, server.ID, v)
			}}
	}
	cfg.NewTokenSource = func(ctx context.Context, oauthCfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
		stored := oauthStoredToken{Token: token, ClientID: oauthCfg.ClientID, ClientSecret: oauthCfg.ClientSecret,
			Issuer: server.OAuth.Issuer, AuthURL: oauthCfg.Endpoint.AuthURL, TokenURL: oauthCfg.Endpoint.TokenURL,
			Scopes: append([]string(nil), oauthCfg.Scopes...), RedirectURL: oauthCfg.RedirectURL}
		if err := m.save(context.Background(), userID, server.ID, &stored); err != nil {
			return nil, err
		}
		return &savingTokenSource{base: oauthCfg.TokenSource(ctx, token), last: token, metadata: stored,
			save: func(v *oauthStoredToken) error { return m.save(context.Background(), userID, server.ID, v) }}, nil
	}
	return mcpauth.NewAuthorizationCodeHandler(cfg)
}

func (m *MCPOAuthManager) install(userID string, server config.MCPServer, remoteTools []*mcp.Tool, closeFn func() error, caller mcpToolCaller, expectedFlow *oauthFlow) error {
	key := oauthConnKey(userID, server.ID)
	marker := new(byte)
	adapted := make(map[string]Tool, len(remoteTools))
	for _, remote := range remoteTools {
		name := mcpToolName(server.ID, remote.Name)
		params, err := mcpParameters(remote.InputSchema)
		if name == "" || len(name) > 64 || err != nil || adapted[name] != nil {
			return errors.New("MCP server advertised invalid tools")
		}
		desc := strings.TrimSpace(remote.Description)
		if desc == "" {
			desc = "Tool supplied by the configured MCP server."
		}
		adapted[name] = &mcpTool{name: name, remoteName: remote.Name, serverName: server.Name,
			description: fmt.Sprintf("[%s MCP server] %s", server.Name, Elide(desc, 4096)),
			parameters:  params, caller: caller, logger: m.logger,
			secrets: mcpSecretValues(server), hideErrors: true,
			onError: func() { m.invalidate(key, marker) }}
	}
	m.mu.Lock()
	if m.closed || (expectedFlow != nil && m.flows[key] != expectedFlow) || (expectedFlow == nil && m.flows[key] != nil) {
		m.mu.Unlock()
		return context.Canceled
	}
	for name := range adapted {
		if m.reserved[name] {
			m.mu.Unlock()
			return errors.New("MCP tool name collides with a registered tool")
		}
		for otherKey, conn := range m.conns {
			if otherKey != key && strings.HasPrefix(otherKey, userID+"\x00") && conn.tools[name] != nil {
				m.mu.Unlock()
				return errors.New("MCP tool names collide between connected servers")
			}
		}
	}
	old := m.conns[key]
	m.conns[key] = &oauthUserConnection{tools: adapted, policy: server.Approval, close: closeFn, marker: marker}
	m.mu.Unlock()
	if old != nil && old.close != nil {
		_ = old.close()
	}
	return nil
}

func (m *MCPOAuthManager) invalidate(key string, marker *byte) {
	m.mu.Lock()
	conn := m.conns[key]
	if conn == nil || conn.marker != marker {
		m.mu.Unlock()
		return
	}
	delete(m.conns, key)
	m.mu.Unlock()
	if conn.close != nil {
		_ = conn.close()
	}
}

func (m *MCPOAuthManager) SetReservedToolNames(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range names {
		m.reserved[name] = true
	}
}

func (m *MCPOAuthManager) connect(ctx context.Context, userID string, server config.MCPServer,
	initial *oauthStoredToken, fetch mcpauth.AuthorizationCodeFetcher, expectedFlow *oauthFlow,
) error {
	h, err := m.handler(userID, server, initial, fetch)
	if err != nil {
		return err
	}
	session, advertised, closeFn, err := connectMCPWithOAuth(ctx, m.logger, server, h)
	if err != nil {
		return err
	}
	if err := m.install(userID, server, advertised, closeFn, session, expectedFlow); err != nil {
		_ = closeFn()
		return err
	}
	return nil
}

func (m *MCPOAuthManager) connectInteractive(ctx context.Context, userID string, server config.MCPServer,
	fetch mcpauth.AuthorizationCodeFetcher, expectedFlow *oauthFlow,
) error {
	if server.OAuth.DynamicClientRegistration {
		if err := m.verifyDynamicRegistrationIssuer(ctx, server); err != nil {
			return err
		}
	}
	h, err := m.handler(userID, server, nil, fetch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
	if err != nil {
		return err
	}
	challenge := `Bearer resource_metadata="` + oauthResourceMetadataURL(server.URL) + `"`
	if len(server.OAuth.Scopes) > 0 {
		challenge += `, scope="` + strings.Join(server.OAuth.Scopes, " ") + `"`
	}
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: http.NoBody, Request: req}
	resp.Header.Set("WWW-Authenticate", challenge)
	if err := h.Authorize(ctx, req, resp); err != nil {
		return err
	}
	session, advertised, closeFn, err := connectMCPWithOAuth(ctx, m.logger, server, h)
	if err != nil {
		return err
	}
	if err := m.install(userID, server, advertised, closeFn, session, expectedFlow); err != nil {
		_ = closeFn()
		return err
	}
	return nil
}

func (m *MCPOAuthManager) verifyDynamicRegistrationIssuer(ctx context.Context, server config.MCPServer) error {
	resource, err := url.Parse(server.URL)
	if err != nil {
		return err
	}
	root := *resource
	root.Path, root.RawPath, root.RawQuery, root.Fragment = "", "", "", ""
	metadataURLs := []struct{ metadata, resource string }{
		{oauthResourceMetadataURL(server.URL), server.URL},
		{root.String() + "/.well-known/oauth-protected-resource", root.String()},
	}
	for _, candidate := range metadataURLs {
		prm, getErr := oauthex.GetProtectedResourceMetadata(ctx, candidate.metadata, candidate.resource, m.client)
		if getErr != nil || prm == nil {
			continue
		}
		for _, issuer := range prm.AuthorizationServers {
			if sameOAuthIssuer(issuer, server.OAuth.Issuer) {
				return nil
			}
		}
		return errors.New("MCP server OAuth issuer does not match the configured issuer")
	}
	return errors.New("MCP server did not publish protected-resource metadata for dynamic client registration")
}

func sameOAuthIssuer(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

func oauthResourceMetadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return resource
	}
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	u.Path = "/.well-known/oauth-protected-resource" + u.Path
	return u.String()
}

func (m *MCPOAuthManager) Restore(ctx context.Context) {
	rows, err := m.db.MCPOAuthConnections(ctx)
	if err != nil {
		m.logger.Error("load MCP OAuth connections failed", "error", err)
		return
	}
	jobs := make(chan store.MCPOAuthConnection)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for row := range jobs {
				m.restoreConnection(ctx, row)
			}
		}()
	}
	for _, row := range rows {
		select {
		case jobs <- row:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

func (m *MCPOAuthManager) restoreConnection(ctx context.Context, row store.MCPOAuthConnection) {
	server, ok := m.oauthServer(row.ServerID)
	if !ok {
		return
	}
	stored, err := m.decrypt(&row)
	if err == nil && stored.Issuer != server.OAuth.Issuer {
		err = errors.New("configured OAuth issuer changed")
	}
	if err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, mcpStartupTimeout)
		err = m.connect(probeCtx, row.UserID, server, stored, func(context.Context, *mcpauth.AuthorizationArgs) (*mcpauth.AuthorizationResult, error) {
			return nil, ErrMCPAuthorizationRequired
		}, nil)
		cancel()
	}
	if err != nil {
		m.logger.Warn("MCP OAuth connection needs reconnect", "server", row.ServerID)
	}
}

func (m *MCPOAuthManager) StartRestore() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		m.Restore(m.lifetime)
	}()
}

func (m *MCPOAuthManager) Begin(ctx context.Context, userID, serverID string) (string, error) {
	server, ok := m.oauthServer(serverID)
	if !ok {
		return "", errors.New("unknown OAuth MCP server")
	}
	flowCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	connectionKey := oauthConnKey(userID, serverID)
	flow := &oauthFlow{cancel: cancel}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return "", errors.New("MCP OAuth manager is closed")
	}
	if previous := m.flows[connectionKey]; previous != nil {
		previous.cancel()
	}
	for state, pending := range m.pending {
		if pending.userID == userID && pending.serverID == serverID {
			delete(m.pending, state)
		}
	}
	m.flows[connectionKey] = flow
	m.wg.Add(1)
	m.mu.Unlock()
	urlReady := make(chan string, 1)
	done := make(chan error, 1)
	var pendingState string
	var pendingMu sync.Mutex
	fetch := func(fetchCtx context.Context, args *mcpauth.AuthorizationArgs) (*mcpauth.AuthorizationResult, error) {
		u, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		for name, value := range server.OAuth.AuthorizationParams {
			q.Set(name, value)
		}
		u.RawQuery = q.Encode()
		args.URL = u.String()
		state := u.Query().Get("state")
		if state == "" {
			return nil, errors.New("authorization URL has no state")
		}
		if len(args.URL) > mcpOAuthMaxAuthURLBytes {
			return nil, errors.New("authorization URL is too large")
		}
		if u.Hostname() == "" || u.User != nil || u.Fragment != "" ||
			(u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackOAuthHost(u.Hostname()))) {
			return nil, errors.New("authorization URL must use HTTPS")
		}
		p := &oauthPending{userID: userID, serverID: serverID, result: make(chan oauthCallback, 1)}
		pendingMu.Lock()
		pendingState = state
		pendingMu.Unlock()
		m.mu.Lock()
		m.pending[state] = p
		m.mu.Unlock()
		select {
		case urlReady <- args.URL:
		default:
		}
		select {
		case got := <-p.result:
			if got.err != nil {
				return nil, got.err
			}
			return &mcpauth.AuthorizationResult{Code: got.code, State: got.state, Iss: got.issuer}, nil
		case <-fetchCtx.Done():
			return nil, fetchCtx.Err()
		}
	}
	go func() {
		defer m.wg.Done()
		err := m.connectInteractive(flowCtx, userID, server, fetch, flow)
		if err != nil && flowCtx.Err() == nil {
			m.logger.Warn("MCP OAuth connection failed", "server", server.ID, "error", err)
		}
		pendingMu.Lock()
		state := pendingState
		pendingMu.Unlock()
		if state != "" {
			m.mu.Lock()
			if current := m.pending[state]; current != nil && current.userID == userID && current.serverID == serverID {
				delete(m.pending, state)
			}
			if m.flows[connectionKey] == flow {
				delete(m.flows, connectionKey)
			}
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			if m.flows[connectionKey] == flow {
				delete(m.flows, connectionKey)
			}
			m.mu.Unlock()
		}
		cancel()
		done <- err
	}()
	select {
	case authURL := <-urlReady:
		return authURL, nil
	case err := <-done:
		cancel()
		if err == nil {
			return "", errors.New("MCP server did not request OAuth authorization")
		}
		return "", err
	case <-ctx.Done():
		cancel()
		return "", ctx.Err()
	case <-time.After(mcpStartupTimeout):
		cancel()
		return "", errors.New("OAuth discovery timed out")
	}
}

func isLoopbackOAuthHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (m *MCPOAuthManager) Complete(state, code, issuer, providerError string) bool {
	m.mu.Lock()
	p := m.pending[state]
	delete(m.pending, state)
	m.mu.Unlock()
	if p == nil {
		return false
	}
	var err error
	if providerError != "" {
		err = errors.New("authorization was rejected")
	} else if code == "" {
		err = errors.New("authorization response contains no code")
	}
	p.result <- oauthCallback{code: code, state: state, issuer: issuer, err: err}
	return true
}

func (m *MCPOAuthManager) Disconnect(ctx context.Context, userID, serverID string) error {
	key := oauthConnKey(userID, serverID)
	m.mu.Lock()
	conn := m.conns[key]
	delete(m.conns, key)
	if flow := m.flows[key]; flow != nil {
		flow.cancel()
		delete(m.flows, key)
	}
	for state, pending := range m.pending {
		if pending.userID == userID && pending.serverID == serverID {
			delete(m.pending, state)
		}
	}
	m.mu.Unlock()
	if conn != nil && conn.close != nil {
		_ = conn.close()
	}
	return m.db.DeleteMCPOAuthConnection(ctx, userID, serverID)
}

func (m *MCPOAuthManager) CloseUser(userID string) {
	m.mu.Lock()
	var closing []*oauthUserConnection
	for key, conn := range m.conns {
		if strings.HasPrefix(key, userID+"\x00") {
			closing = append(closing, conn)
			delete(m.conns, key)
		}
	}
	for state, p := range m.pending {
		if p.userID == userID {
			delete(m.pending, state)
		}
	}
	for key, flow := range m.flows {
		if strings.HasPrefix(key, userID+"\x00") {
			flow.cancel()
			delete(m.flows, key)
		}
	}
	m.mu.Unlock()
	for _, conn := range closing {
		if conn.close != nil {
			_ = conn.close()
		}
	}
}

func (m *MCPOAuthManager) Connections(ctx context.Context, userID string) ([]MCPConnectionStatus, error) {
	var out []MCPConnectionStatus
	for _, server := range m.cfg.MCP.Servers {
		if server.OAuth == nil {
			continue
		}
		status := "not_connected"
		m.mu.RLock()
		live := m.conns[oauthConnKey(userID, server.ID)] != nil
		connecting := m.flows[oauthConnKey(userID, server.ID)] != nil
		m.mu.RUnlock()
		if live {
			status = "connected"
		} else if connecting {
			status = "connecting"
		} else if row, err := m.db.MCPOAuthConnection(ctx, userID, server.ID); err != nil {
			return nil, err
		} else if row != nil {
			status = "reconnect_required"
		}
		out = append(out, MCPConnectionStatus{ID: server.ID, Name: server.Name, Status: status})
	}
	return out, nil
}

func (m *MCPOAuthManager) Specs(userID string) []llm.ToolSpec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var names []string
	byName := map[string]Tool{}
	for key, conn := range m.conns {
		if !strings.HasPrefix(key, userID+"\x00") {
			continue
		}
		for name, tool := range conn.tools {
			names = append(names, name)
			byName[name] = tool
		}
	}
	sort.Strings(names)
	out := make([]llm.ToolSpec, 0, len(names))
	for _, name := range names {
		t := byName[name]
		out = append(out, llm.ToolSpec{Type: "function", Function: llm.ToolFunction{Name: t.Name(), Description: t.Description(), Parameters: t.Parameters()}})
	}
	return out
}

func (m *MCPOAuthManager) Get(userID, name string) (Tool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for key, conn := range m.conns {
		if strings.HasPrefix(key, userID+"\x00") {
			if t, ok := conn.tools[name]; ok {
				return t, true
			}
		}
	}
	return nil, false
}

func (m *MCPOAuthManager) Policy(userID, name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for key, conn := range m.conns {
		if strings.HasPrefix(key, userID+"\x00") && conn.tools[name] != nil {
			if conn.policy == ApprovalAuto {
				return ApprovalAuto
			}
			return ApprovalConfirm
		}
	}
	return ApprovalConfirm
}

func (m *MCPOAuthManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.stop()
	for _, flow := range m.flows {
		flow.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()

	m.mu.Lock()
	var closing []*oauthUserConnection
	for _, conn := range m.conns {
		closing = append(closing, conn)
	}
	m.conns = map[string]*oauthUserConnection{}
	m.pending = map[string]*oauthPending{}
	m.flows = map[string]*oauthFlow{}
	m.mu.Unlock()
	var errs []error
	for _, conn := range closing {
		if conn.close != nil {
			errs = append(errs, conn.close())
		}
	}
	if idle, ok := m.client.Transport.(interface{ CloseIdleConnections() }); ok {
		idle.CloseIdleConnections()
	}
	return errors.Join(errs...)
}
