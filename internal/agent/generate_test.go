package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mullsmith/internal/config"
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
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"web_fetch","arguments":"{\"u"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"web_search","arguments":"{\"q"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"rl\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"uery\":\"hi\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	g := genFor(t, ts)

	var deltaEvents int
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil },
		func(e ToolEvent) {
			if e.Kind == "tool_args" {
				deltaEvents++
			}
		})
	if res.Status != "complete" {
		t.Fatalf("status = %s (err %v)", res.Status, res.Err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("calls = %+v, want 2", res.ToolCalls)
	}
	// Order follows the index even though index 1 streamed first.
	if res.ToolCalls[0].ID != "call_a" || res.ToolCalls[1].ID != "call_b" {
		t.Errorf("order wrong: %s then %s", res.ToolCalls[0].ID, res.ToolCalls[1].ID)
	}
	if got := res.ToolCalls[0].Function.Arguments; got != `{"query":"hi"}` {
		t.Errorf("args[0] = %s", got)
	}
	if got := res.ToolCalls[1].Function.Arguments; got != `{"url":"x"}` {
		t.Errorf("args[1] = %s", got)
	}
	if deltaEvents != 4 {
		t.Errorf("toolDelta saw %d events, want 4 (raw pass-through)", deltaEvents)
	}
}

func TestRoundDropsNamelessCall(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"ghost","function":{"arguments":"junk"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"real","function":{"name":"web_search","arguments":"{}"}}]}}]}`,
	)
	g := genFor(t, ts)
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil }, nil)
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "real" {
		t.Fatalf("calls = %+v, want only [real]", res.ToolCalls)
	}
}

func TestRoundContentAndReasoningSeparate(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"reasoning_content":"think "}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":42}}`,
	)
	g := genFor(t, ts)
	var got []string
	res := g.Round(context.Background(), "", nil, nil, nil, func(kind, text string) error {
		got = append(got, kind+":"+text)
		return nil
	}, nil)
	if res.Content != "answer" || res.Reasoning != "think " {
		t.Fatalf("content=%q reasoning=%q", res.Content, res.Reasoning)
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
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil }, nil)
	if res.Usage != 42 || res.Completion != 9 {
		t.Errorf("usage=%d completion=%d, want 42/9", res.Usage, res.Completion)
	}
}

func TestRoundUsageEstimatedWhenUnreported(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"content":"some words came back"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	g := genFor(t, ts)
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil }, nil)
	if res.Usage <= 0 || res.Completion <= 0 {
		t.Errorf("no upstream usage: usage=%d completion=%d, want estimates > 0",
			res.Usage, res.Completion)
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
	}, nil)
	if res.Status != "aborted" {
		t.Fatalf("status = %s, want aborted", res.Status)
	}
}

// Review2 finding 5: an endpoint that opens a tool call WITHOUT an id gets
// one minted at the first delta — and every live card is keyed by it from
// then on. A LATER fragment carrying the endpoint's own id must not
// replace the published identity, or the completed call, its audit row and
// its replayed protocol answer to a different id than the cards on screen.
func TestRoundFreezesPublishedToolCallID(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"web_search","arguments":"{\"q"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"arguments":"\":\"hi\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	g := genFor(t, ts)

	var published []string
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil },
		func(e ToolEvent) { published = append(published, e.CallID) })
	if len(res.ToolCalls) != 1 {
		t.Fatalf("calls = %+v, want 1", res.ToolCalls)
	}
	final := res.ToolCalls[0].ID
	if final == "" || final == "call_late" {
		t.Fatalf("final id = %q: the first delta had no id, so a canonical one must be minted and kept", final)
	}
	for i, id := range published {
		if id != final {
			t.Fatalf("delta %d published id %q, want the frozen %q", i, id, final)
		}
	}
	if got := res.ToolCalls[0].Function.Arguments; got != `{"q":"hi"}` {
		t.Errorf("args = %s, want the late fragment to still contribute", got)
	}
}

// The mirror case (finding 6): an endpoint that DOES send an id keeps it —
// absorb takes the endpoint's identity whenever it arrives before one has
// been published.
func TestRoundKeepsEndpointIDWhenPresent(t *testing.T) {
	ts := sseServer(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_first","function":{"name":"web_search","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	g := genFor(t, ts)
	res := g.Round(context.Background(), "", nil, nil, nil, func(string, string) error { return nil }, nil)
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_first" {
		t.Fatalf("calls = %+v, want the endpoint's own id kept", res.ToolCalls)
	}
}
