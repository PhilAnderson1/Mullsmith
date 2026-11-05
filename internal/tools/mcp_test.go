package tools

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"mullsmith/internal/config"
)

func TestMCPToolAdapterCallsSession(t *testing.T) {
	ctx := context.Background()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	type input struct {
		Question string `json:"question"`
	}
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "ask"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in input) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "answer: " + in.Question}}, IsError: in.Question == "fail"}, nil, nil
	})
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "mullsmith-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	tool := &mcpTool{name: "mcp_docs_ask", remoteName: "ask", serverName: "Docs",
		session: clientSession, parameters: map[string]any{"type": "object"}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result, err := tool.Execute(ctx, Invocation{Arguments: `{"question":"why"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "answer: why") || result.Summary != "Called Docs: ask" {
		t.Fatalf("unexpected result: %+v", result)
	}
	failed, err := tool.Execute(ctx, Invocation{Arguments: `{"question":"fail"}`})
	if err != nil || !failed.Failed || !strings.Contains(failed.Summary, "reported an error") {
		t.Fatalf("reported failure = %+v, %v", failed, err)
	}
}

func TestRegisterMCPDiscoversHTTPToolsAndPolicy(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "ask", Description: "Ask the docs"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
		})
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return server
	}, &mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
	defer httpServer.Close()

	cfg := &config.Config{MCP: config.MCP{Servers: []config.MCPServer{{
		ID: "docs", Name: "Documentation", Transport: "streamable_http", URL: httpServer.URL, Approval: ApprovalAuto,
	}}}}
	policies := map[string]string{}
	reg := NewRegistry(func(name string) string { return policies[name] })
	registerMCP(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), reg, policies)
	defer reg.Close()
	tool, ok := reg.Get("mcp_docs_ask")
	if !ok || reg.Policy(tool.Name()) != ApprovalAuto {
		t.Fatalf("registered MCP tool = %v, policy = %q", ok, reg.Policy("mcp_docs_ask"))
	}
	result, err := tool.Execute(context.Background(), Invocation{Arguments: `{}`})
	if err != nil || !strings.Contains(result.Content, `"text":"ok"`) {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
}

func TestMCPToolNamesAndSchemasAreBounded(t *testing.T) {
	if got := mcpToolName("docs", "Search Docs"); got != "mcp_docs_search_docs" {
		t.Fatalf("name = %q", got)
	}
	if got := mcpToolName("docs", "!!!"); got != "" {
		t.Fatalf("empty normalized name = %q", got)
	}
	deep := map[string]any{}
	here := deep
	for range mcpMaxSchemaDepth + 1 {
		next := map[string]any{}
		here["x"] = next
		here = next
	}
	if _, err := mcpParameters(deep); err == nil {
		t.Fatal("deep schema accepted")
	}
}
