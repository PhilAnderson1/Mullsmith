package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mullsmith/internal/id"
	"mullsmith/internal/store"
)

// A terminal reason must survive a reload, and an incomplete tool proposal
// must never execute side effects or start a continuation.
func TestModelFinishReasonIsDurable(t *testing.T) {
	for _, tc := range []struct {
		name, reason, note string
		tool               bool
	}{
		{name: "normal answer", reason: "stop"},
		{name: "normal tools", reason: "tool_calls", tool: true},
		{name: "output limit", reason: "length", note: "The model reached its output limit before finishing this reply."},
		{name: "output limit during tool proposal", reason: "length", tool: true, note: "The model reached its output limit before finishing this reply."},
		{name: "filtered answer", reason: "content_filter", note: "The model stopped this reply because of its content filter."},
		{name: "filtered tool proposal", reason: "content_filter", tool: true, note: "The model stopped this reply because of its content filter."},
		{name: "unknown reason", reason: "unexpected", note: "The model stopped this reply for an unexpected reason."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &fakeTool{name: "web_search", content: `{}`, summary: "ran"}
			reg := registryFor(map[string]string{"web_search": "auto"}, tool)
			cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
			first := []string{answerChunk("Partial answer.")}
			if tc.tool {
				first = append(first, toolCallChunk(0, "call_1", "web_search", `{"query":"test"}`))
			}
			first = append(first, finishChunk(tc.reason))
			a, script := newToolServer(t, cfg, reg, [][]string{first, {answerChunk("Final answer."), finishChunk("stop")}})
			conv := createConv(t, a, map[string]any{"title": "Finish reason"})
			resp := sendChat(t, a, conv.ID, "answer")
			defer resp.Body.Close()
			events := drainSSE(t, newSSEReader(t, resp))
			status, wantRounds, wantCalls := "complete", 1, 0
			if tc.note != "" {
				status = "error"
			} else if tc.tool {
				wantRounds, wantCalls = 2, 1
			}
			if events.done["status"] != status || script.roundCount() != wantRounds || tool.callCount() != wantCalls {
				t.Fatalf("done=%v model rounds=%d tool calls=%d", events.done, script.roundCount(), tool.callCount())
			}
			outcomes := roundsOf(t, events.done)
			if len(outcomes) != wantRounds {
				t.Fatalf("outcomes=%v", outcomes)
			}
			var page messageHistoryPage
			getJSON(t, a, "/api/v1/conversations/"+conv.ID+"/messages", &page)
			last := page.Items[len(page.Items)-1]
			if last.Status != status || last.StatusNote != tc.note || last.ID != lastRoundID(t, events.done) {
				t.Fatalf("reload disagrees with terminal outcome: %+v", last)
			}
			if tc.note != "" && outcomes[0]["note"] != tc.note {
				t.Fatalf("live note=%v want=%q", outcomes[0]["note"], tc.note)
			}
		})
	}
}

// Hidden protocol rows must not consume a page's visible-message allowance.
// They remain available in context replay, while display payloads are the
// only tool results sent back to the browser.
func TestHistoryPagesContainOnlyDisplayData(t *testing.T) {
	a := signedIn(t)
	user, err := a.db.UserByEmail(context.Background(), "owner@b.io")
	if err != nil || user == nil {
		t.Fatalf("owner lookup: %v", err)
	}
	conv := createConv(t, a, map[string]any{"title": "Visible pages"})
	ctx := context.Background()
	for i := range 5 {
		m := &store.Message{ConversationID: conv.ID, Role: "assistant", Content: fmt.Sprint(i), CreatedAt: time.Unix(int64(100+i*3), 0)}
		if err := a.db.AddMessage(ctx, user.ID, m); err != nil {
			t.Fatal(err)
		}
		call := &store.ToolCall{MessageID: m.ID, LLMCallID: id.New(), Tool: "fs_read", Arguments: `{}`, Status: "running"}
		if err := a.db.AddToolCall(ctx, call); err != nil {
			t.Fatal(err)
		}
		if err := a.db.FinishToolCall(ctx, call.ID, "ok", "MODEL_ONLY_SENTINEL", `{"summary":"read","data":null}`, 1); err != nil {
			t.Fatal(err)
		}
		for _, hidden := range []*store.Message{
			{ConversationID: conv.ID, Role: "tool", Content: "MODEL_ONLY_SENTINEL", ToolCallID: call.LLMCallID, CreatedAt: time.Unix(int64(101+i*3), 0)},
			{ConversationID: conv.ID, Role: "user", Content: "SYNTHETIC_SENTINEL", Synthetic: true, CreatedAt: time.Unix(int64(102+i*3), 0)},
		} {
			if err := a.db.AddMessage(ctx, user.ID, hidden); err != nil {
				t.Fatal(err)
			}
		}
	}
	var seen []string
	cursor := ""
	for {
		var page struct {
			Items []map[string]any `json:"items"`
			Next  string           `json:"next_cursor"`
		}
		getJSON(t, a, "/api/v1/conversations/"+conv.ID+"/messages?limit=2&cursor="+cursor, &page)
		if len(page.Items) == 0 || len(page.Items) > 2 {
			t.Fatalf("invalid visible page: %+v", page)
		}
		for _, item := range page.Items {
			if item["role"] != "assistant" {
				t.Fatalf("hidden row leaked: %v", item)
			}
			seen = append(seen, item["content"].(string))
			calls := item["tool_calls"].([]any)
			call := calls[0].(map[string]any)
			if _, exists := call["result"]; exists {
				t.Fatal("model result leaked into browser history")
			}
			if call["display"] != `{"summary":"read","data":null}` {
				t.Fatalf("card display lost: %v", call)
			}
		}
		cursor = page.Next
		if cursor == "" {
			break
		}
	}
	if fmt.Sprint(seen) != "[3 4 1 2 0]" {
		t.Fatalf("page ordering=%v", seen)
	}
	history, err := a.db.ContextHistory(ctx, conv.ID)
	if err != nil || len(history) != 15 {
		t.Fatalf("model history lost rows: %d (%v)", len(history), err)
	}
	if history[1].Content != "MODEL_ONLY_SENTINEL" || history[0].Calls[0].Result != "MODEL_ONLY_SENTINEL" {
		t.Fatal("stored tool result lost")
	}
}
