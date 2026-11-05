package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"mullsmith/internal/config"
)

func TestMCPAuthSurvivesSameOriginRedirect(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "ok"},
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
		})
	mcpHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true})
	var baseURL string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, baseURL+"/mcp", http.StatusTemporaryRedirect)
			return
		}
		if r.Header.Get("Authorization") != "Bearer same-origin-secret" || r.Header.Get("X-API-Key") != "same-origin-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	baseURL = httpServer.URL

	ctx, cancel := context.WithTimeout(context.Background(), mcpStartupTimeout)
	defer cancel()
	session, tools, closeSession, err := connectMCP(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), config.MCPServer{
		ID: "auth", Name: "Auth", Transport: "streamable_http", URL: baseURL + "/start", BearerToken: "same-origin-secret",
		Headers: map[string]string{"X-API-Key": "same-origin-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession()
	if session == nil || len(tools) != 1 {
		t.Fatalf("session/tools = %v/%d", session != nil, len(tools))
	}
}

func TestMCPAuthNeverCrossesOriginRedirect(t *testing.T) {
	var destinationHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationHits.Add(1)
		if auth, key := r.Header.Get("Authorization"), r.Header.Get("X-API-Key"); auth != "" || key != "" {
			t.Errorf("credentials crossed origin: authorization=%q api-key=%q", auth, key)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	ctx, cancel := context.WithTimeout(context.Background(), mcpStartupTimeout)
	defer cancel()
	_, _, _, err := connectMCP(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), config.MCPServer{
		ID: "auth", Name: "Auth", Transport: "streamable_http", URL: source.URL, BearerToken: "must-not-cross",
		Headers: map[string]string{"X-API-Key": "also-must-not-cross"},
	})
	if err == nil {
		t.Fatal("cross-origin redirect unexpectedly connected")
	}
	if destinationHits.Load() != 0 {
		t.Fatalf("redirect destination received %d request(s)", destinationHits.Load())
	}
}

func TestSameMCPOriginIncludesScheme(t *testing.T) {
	httpsURL, err := url.Parse("https://example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	httpURL, err := url.Parse("http://example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if sameMCPOrigin(httpsURL, httpURL) {
		t.Fatal("HTTPS to HTTP was treated as a same-origin redirect")
	}
}

func TestMCPSecretsAreRedactedFromChecksAndLogs(t *testing.T) {
	const secret = "very-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected "+secret, http.StatusUnauthorized)
	}))
	defer server.Close()
	cfg := &config.Config{MCP: config.MCP{Servers: []config.MCPServer{{
		ID: "private", Name: "Private", Transport: "streamable_http", URL: server.URL, BearerToken: secret,
	}}}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	checks := CheckMCPServers(context.Background(), cfg, logger)
	if len(checks) != 1 || checks[0].OK {
		t.Fatalf("checks = %+v", checks)
	}
	logger = mcpSafeLogger(logger, cfg.MCP.Servers[0])
	logger.Error("failure "+secret, "error", errors.New("credential "+secret), "nested", slog.GroupValue(slog.String("value", secret)))
	combined := checks[0].Detail + logs.String()
	if strings.Contains(combined, secret) {
		t.Fatalf("secret leaked through MCP diagnostics: %s", combined)
	}
	if !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("redaction marker absent: %s", combined)
	}
}
