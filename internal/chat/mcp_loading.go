package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"mullsmith/internal/config"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
)

// conversationTools uses only tools currently available to this user. Persisted
// activation is not authorization, and never restores disconnected OAuth tools.
func (s *Service) conversationTools(conv *store.Conversation, userID string) []llm.ToolSpec {
	all := s.Tools.SpecsFor(userID)
	out := make([]llm.ToolSpec, 0, len(all))
	seen := map[string]bool{}
	for _, spec := range all {
		if spec.Function.Name == tools.ViewImageName && !s.cfg.LLM.VisionFor(s.Model(conv.Model)) {
			continue
		}
		tool, ok := s.Tools.GetFor(userID, spec.Function.Name)
		if !ok {
			continue
		}
		mcp, ok := tool.(interface{ MCPServerID() string })
		if ok {
			for _, server := range s.cfg.MCP.Servers {
				if server.ID != mcp.MCPServerID() || !server.DeferredLoad || slices.Contains(conv.LoadedMCP, server.ID) {
					continue
				}
				if !seen[server.ID] {
					loader := mcpLoader{server: server}
					out = append(out, llm.ToolSpec{Type: "function", Function: llm.ToolFunction{
						Name: loader.Name(), Description: loader.Description(), Parameters: loader.Parameters(),
					}})
					seen[server.ID] = true
				}
				goto next
			}
		}
		out = append(out, spec)
	next:
	}
	return out
}

type mcpLoader struct {
	server config.MCPServer
	load   func(context.Context) error
}

func (l mcpLoader) Name() string { return l.server.ID + "_load" }
func (l mcpLoader) Description() string {
	return "Load " + l.server.Name + " tools for this conversation. " + l.server.ProvidesSummary
}
func (l mcpLoader) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (l mcpLoader) Execute(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(inv.Arguments), &args); err != nil || args == nil || len(args) != 0 {
		return tools.Result{}, errors.New("loader arguments must be an empty JSON object")
	}
	if err := l.load(ctx); err != nil {
		return tools.Result{}, err
	}
	body, _ := json.Marshal(map[string]string{"loaded": l.server.ID})
	return tools.Result{Content: string(body), Summary: "Loaded " + l.server.Name + " tools", StatusOnly: true}, nil
}

func (s *Service) loaderFor(conv *store.Conversation, userID, name string, msgs []llm.ChatMessage) *mcpLoader {
	for _, server := range s.cfg.MCP.Servers {
		if !server.DeferredLoad || name != server.ID+"_load" {
			continue
		}
		return &mcpLoader{server: server, load: func(ctx context.Context) error {
			// Recheck availability at execution time, including OAuth disconnects.
			available := false
			for _, spec := range s.conversationTools(conv, userID) {
				if spec.Function.Name == name {
					available = true
				}
			}
			if !available {
				return errors.New("MCP server is no longer available to load")
			}
			candidate := *conv
			candidate.LoadedMCP = append(slices.Clone(conv.LoadedMCP), server.ID)
			result := llm.ChatMessage{Role: "tool", Content: `{"loaded":` + jsonQuote(server.ID) + `}`}
			prospective := append(slices.Clone(msgs), result)
			if s.continuationFull(conv, prospective, s.conversationTools(&candidate, userID)) {
				return errors.New("MCP tools do not fit in the remaining context; server not loaded")
			}
			if err := s.store.LoadMCPServer(ctx, userID, conv.ID, server.ID); err != nil {
				return errors.New("could not save MCP tool loading state")
			}
			conv.LoadedMCP = candidate.LoadedMCP
			return nil
		}}
	}
	return nil
}

func advertisedTool(specs []llm.ToolSpec, name string) bool {
	for _, spec := range specs {
		if spec.Function.Name == name {
			return true
		}
	}
	return false
}

// Preserve existing feedback for unknown tools and non-vision models.
func (s *Service) unavailableToolMessage(userID, name string, vision bool) string {
	if _, ok := s.Tools.GetFor(userID, name); !ok {
		return "unknown tool: " + name
	}
	if name == tools.ViewImageName && !vision {
		return "this model cannot see images"
	}
	return "tool is not loaded; load its MCP server first"
}
