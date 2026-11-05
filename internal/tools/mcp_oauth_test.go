package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"mullsmith/internal/config"
	"mullsmith/internal/store"
)

func TestMCPOAuthManagerConnectsAndIsolatesUsers(t *testing.T) {
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "oauth-test", Version: "1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "whoami", Description: "return identity"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "alice"}}}, nil, nil
		})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{JSONResponse: true})

	var root string
	var seenMu sync.Mutex
	var seen []string
	record := func(s string) { seenMu.Lock(); seen = append(seen, s); seenMu.Unlock() }
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		record(r.Method + " /mcp " + r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer access-token" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+root+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		record(r.Method + " /prm")
		mcpauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource: root + "/mcp", AuthorizationServers: []string{root},
		}).ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		record("GET /metadata")
		writeTestJSON(w, map[string]any{"issuer": root, "authorization_endpoint": root + "/authorize",
			"token_endpoint": root + "/token", "registration_endpoint": root + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		record("POST /register")
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		writeTestJSON(w, map[string]any{"client_id": "dynamic-client", "token_endpoint_auth_method": "none",
			"redirect_uris": []string{"http://mullsmith.test:3210/api/v1/mcp/oauth/callback"},
			"grant_types":   []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		record("GET /authorize")
		q := r.URL.Query()
		if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("scope") != "calendar.read" {
			http.Error(w, "missing configured authorization parameters or scope", http.StatusBadRequest)
			return
		}
		redirect, _ := url.Parse(q.Get("redirect_uri"))
		values := redirect.Query()
		values.Set("code", "valid-code")
		values.Set("state", q.Get("state"))
		values.Set("iss", root)
		redirect.RawQuery = values.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		record("POST /token")
		_ = r.ParseForm()
		id, secret, basic := r.BasicAuth()
		if !basic {
			id, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
		}
		validStatic := id == "client" && secret == "secret"
		validDynamic := r.Form.Get("client_id") == "dynamic-client" && secret == ""
		if (!validStatic && !validDynamic) || r.Form.Get("code") != "valid-code" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		writeTestJSON(w, map[string]any{"access_token": "access-token", "refresh_token": "refresh-token",
			"token_type": "Bearer", "expires_in": 3600})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	root = ts.URL

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	if err := db.CreateUser(context.Background(), &store.User{ID: "user-a", Email: "a@example.test", Name: "A", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(context.Background(), &store.User{ID: "user-b", Email: "b@example.test", Name: "B", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Server: config.Server{Listen: "127.0.0.1:3210", Host: "mullsmith.test:3210"}, MCP: config.MCP{
		EnableOAuth: true, TokenEncryptionKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		Servers: []config.MCPServer{{ID: "private", Name: "Private", Transport: "streamable_http", URL: ts.URL + "/mcp",
			OAuth: &config.MCPOAuth{ClientID: "client", ClientSecret: "secret", Issuer: ts.URL,
				Scopes: []string{"calendar.read"}, AuthorizationParams: map[string]string{"access_type": "offline", "prompt": "consent"}}, Approval: "auto"},
			{ID: "dynamic", Name: "Dynamic", Transport: "streamable_http", URL: ts.URL + "/mcp",
				OAuth: &config.MCPOAuth{DynamicClientRegistration: true, Issuer: ts.URL,
					Scopes: []string{"calendar.read"}, AuthorizationParams: map[string]string{"access_type": "offline", "prompt": "consent"}}, Approval: "auto"}},
	}}
	var logs bytes.Buffer
	manager, err := NewMCPOAuthManager(cfg, db, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	manager.client = ts.Client()

	authURL, err := manager.Begin(context.Background(), "user-a", "private")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	location, err := resp.Location()
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Complete(location.Query().Get("state"), location.Query().Get("code"), location.Query().Get("iss"), "") {
		t.Fatal("callback state was not accepted")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(manager.Specs("user-a")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(manager.Specs("user-a")) != 1 {
		seenMu.Lock()
		defer seenMu.Unlock()
		stored, storedErr := db.MCPOAuthConnection(context.Background(), "user-a", "private")
		t.Fatalf("user-a specs = %v; requests: %v; stored=%v storedErr=%v; logs: %s", manager.Specs("user-a"), seen, stored != nil, storedErr, logs.String())
	}
	if len(manager.Specs("user-b")) != 0 {
		t.Fatalf("user-b saw user-a tools: %v", manager.Specs("user-b"))
	}
	if manager.Policy("user-a", "mcp_private_whoami") != ApprovalAuto || manager.Policy("user-b", "mcp_private_whoami") != ApprovalConfirm {
		t.Fatal("per-user OAuth approval policy was not isolated")
	}
	tool, ok := manager.Get("user-a", "mcp_private_whoami")
	if !ok {
		t.Fatal("connected tool missing")
	}
	result, err := tool.Execute(context.Background(), Invocation{UserID: "user-a", Arguments: `{}`})
	if err != nil || !strings.Contains(result.Content, "alice") {
		t.Fatalf("tool result=%+v err=%v", result, err)
	}

	row, err := db.MCPOAuthConnection(context.Background(), "user-a", "private")
	if err != nil || row == nil {
		t.Fatalf("stored connection=%v err=%v", row, err)
	}
	if strings.Contains(string(row.TokenCipher), "access-token") || strings.Contains(string(row.TokenCipher), "refresh-token") {
		t.Fatal("plaintext token reached SQLite")
	}

	dynamicURL, err := manager.Begin(context.Background(), "user-b", "dynamic")
	if err != nil {
		t.Fatal(err)
	}
	dynamicResp, err := client.Get(dynamicURL)
	if err != nil {
		t.Fatal(err)
	}
	dynamicResp.Body.Close()
	dynamicLocation, err := dynamicResp.Location()
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Complete(dynamicLocation.Query().Get("state"), dynamicLocation.Query().Get("code"), dynamicLocation.Query().Get("iss"), "") {
		t.Fatal("dynamic callback state was not accepted")
	}
	deadline = time.Now().Add(3 * time.Second)
	for len(manager.Specs("user-b")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(manager.Specs("user-b")) != 1 {
		seenMu.Lock()
		defer seenMu.Unlock()
		t.Fatalf("dynamic specs = %v; requests: %v; logs: %s", manager.Specs("user-b"), seen, logs.String())
	}
	dynamicRow, err := db.MCPOAuthConnection(context.Background(), "user-b", "dynamic")
	if err != nil || dynamicRow == nil {
		t.Fatalf("stored dynamic connection=%v err=%v", dynamicRow, err)
	}
	dynamicStored, err := manager.decrypt(dynamicRow)
	if err != nil || dynamicStored.ClientID != "dynamic-client" {
		t.Fatalf("stored dynamic client id=%q err=%v", dynamicStored.ClientID, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := NewMCPOAuthManager(cfg, db, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	restored.client = ts.Client()
	defer restored.Close()
	restored.Restore(context.Background())
	if len(restored.Specs("user-a")) != 1 {
		t.Fatalf("restored specs = %v; logs: %s", restored.Specs("user-a"), logs.String())
	}
	if len(restored.Specs("user-b")) != 1 {
		t.Fatalf("restored dynamic specs = %v; logs: %s", restored.Specs("user-b"), logs.String())
	}
	if err := restored.Disconnect(context.Background(), "user-b", "private"); err != nil {
		t.Fatal(err)
	}
	if len(restored.Specs("user-a")) != 1 {
		t.Fatal("another user disconnected user-a's connection")
	}
	if err := restored.Disconnect(context.Background(), "user-a", "private"); err != nil {
		t.Fatal(err)
	}
	if len(restored.Specs("user-a")) != 0 {
		t.Fatal("disconnected tools remain registered")
	}
	row, err = db.MCPOAuthConnection(context.Background(), "user-a", "private")
	if err != nil || row != nil {
		t.Fatalf("disconnected stored connection = %v, %v", row, err)
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

type oauthTestCaller struct{}

func (oauthTestCaller) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{}, nil
}

type oauthFailCaller struct{}

func (oauthFailCaller) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return nil, fmt.Errorf("authorization failed: %w", ErrMCPAuthorizationRequired)
}

func TestMCPOAuthManagerRejectsToolNameCollisions(t *testing.T) {
	cfg := &config.Config{MCP: config.MCP{EnableOAuth: true,
		TokenEncryptionKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}}
	manager, err := NewMCPOAuthManager(cfg, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.SetReservedToolNames([]string{"mcp_reserved_name"})
	remote := func(name string) []*mcp.Tool {
		return []*mcp.Tool{{Name: name, InputSchema: map[string]any{"type": "object"}}}
	}
	if err := manager.install("user-a", config.MCPServer{ID: "reserved", Name: "Reserved"}, remote("name"), func() error { return nil }, oauthTestCaller{}, nil); err == nil {
		t.Fatal("reserved built-in tool name was accepted")
	}
	if err := manager.install("user-a", config.MCPServer{ID: "foo", Name: "Foo", Approval: ApprovalAuto}, remote("bar_x"), func() error { return nil }, oauthTestCaller{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.install("user-a", config.MCPServer{ID: "foo_bar", Name: "Foo bar"}, remote("x"), func() error { return nil }, oauthTestCaller{}, nil); err == nil {
		t.Fatal("colliding OAuth tools for one user were accepted")
	}
	if err := manager.install("user-b", config.MCPServer{ID: "foo_bar", Name: "Foo bar"}, remote("x"), func() error { return nil }, oauthTestCaller{}, nil); err != nil {
		t.Fatalf("another user's independent tool namespace was rejected: %v", err)
	}
	if manager.Policy("user-a", "mcp_foo_bar_x") != ApprovalAuto || manager.Policy("user-b", "mcp_foo_bar_x") != ApprovalConfirm {
		t.Fatal("approval policy was inferred from an ambiguous tool-name prefix")
	}
	if err := manager.install("user-a", config.MCPServer{ID: "failed", Name: "Failed"}, remote("call"), func() error { return nil }, oauthFailCaller{}, nil); err != nil {
		t.Fatal(err)
	}
	failed, ok := manager.Get("user-a", "mcp_failed_call")
	if !ok {
		t.Fatal("failing tool was not installed")
	}
	if _, err := failed.Execute(context.Background(), Invocation{Arguments: `{}`}); err == nil {
		t.Fatal("failing transport returned success")
	}
	if _, ok := manager.Get("user-a", "mcp_failed_call"); ok {
		t.Fatal("failed OAuth connection remained live")
	}
}

func TestMCPOAuthResponseBodyIsBounded(t *testing.T) {
	transport := &mcpOAuthResponseLimitTransport{base: egressRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1,
			Body: io.NopCloser(strings.NewReader(strings.Repeat("x", mcpOAuthMaxResponseBytes+1)))}, nil
	})}
	resp, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "https://example.test", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("oversized OAuth response was accepted")
	}
}
