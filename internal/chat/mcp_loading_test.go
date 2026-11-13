package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mullsmith/internal/agent"
	"mullsmith/internal/config"
	"mullsmith/internal/llm"
	"mullsmith/internal/tools"
)

type loadingTestTool struct{}

func (loadingTestTool) Name() string               { return "mcp_tasks_list" }
func (loadingTestTool) MCPServerID() string        { return "tasks" }
func (loadingTestTool) Description() string        { return "List tasks." }
func (loadingTestTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (loadingTestTool) Execute(context.Context, tools.Invocation) (tools.Result, error) {
	return tools.Result{Content: "[]"}, nil
}

type loadingProvider struct{ connected bool }

func (p *loadingProvider) Get(user, name string) (tools.Tool, bool) {
	return loadingTestTool{}, p.connected && user == "user" && name == "mcp_tasks_list"
}
func (p *loadingProvider) Specs(user string) []llm.ToolSpec {
	if _, ok := p.Get(user, "mcp_tasks_list"); !ok {
		return nil
	}
	t := loadingTestTool{}
	return []llm.ToolSpec{{Type: "function", Function: llm.ToolFunction{Name: t.Name(), Description: t.Description(), Parameters: t.Parameters()}}}
}
func (*loadingProvider) Policy(string, string) string { return tools.ApprovalConfirm }

func configureLoading(s *Service) *loadingProvider {
	s.cfg.MCP.Servers = []config.MCPServer{{ID: "tasks", Name: "Tasks", DeferredLoad: true, ProvidesSummary: "Manage tasks."}}
	p := &loadingProvider{connected: true}
	s.Tools.SetUserToolProvider(p)
	return p
}

func TestMCPLoadingPersistsAndRespectsUserAccess(t *testing.T) {
	s, db, conv := chatFixture(t)
	p := configureLoading(s)
	specs := s.conversationTools(conv, "user")
	if !advertisedTool(specs, "tasks_load") || advertisedTool(specs, "mcp_tasks_list") {
		t.Fatalf("unexpected initial specs: %+v", specs)
	}
	loader := s.loaderFor(conv, "user", "tasks_load", nil)
	if _, err := loader.Execute(context.Background(), tools.Invocation{Arguments: `{"extra":1}`}); err == nil {
		t.Fatal("accepted invalid args")
	}
	result, err := loader.Execute(context.Background(), tools.Invocation{Arguments: "{}"})
	if err != nil || result.Content != `{"loaded":"tasks"}` {
		t.Fatalf("%+v %v", result, err)
	}
	reloaded, err := db.ConversationForUser(context.Background(), "user", conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	specs = s.conversationTools(reloaded, "user")
	if advertisedTool(specs, "tasks_load") || !advertisedTool(specs, "mcp_tasks_list") {
		t.Fatal("activation not restored")
	}
	if s.Tools.PolicyFor("user", "mcp_tasks_list") != tools.ApprovalConfirm {
		t.Fatal("loading changed approval policy")
	}
	if advertisedTool(s.conversationTools(reloaded, "other"), "mcp_tasks_list") {
		t.Fatal("tools leaked to another user")
	}
	p.connected = false
	specs = s.conversationTools(reloaded, "user")
	if advertisedTool(specs, "tasks_load") || advertisedTool(specs, "mcp_tasks_list") {
		t.Fatal("disconnected tools advertised")
	}
	if err := db.LoadMCPServer(context.Background(), "other", conv.ID, "tasks"); err == nil {
		t.Fatal("cross-user activation accepted")
	}
}

func TestMCPLoadingRefusesContextOverflow(t *testing.T) {
	s, db, conv := chatFixture(t)
	configureLoading(s)
	s.Window = func(string) (int64, bool) { return 1, true }
	loader := s.loaderFor(conv, "user", "tasks_load", nil)
	_, err := loader.Execute(context.Background(), tools.Invocation{Arguments: "{}"})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected overflow refusal: %v", err)
	}
	saved, err := db.ConversationForUser(context.Background(), "user", conv.ID)
	if err != nil || len(saved.LoadedMCP) != 0 || len(conv.LoadedMCP) != 0 {
		t.Fatal("overflow persisted activation")
	}
}

func TestMCPLoaderUnavailableAtExecution(t *testing.T) {
	s, _, conv := chatFixture(t)
	p := configureLoading(s)
	loader := s.loaderFor(conv, "user", "tasks_load", nil)
	p.connected = false
	if _, err := loader.Execute(context.Background(), tools.Invocation{Arguments: "{}"}); err == nil {
		t.Fatal("disconnected server loaded")
	}
}

func TestMCPLoadingExpandsBetweenRoundsWithoutApproval(t *testing.T) {
	s, db, conv := chatFixture(t)
	configureLoading(s)
	rounds := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []llm.ToolSpec `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			if !advertisedTool(req.Tools, "tasks_load") || advertisedTool(req.Tools, "mcp_tasks_list") {
				t.Error("wrong initial tools")
			}
			fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"load","type":"function","function":{"name":"tasks_load","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			if !advertisedTool(req.Tools, "mcp_tasks_list") || advertisedTool(req.Tools, "tasks_load") {
				t.Error("tools not expanded")
			}
			fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"Ready."},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	s.cfg.LLM.Endpoint = upstream.URL + "/v1"
	s.generator = &agent.Generator{LLM: llm.NewClient(s.cfg, nil)}
	op, err := s.Begin(context.Background(), "user", Command{ConversationID: conv.ID, Content: "Show my tasks"})
	if err != nil {
		t.Fatal(err)
	}
	op.Run(func(event Event) error { return nil })
	if rounds != 2 {
		t.Fatalf("got %d rounds", rounds)
	}
	saved, err := db.ConversationForUser(context.Background(), "user", conv.ID)
	if err != nil || len(saved.LoadedMCP) != 1 {
		t.Fatalf("activation: %+v %v", saved, err)
	}
}

func TestMCPWithoutDeferralKeepsExistingBehavior(t *testing.T) {
	s, _, conv := chatFixture(t)
	configureLoading(s)
	s.cfg.MCP.Servers[0].DeferredLoad = false
	specs := s.conversationTools(conv, "user")
	if advertisedTool(specs, "tasks_load") || !advertisedTool(specs, "mcp_tasks_list") {
		t.Fatal("nondeferred tools changed")
	}
}
