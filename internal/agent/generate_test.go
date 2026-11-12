package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mullsmith/internal/config"
	"mullsmith/internal/id"
	"mullsmith/internal/llm"
)

// sseServer serves a canned OpenAI-style SSE body.
func sseServer(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprint(w, "data: ", c, "\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(ts.Close)
	return ts
}

func genFor(t *testing.T, ts *httptest.Server) *Generator {
	t.Helper()
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL + "/v1"
	cfg.LLM.Models = []config.LLMModel{{ID: "test-model", Default: true}}
	cfg.LLM.MaxTokensResponse = 128
	return &Generator{LLM: llm.NewClient(cfg, nil)}
}

func TestRoundAccumulatesToolCallsByIndex(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"reused","function":{"name":"web_fetch","arguments":"{\"u"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"reused","function":{"name":"web_search","arguments":"{\"q"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"rl\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"uery\":\"hi\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	g := genFor(t, ts)

	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil })
	if res.Status != "complete" {
		t.Fatalf("status = %s (err %v)", res.Status, res.Err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("calls = %+v, want 2", res.ToolCalls)
	}
	if !id.Valid(res.ToolCalls[0].ID) || !id.Valid(res.ToolCalls[1].ID) || res.ToolCalls[0].ID == res.ToolCalls[1].ID {
		t.Fatalf("endpoint IDs were not replaced by distinct canonical IDs: %+v", res.ToolCalls)
	}
	// Order follows the index even though index 1 streamed first.
	if res.ToolCalls[0].Function.Name != "web_search" || res.ToolCalls[1].Function.Name != "web_fetch" {
		t.Errorf("order wrong: %s then %s", res.ToolCalls[0].ID, res.ToolCalls[1].ID)
	}
	if got := res.ToolCalls[0].Function.Arguments; got != `{"query":"hi"}` {
		t.Errorf("args[0] = %s", got)
	}
	if got := res.ToolCalls[1].Function.Arguments; got != `{"url":"x"}` {
		t.Errorf("args[1] = %s", got)
	}
}

func TestRoundDropsNamelessCall(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"ghost","function":{"arguments":"junk"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"real","function":{"name":"web_search","arguments":"{}"}}]}}]}`,
	)
	g := genFor(t, ts)
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil })
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Name != "web_search" || !id.Valid(res.ToolCalls[0].ID) {
		t.Fatalf("calls = %+v, want only [real]", res.ToolCalls)
	}
}

func TestRoundContentAndReasoningSeparate(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"reasoning_content":"think ","reasoning_details":[{"type":"reasoning.text","text":"think ","index":0}]}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":42}}`,
	)
	g := genFor(t, ts)
	var got []string
	res := g.Round(context.Background(), "", nil, nil, nil, func(kind, text string) error {
		got = append(got, kind+":"+text)
		return nil
	})
	if res.Content != "answer" || res.Reasoning != "think " {
		t.Fatalf("content=%q reasoning=%q", res.Content, res.Reasoning)
	}
	if len(res.ReasoningDetails) != 1 {
		t.Fatalf("reasoning details = %s", res.ReasoningDetails)
	}
	want := []string{EventReasoning + ":think ", EventToken + ":answer"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", got, want)
	}
	if res.Usage != 42 {
		t.Errorf("usage = %d, want 42", res.Usage)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("unexpected tool calls: %+v", res.ToolCalls)
	}
}

// completion_tokens feeds the tok/s readout; without it the round falls
// back to the same estimate the total uses — never a zero.
func TestRoundCompletionTokens(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"content":"answer"}}],"usage":{"completion_tokens":9,"total_tokens":42}}`,
	)
	g := genFor(t, ts)
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil })
	if res.Usage != 42 || res.Completion != 9 {
		t.Errorf("usage=%d completion=%d, want 42/9", res.Usage, res.Completion)
	}
}

func TestRoundUsageEstimateUsesReportedCompletion(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"content":"answer"}}],"usage":{"completion_tokens":17}}`,
	)
	g := genFor(t, ts)
	msgs := []llm.ChatMessage{{Role: "user", Content: "question"}}
	res := g.Round(context.Background(), "", msgs, nil, nil, func(string, string) error { return nil })
	want := EstimateRequestTokens(msgs, nil) + 17
	if res.Usage != want || res.Completion != 17 {
		t.Errorf("usage=%d completion=%d, want %d/17", res.Usage, res.Completion, want)
	}
}

func TestRoundUsageEstimatedWhenUnreported(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"reasoning_content":"consider this"}}]}`,
		`{"choices":[{"delta":{"content":"some words came back"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	g := genFor(t, ts)
	msgs := []llm.ChatMessage{{Role: "assistant", Content: "earlier answer",
		Reasoning: "earlier reasoning", ReasoningField: "reasoning_content"}}
	tools := []llm.ToolSpec{{Type: "function", Function: llm.ToolFunction{
		Name: "lookup", Description: "look something up", Parameters: map[string]any{"type": "object"},
	}}}
	res := g.Round(context.Background(), "", msgs, tools, nil, func(string, string) error { return nil })
	wantCompletion := int64(EstimateTokens("some words came back") + EstimateTokens("consider this"))
	wantUsage := EstimateRequestTokens(msgs, tools) + wantCompletion
	if res.Usage != wantUsage || res.Completion != wantCompletion {
		t.Errorf("no upstream usage: usage=%d completion=%d, want %d/%d",
			res.Usage, res.Completion, wantUsage, wantCompletion)
	}
}

func TestRoundClientGoneIsAborted(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"content":"x"}}]}`,
		`{"choices":[{"delta":{"content":"y"}}]}`,
	)
	g := genFor(t, ts)
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error {
		return ErrClientGone
	})
	if res.Status != "aborted" {
		t.Fatalf("status = %s, want aborted", res.Status)
	}
}

// An endpoint that opens a tool call without an id still receives one stable
// application identity. A later endpoint id must not replace it.
func TestRoundOwnsLateEndpointToolCallID(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"web_search","arguments":"{\"q"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"arguments":"\":\"hi\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	g := genFor(t, ts)

	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil })
	if len(res.ToolCalls) != 1 {
		t.Fatalf("calls = %+v, want 1", res.ToolCalls)
	}
	final := res.ToolCalls[0].ID
	if final == "" || final == "call_late" {
		t.Fatalf("final id = %q: the first delta had no id, so a canonical one must be minted and kept", final)
	}
	if got := res.ToolCalls[0].Function.Arguments; got != `{"q":"hi"}` {
		t.Errorf("args = %s, want the late fragment to still contribute", got)
	}
}

// Endpoint IDs may repeat across calls and requests. Each invocation gets a
// unique application identity, regardless of the endpoint's proposed ID.
func TestRoundOwnsCallIDsAcrossRequests(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_first","function":{"name":"web_search","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	g := genFor(t, ts)
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil })
	again := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil })
	if len(res.ToolCalls) != 1 || len(again.ToolCalls) != 1 || !id.Valid(res.ToolCalls[0].ID) || res.ToolCalls[0].ID == again.ToolCalls[0].ID {
		t.Fatalf("endpoint ID leaked or repeated: %+v / %+v", res.ToolCalls, again.ToolCalls)
	}
}
