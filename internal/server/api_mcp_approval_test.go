package server

import (
	"testing"
)

type approvalMCPTool struct {
	*fakeTool
	serverID string
}

func (t *approvalMCPTool) MCPServerID() string { return t.serverID }

func TestMCPRememberApprovalScope(t *testing.T) {
	list := &approvalMCPTool{&fakeTool{name: "mcp_tasks_team_list", content: `{}`}, "tasks_team"}
	other := &approvalMCPTool{&fakeTool{name: "mcp_tasks_team_find-by-date", content: `{}`}, "tasks_team"}
	separate := &approvalMCPTool{&fakeTool{name: "mcp_tasks_team_delete", content: `{}`}, "tasks"}
	reg := registryFor(nil, list, other, separate)
	call := func(id, name string) []string {
		return []string{toolCallChunk(0, id, name, `{}`), finishChunk("tool_calls")}
	}
	answer := []string{answerChunk("done"), finishChunk("stop")}
	a, _ := newToolServer(t, toolChatCfg(t, 6, nil), reg, [][]string{
		call("1", list.name), call("2", list.name), call("3", other.name), call("separate", separate.name), answer,
		call("4", list.name), answer,
		call("5", list.name), answer,
	})
	c := createConv(t, a, map[string]any{"title": "Remember MCP"})
	run := func(conv string, decisions []decision) chatRun {
		resp := sendChat(t, a, conv, "check tasks")
		defer resp.Body.Close()
		return driveChat(t, a, newSSEReader(t, resp), decisions)
	}
	first := run(c.ID, []decision{{approve: true, remember: true}, {approve: false}})
	if len(first.approvals) != 2 || list.callCount() != 2 || other.callCount() != 1 || separate.callCount() != 0 {
		t.Fatalf("unexpected scope: prompts=%v list=%d other=%d", first.approvals, list.callCount(), other.callCount())
	}
	for _, prompt := range first.approvals {
		if prompt["rememberable"] != true {
			t.Fatalf("MCP checkbox not offered: %v", prompt)
		}
	}
	// Consent survives a later user message in the same conversation.
	run(c.ID, nil)
	fresh := createConv(t, a, map[string]any{"title": "Fresh MCP"})
	run(fresh.ID, []decision{{approve: true}})
	if list.callCount() != 4 {
		t.Fatalf("list ran %d times", list.callCount())
	}
}

func TestRememberApprovalRequiresConsentAndEligibleTool(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		approve, remember, eligible bool
		runs                        int
	}{
		{"mcp_tasks_list", true, false, true, 2},
		{"mcp_tasks_list", false, true, true, 1},
		{"fs_delete", true, true, false, 2},
	} {
		t.Run(tc.name+"_"+map[bool]string{true: "approve", false: "deny"}[tc.approve], func(t *testing.T) {
			tool := &approvalMCPTool{fakeTool: &fakeTool{name: tc.name, content: `{}`}}
			if tc.eligible {
				tool.serverID = "tasks"
			}
			a, _ := newToolServer(t, toolChatCfg(t, 4, nil), registryFor(nil, tool), [][]string{
				{toolCallChunk(0, "1", tc.name, `{}`), finishChunk("tool_calls")},
				{toolCallChunk(0, "2", tc.name, `{}`), finishChunk("tool_calls")},
				{answerChunk("done"), finishChunk("stop")},
			})
			c := createConv(t, a, map[string]any{"title": "Consent"})
			resp := sendChat(t, a, c.ID, "run")
			defer resp.Body.Close()
			result := driveChat(t, a, newSSEReader(t, resp), []decision{{approve: tc.approve, remember: tc.remember}, {approve: true}})
			if tool.callCount() != tc.runs {
				t.Fatalf("executions=%d want %d", tool.callCount(), tc.runs)
			}
			for _, prompt := range result.approvals {
				if prompt["rememberable"] != tc.eligible {
					t.Fatalf("checkbox=%v want %v", prompt, tc.eligible)
				}
			}
		})
	}
}
