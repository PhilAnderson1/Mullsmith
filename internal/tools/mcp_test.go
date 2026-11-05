package tools

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		caller: clientSession, parameters: map[string]any{"type": "object"}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

func TestRegisterMCPDiscoversStdioToolsWithConstrainedEnvironment(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULLSMITH_PARENT_SECRET", "must-not-leak")
	root := t.TempDir()
	server := config.MCPServer{
		ID: "local", Name: "Local", Transport: "stdio", Command: exe,
		Args: []string{"-test.run=^TestMCPStdioHelperProcess$"},
		Environment: map[string]string{
			"MULLSMITH_MCP_TEST_HELPER": "1",
			"MCP_TEST_VALUE":            "allowed",
		},
		Approval: ApprovalConfirm,
		StateDir: filepath.Join(root, "local"),
	}
	cfg := &config.Config{MCP: config.MCP{StateDir: root, Servers: []config.MCPServer{server}}}
	policies := map[string]string{}
	reg := NewRegistry(func(name string) string { return policies[name] })
	registerMCP(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), reg, policies)
	tool, ok := reg.Get("mcp_local_inspect")
	if !ok {
		reg.Close()
		t.Fatal("stdio MCP tool was not registered")
	}
	result, err := tool.Execute(context.Background(), Invocation{Arguments: `{}`})
	if err != nil {
		reg.Close()
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `allowed`) || strings.Contains(result.Content, `must-not-leak`) {
		t.Fatalf("constrained environment result = %s", result.Content)
	}
	if !strings.Contains(result.Content, server.StateDir) {
		t.Fatalf("HOME/cwd not rooted at state directory: %s", result.Content)
	}
	if _, err := os.Stat(filepath.Join(server.StateDir, "helper-state")); err != nil {
		t.Fatalf("helper did not write its state directory: %v", err)
	}
	slow, ok := reg.Get("mcp_local_slow")
	if !ok {
		reg.Close()
		t.Fatal("stdio MCP slow tool was not registered")
	}
	cancelled, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := slow.Execute(cancelled, Invocation{Arguments: `{}`}); err == nil {
		t.Fatal("cancelled stdio MCP call returned no error")
	}
	crash, ok := reg.Get("mcp_local_crash")
	if !ok {
		reg.Close()
		t.Fatal("stdio MCP crash tool was not registered")
	}
	if _, err := crash.Execute(context.Background(), Invocation{Arguments: `{}`}); err == nil {
		t.Fatal("crashed stdio MCP call returned no error")
	}
	time.Sleep(mcpRestartBackoffInitial + 50*time.Millisecond)
	if _, err := tool.Execute(context.Background(), Invocation{Arguments: `{}`}); err != nil {
		reg.Close()
		t.Fatalf("stdio MCP did not restart after a crash: %v", err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("close stdio MCP: %v", err)
	}
}

// TestMCPStdioHelperProcess is re-executed as the child MCP server by the
// integration test above. It is inert in the ordinary parent test process.
func TestMCPStdioHelperProcess(t *testing.T) {
	if os.Getenv("MULLSMITH_MCP_TEST_HELPER") != "1" {
		return
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "stdio-test", Version: "1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "inspect", Description: "Inspect the constrained child environment"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			_ = os.WriteFile("helper-state", []byte("ok"), 0o600)
			text := strings.Join([]string{
				"home=" + os.Getenv("HOME"),
				"cwd=" + mustGetwd(),
				"allowed=" + os.Getenv("MCP_TEST_VALUE"),
				"parent_secret=" + os.Getenv("MULLSMITH_PARENT_SECRET"),
			}, "\n")
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}, nil, nil
		})
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "slow", Description: "Wait until cancelled"},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "crash", Description: "Exit the helper process"},
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, any, error) {
			os.Exit(23)
			return nil, nil, nil
		})
	_ = server.Run(context.Background(), &mcpsdk.StdioTransport{MaxLineLength: mcpMaxStdioFrameBytes})
}

func mustGetwd() string {
	dir, _ := os.Getwd()
	return dir
}

func TestMCPStderrBufferIsBounded(t *testing.T) {
	w := &boundedBuffer{max: 8}
	if n, err := w.Write([]byte("1234567890")); err != nil || n != 10 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := w.buf.String(); got != "12345678" || w.dropped != 2 {
		t.Fatalf("buffer = %q, dropped = %d", got, w.dropped)
	}
	if n, err := w.Write([]byte("abc")); err != nil || n != 3 || w.dropped != 5 {
		t.Fatalf("second Write = %d, %v; dropped = %d", n, err, w.dropped)
	}
}

func TestUnavailableStdioMCPDoesNotPreventRegistryBuild(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{MCP: config.MCP{StateDir: root, Servers: []config.MCPServer{{
		ID: "missing", Name: "Missing", Transport: "stdio",
		Command: filepath.Join(root, "does-not-exist"), StateDir: filepath.Join(root, "missing"),
		Approval: ApprovalConfirm,
	}}}}
	reg := NewRegistry(nil)
	registerMCP(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), reg, map[string]string{})
	defer reg.Close()
	if names := reg.Names(); len(names) != 0 {
		t.Fatalf("unavailable stdio server registered tools: %v", names)
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
