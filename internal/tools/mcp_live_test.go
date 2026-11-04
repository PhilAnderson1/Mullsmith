package tools

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"mullsmith/internal/config"
)

func TestLiveDeepWikiAsk(t *testing.T) {
	if os.Getenv("MULLSMITH_LIVE_MCP_TEST") != "1" {
		t.Skip("set MULLSMITH_LIVE_MCP_TEST=1 to call the public DeepWiki server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, _, closeSession, err := connectMCP(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), config.MCPServer{
		ID: "deepwiki", Name: "DeepWiki", Transport: "streamable_http", URL: "https://mcp.deepwiki.com/mcp", Approval: "confirm",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer closeSession()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "ask_wiki_question", Arguments: map[string]any{
		"repoName": "golang/go",
		"question": "How does net/http Server.Shutdown handle active and idle connections? Describe the implementation structures involved.",
	}})
	if err != nil {
		t.Fatalf("call: %T: %v", err, err)
	}
	if result.IsError {
		t.Fatalf("tool error: %#v", result.Content)
	}
}
