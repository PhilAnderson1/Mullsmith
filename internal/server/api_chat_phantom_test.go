package server

// Tool-call normalization regressions: endpoint IDs and arguments can arrive
// in arbitrary fragments. The browser must receive exactly one complete call,
// under the same application-owned ID used for persistence and protocol replay.
//
// The invariant: exactly ONE tool_start per completed call, and its call_id
// equals the id the protocol row is stored under, whatever the endpoint's
// fragmentation looked like.

import (
	"encoding/json"
	"testing"
)

// fragmentChunk builds one streaming tool_call fragment; empty id/name and a
// negative index are omitted entirely, like real endpoints do.
func fragmentChunk(idx int, id, name, args string) string {
	call := map[string]any{"function": map[string]any{"name": name, "arguments": args}}
	if id != "" {
		call["id"] = id
	}
	if idx >= 0 {
		call["index"] = idx
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{call}},
	}}})
	return string(b)
}

// phantomExchange drives a fs_read round whose fragments look like `round`,
// then a closing answer, and returns the drained stream plus the stored
// protocol rows.
func phantomExchange(t *testing.T, round []string) (eventStream, []struct {
	LLMCallID string `json:"llm_call_id"`
}, *authTest) {
	t.Helper()
	read := &fakeTool{name: "fs_read", content: `{"path":"p.txt","content":"payload","total_lines":1,"total_bytes":7}`}
	reg := registryFor(map[string]string{"fs_read": "auto"}, read)
	cfg := toolChatCfg(t, 4, map[string]string{"fs_read": "auto"})
	a, _ := newToolServer(t, cfg, reg, [][]string{
		round,
		{answerChunk("read it"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Phantom"})
	resp := sendChat(t, a, c.ID, "read p.txt")
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))
	if es.done == nil {
		t.Fatalf("generation did not complete: %v", es.events)
	}
	var page struct {
		Items []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"items"`
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	var stored []struct {
		LLMCallID string `json:"llm_call_id"`
	}
	for _, it := range page.Items {
		for _, tc := range it.ToolCalls {
			stored = append(stored, struct {
				LLMCallID string `json:"llm_call_id"`
			}{tc.ID})
		}
	}
	return es, stored, a
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// One tool call, id arriving late (name with no id first): exactly one
// tool_start, and its id is the id stored for the protocol row, so the live
// tool_end lands on the card and the reload shows the same single card.
func TestLateToolCallIDEmitsOneStartMatchingStoredRow(t *testing.T) {
	round := []string{
		fragmentChunk(0, "", "fs_read", ""),         // name first, NO id
		fragmentChunk(0, "cc-late", "", `{"path":`), // id arrives late
		fragmentChunk(0, "", "", `"p.txt"}`),        // args continue
		finishChunk("tool_calls"),
	}
	es, stored, _ := phantomExchange(t, round)
	if len(stored) != 1 {
		t.Fatalf("stored tool rows = %d, want 1", len(stored))
	}
	if n := countEvent(es, "tool_start"); n != 1 {
		t.Fatalf("tool_start count = %d, want exactly 1 — phantom card regression", n)
	}
	if got := stringOf(es.data["tool_start"]["call_id"]); got != stored[0].LLMCallID {
		t.Fatalf("card id %q != stored protocol id %q: live card and history diverge", got, stored[0].LLMCallID)
	}
	if stringOf(es.data["tool_end"]["call_id"]) != stored[0].LLMCallID {
		t.Errorf("tool_end did not land on the announced card")
	}
	if es.data["tool_start"]["arguments"] != `{"path":"p.txt"}` {
		t.Fatalf("complete arguments not announced: %v", es.data["tool_start"])
	}
}

// No id at all from the endpoint (Gufo style): the card's mint and the
// protocol row's id must be the SAME mint — one card live, matching history.
func TestEmptyUpstreamToolIDEmitsOneStartMatchingStoredRow(t *testing.T) {
	round := []string{
		fragmentChunk(0, "", "fs_read", `{"path":"p.txt"}`),
		finishChunk("tool_calls"),
	}
	es, stored, _ := phantomExchange(t, round)
	if len(stored) != 1 {
		t.Fatalf("stored tool rows = %d, want 1", len(stored))
	}
	if n := countEvent(es, "tool_start"); n != 1 {
		t.Fatalf("tool_start count = %d, want exactly 1 — phantom card regression", n)
	}
	if stringOf(es.data["tool_start"]["call_id"]) != stored[0].LLMCallID {
		t.Fatalf("card id %q != stored protocol id %q: live card and history diverge",
			stringOf(es.data["tool_start"]["call_id"]), stored[0].LLMCallID)
	}
	if stringOf(es.data["tool_end"]["call_id"]) != stored[0].LLMCallID {
		t.Errorf("tool_end did not land on the announced card")
	}
}

func countEvent(es eventStream, name string) int {
	n := 0
	for _, ev := range es.events {
		if ev == name {
			n++
		}
	}
	return n
}

func TestToolIDBeforeNamePublishesCoherentStart(t *testing.T) {
	es, stored, _ := phantomExchange(t, []string{
		fragmentChunk(0, "call_first", "", ""),
		fragmentChunk(0, "", "", `{"path":`),
		fragmentChunk(0, "", "fs_read", `"work/input.csv"}`), finishChunk("tool_calls"),
	})
	if es.data["tool_start"]["tool"] != "fs_read" || countEvent(es, "tool_start") != 1 {
		t.Fatalf("bad start: %v", es.data["tool_start"])
	}
	if len(stored) != 1 || es.data["tool_start"]["call_id"] != stored[0].LLMCallID {
		t.Fatalf("ID mismatch: %v %v", es.data["tool_start"], stored)
	}
	if es.data["tool_start"]["arguments"] != `{"path":"work/input.csv"}` {
		t.Fatalf("arguments lost: %v", es.data["tool_start"])
	}
}
