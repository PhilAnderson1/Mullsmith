package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/thinking"
)

// sseHandler serves canned SSE bodies with OpenAI chunk framing.
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func clientFor(ts *httptest.Server, transport http.RoundTripper) *Client {
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL + "/v1"
	cfg.LLM.Model = "test-model"
	cfg.LLM.MaxTokensResponse = 128
	return NewClient(cfg, transport)
}

// collect drains a Stream into one content/reasoning string.
func collect(t *testing.T, c *Client, msgs []ChatMessage) (content, reasoning string, usage int64, err error) {
	t.Helper()
	err = c.Stream(context.Background(), "", msgs, func(d Delta) error {
		content += d.Content
		reasoning += d.Reasoning
		if d.Usage > 0 {
			usage = d.Usage
		}
		return nil
	})
	return
}

func TestStreamHappyPath(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":", world"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"usage":{"total_tokens":42}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	ts := sseServer(t, body)

	content, reasoning, usage, err := collect(t, clientFor(ts, nil), []ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if content != "Hello, world" {
		t.Errorf("content = %q", content)
	}
	if reasoning != "" {
		t.Errorf("reasoning = %q, want empty", reasoning)
	}
	if usage != 42 {
		t.Errorf("usage = %d, want 42", usage)
	}
}

// Usage arrives on the final chunk; completion_tokens rides alongside
// total_tokens and powers the per-message tok/s readout.
func TestStreamUsageCompletionTokens(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: {"usage":{"completion_tokens":7,"total_tokens":99}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	ts := sseServer(t, body)

	var usage, completion int64
	err := clientFor(ts, nil).Stream(context.Background(), "", nil, func(d Delta) error {
		if d.Usage > 0 {
			usage = d.Usage
		}
		if d.Completion > 0 {
			completion = d.Completion
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage != 99 || completion != 7 {
		t.Errorf("usage=%d completion=%d, want 99/7", usage, completion)
	}
}

func TestStreamReasoningKeptSeparate(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think "}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"hard"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"answer"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	ts := sseServer(t, body)

	content, reasoning, _, err := collect(t, clientFor(ts, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if content != "answer" {
		t.Errorf("content = %q", content)
	}
	if reasoning != "think hard" {
		t.Errorf("reasoning = %q", reasoning)
	}
}

func TestStreamEOFWithoutDoneTolerated(t *testing.T) {
	// Upstream closes right after the last chunk: no [DONE], no finish_reason.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	ts := sseServer(t, body)

	content, _, _, err := collect(t, clientFor(ts, nil), nil)
	if err != nil {
		t.Fatalf("EOF without [DONE] must be tolerated, got %v", err)
	}
	if content != "partial" {
		t.Errorf("content = %q", content)
	}
}

func TestStreamSkipsMalformedAndForeignLines(t *testing.T) {
	body := strings.Join([]string{
		`: keepalive comment`,
		`event: message`,
		`data: {this is not json`,
		``,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"!"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	ts := sseServer(t, body)

	content, _, _, err := collect(t, clientFor(ts, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if content != "ok!" {
		t.Errorf("content = %q, want the valid chunks only", content)
	}
}

func TestStreamNonOKStatusIsErrUpstream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusTooManyRequests)
	}))
	defer ts.Close()

	_, _, _, err := collect(t, clientFor(ts, nil), nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("err = %v, want sanitized HTTP 429 failure", err)
	}
}

// flakyTransport refuses the first connection, then proxies normally.
type flakyTransport struct {
	attempts int
	inner    http.RoundTripper
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.attempts++
	if f.attempts == 1 {
		return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	}
	return f.inner.RoundTrip(req)
}

func TestStreamRetriesOnceOnConnRefused(t *testing.T) {
	ts := sseServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: [DONE]\n\n")
	tr := &flakyTransport{inner: http.DefaultTransport}

	content, _, _, err := collect(t, clientFor(ts, tr), nil)
	if err != nil {
		t.Fatal(err)
	}
	if content != "recovered" {
		t.Errorf("content = %q", content)
	}
	if tr.attempts != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one retry)", tr.attempts)
	}
}

func TestStreamNoRetryAfterDeltaSeen(t *testing.T) {
	// A server that streams once then dies; a second call must NOT be made
	// after content flowed. Counting upstream hits proves it.
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Panic mid-response kills the connection: the client sees a read
		// error after deltas arrived.
		panic(http.ErrAbortHandler)
	}))
	defer ts.Close()

	_, _, _, err := collect(t, clientFor(ts, nil), nil)
	if err == nil {
		t.Fatal("expected a stream error")
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (no retry once deltas flowed)", hits)
	}
}

func TestStreamCancelAborts(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hang until the client hangs up
	}))
	defer ts.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		err := clientFor(ts, nil).Stream(ctx, "", nil, func(Delta) error { return nil })
		got <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the first delta arrive
	cancel()

	select {
	case err := <-got:
		if err != ErrAborted {
			t.Errorf("err = %v, want ErrAborted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stream did not return after context cancellation")
	}
}

func TestStreamRequestBody(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("request Content-Type = %q", ct)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		} else {
			if req["model"] != "other-model" || req["stream"] != true {
				t.Errorf("model/stream = %v/%v", req["model"], req["stream"])
			}
			if req["max_tokens"] != float64(64) || req["temperature"] != float64(0.7) {
				t.Errorf("max_tokens/temperature = %v/%v", req["max_tokens"], req["temperature"])
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL + "/v1"
	cfg.LLM.Model = "default-model"
	cfg.LLM.APIKey = "sekret"
	cfg.LLM.MaxTokensResponse = 64
	cfg.LLM.DefaultTemperature = 0.7

	c := NewClient(cfg, nil)
	if err := c.Stream(context.Background(), "other-model", []ChatMessage{{Role: "user", Content: "x"}},
		func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// --- M4: tool protocol -----------------------------------------------------------

func TestStreamToolCallFragments(t *testing.T) {
	// Two calls delivered OpenAI-style: fragments share index, the first
	// carries id+name, later ones append arguments. Both fragments of a
	// two-call chunk may omit index — position within the chunk then wins.
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"web_search","arguments":"{\"que"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ry\":\"hello\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_A2","function":{"arguments":"tail"}},{"id":"call_B","function":{"name":"web_fetch","arguments":"{\"url\":\"http://x/\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	ts := sseServer(t, body)
	c := clientFor(ts, nil)

	var frags []ToolCallDelta
	var finish string
	var done bool
	err := c.Stream(context.Background(), "", nil, func(d Delta) error {
		frags = append(frags, d.ToolCalls...)
		if d.Finish != "" {
			finish = d.Finish
		}
		done = d.Done
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if finish != "tool_calls" || !done {
		t.Fatalf("finish=%q done=%v", finish, done)
	}
	if len(frags) != 4 {
		t.Fatalf("frags = %+v, want 4", frags)
	}
	// Indexless two-call chunk: positions distinguish the calls.
	if frags[2].Index != 0 || frags[2].ID != "call_A2" {
		t.Errorf("indexless fragment 0 parsed wrong: %+v", frags[2])
	}
	if frags[3].Index != 1 || frags[3].ID != "call_B" || frags[3].Name != "web_fetch" {
		t.Errorf("indexless fragment 1 parsed wrong: %+v", frags[3])
	}
	if frags[0].ID != "call_A" || frags[0].Name != "web_search" {
		t.Errorf("first fragment = %+v", frags[0])
	}
}

func TestStreamToolsPayload(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()
	c := clientFor(ts, nil)

	tools := []ToolSpec{{Type: "function", Function: ToolFunction{
		Name: "web_search", Description: "search", Parameters: map[string]any{"type": "object"},
	}}}
	msgs := []ChatMessage{
		{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "c1", Type: "function",
			Function: ChatToolCallFunc{Name: "web_search", Arguments: `{"query":"x"}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "{}"},
	}
	if err := c.StreamTools(context.Background(), "m", msgs, tools, nil, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if tc, ok := gotBody["tools"].([]any); !ok || len(tc) != 1 {
		t.Fatalf("tools not in payload: %v", gotBody["tools"])
	}
	if gotBody["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", gotBody["tool_choice"])
	}
	// Replay rows must serialize with protocol field names.
	raw, _ := json.Marshal(gotBody["messages"])
	s := string(raw)
	for _, want := range []string{`"tool_calls"`, `"tool_call_id"`, `"c1"`, `"web_search"`} {
		if !strings.Contains(s, want) {
			t.Errorf("messages payload missing %s: %s", want, s)
		}
	}
}

func TestStreamWithoutToolsOmitsFields(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()
	c := clientFor(ts, nil)
	if err := c.Stream(context.Background(), "m", []ChatMessage{{Role: "user", Content: "hi"}},
		func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody["tools"]; ok {
		t.Error("tools present with no tool specs — confuses non-tool endpoints")
	}
	if _, ok := gotBody["tool_choice"]; ok {
		t.Error("tool_choice present with no tool specs")
	}
}

func TestChatMessageMarshalsImagesAsContentParts(t *testing.T) {
	// No images → the wire shape is untouched: content stays a plain string.
	// Text-only endpoints must never see the parts array.
	plain, err := json.Marshal(ChatMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), `"content":"hi"`) {
		t.Errorf("plain content = %s, want a string", plain)
	}

	// Images → OpenAI parts array, text block first, then one per picture.
	b, err := json.Marshal(ChatMessage{
		Role:    "user",
		Content: "look",
		Images:  []string{"data:image/png;base64,AAA=", "data:image/webp;base64,BBB="},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("parts body not decodable: %v — %s", err, b)
	}
	if got.Role != "user" || len(got.Content) != 3 {
		t.Fatalf("parts = %+v", got.Content)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "look" {
		t.Errorf("part 0 = %+v, want the text block first", got.Content[0])
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL.URL != "data:image/png;base64,AAA=" {
		t.Errorf("part 1 = %+v", got.Content[1])
	}
	if got.Content[2].Type != "image_url" || got.Content[2].ImageURL.URL != "data:image/webp;base64,BBB=" {
		t.Errorf("part 2 = %+v", got.Content[2])
	}

	// Image-only content (no text) still yields a valid parts array.
	b2, err := json.Marshal(ChatMessage{Role: "user", Images: []string{"data:image/jpeg;base64,C=="}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), `"type":"image_url"`) {
		t.Errorf("image-only body = %s", b2)
	}
}

// --- thinking fragments on the wire (thinking-control-specification.md) ----

// captureServer records each request's path and body, answering with a
// minimal finished stream.
func captureServer(t *testing.T) (*httptest.Server, func() (string, map[string]any)) {
	t.Helper()
	var (
		mu   sync.Mutex
		path string
		body map[string]any
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		path, body = r.URL.Path, b
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(ts.Close)
	return ts, func() (string, map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		return path, body
	}
}

// gufoQwen38 resolves the shipped gufo/qwen3_8 pair from the repo's
// catalogue files — the catalogues live only there now.
func gufoQwen38(t *testing.T) *thinking.Runtime {
	t.Helper()
	rt, err := thinking.Build(thinking.Settings{
		EndpointType:  "gufo",
		ProfileID:     "qwen3_8",
		EndpointsFile: filepath.Join("..", "..", "configs", "endpoints.yaml"),
		ProfilesFile:  filepath.Join("..", "..", "configs", "thinking-profiles.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// The endpoint definition's path joins the base URL exactly once — the
// reverse-proxy prefix in the URL survives.
func TestClientRouteFromEndpointDefinition(t *testing.T) {
	ts, last := captureServer(t)
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL + "/proxy/v1"
	cfg.LLM.Model = "m"
	cfg.LLM.MaxTokensResponse = 128
	cfg.Thinking = gufoQwen38(t)
	c := NewClient(cfg, nil)
	if err := c.Stream(context.Background(), "", nil, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if path, _ := last(); path != "/proxy/v1/chat/completions" {
		t.Errorf("path = %q, want /proxy/v1/chat/completions", path)
	}
}

// A rendered fragment rides the normal body (no extra_body wrapper);
// Default (nil) leaves no thinking field of any kind behind, so a later
// request can never inherit an earlier turn's effort or budget.
func TestStreamThinkingFragment(t *testing.T) {
	ts, last := captureServer(t)
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL + "/v1"
	cfg.LLM.Model = "m"
	cfg.LLM.MaxTokensResponse = 128
	rt := gufoQwen38(t)
	cfg.Thinking = rt
	c := NewClient(cfg, nil)

	high, _ := rt.Resolve("high")
	frag, err := rt.Fragment(high)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StreamTools(context.Background(), "", nil, nil, frag,
		func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, body := last()
	if body["reasoning_effort"] != "xhigh" {
		t.Errorf("body = %v, want reasoning_effort xhigh", body)
	}
	if ctk, ok := body["chat_template_kwargs"].(map[string]any); !ok || ctk["enable_thinking"] != true {
		t.Errorf("chat_template_kwargs = %v, want enable_thinking true", body["chat_template_kwargs"])
	}

	// Default after High: fresh body, no leftover thinking fields.
	if err := c.StreamTools(context.Background(), "", nil, nil, nil,
		func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, body = last()
	for _, k := range []string{"reasoning_effort", "chat_template_kwargs", "reasoning_budget_tokens"} {
		if _, found := body[k]; found {
			t.Errorf("Default request carries stale %q: %v", k, body)
		}
	}
}

// mergeJSON preserves unrelated fields inside nested objects (§6).
func TestMergeJSONPreservesUnrelatedFields(t *testing.T) {
	dst := map[string]any{
		"chat_template_kwargs": map[string]any{"skip_special": true},
		"temperature":          0.7,
	}
	src := map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": true},
	}
	mergeJSON(dst, src)
	ctk := dst["chat_template_kwargs"].(map[string]any)
	if ctk["skip_special"] != true || ctk["enable_thinking"] != true {
		t.Errorf("merged kwargs = %v, want both keys", ctk)
	}
	// src must not be aliased into dst: later mutation of dst cannot
	// corrupt the shared fragment.
	ctk["skip_special"] = false
	if src["chat_template_kwargs"].(map[string]any)["skip_special"] != nil {
		t.Error("merge aliased the source map")
	}
}

// The reasoning aliases: same trace under both spellings must reach the
// consumer exactly once; either one alone still arrives; and content in
// the same chunk is processed independently.
func TestReasoningAliasesDedupe(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think","reasoning":"think"}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning":"only"}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"rc","content":"answer"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
	}, "\n")
	ts := sseServer(t, body)
	_, reasoning, _, err := collect(t, clientFor(ts, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if reasoning != "thinkonlyrc" {
		t.Errorf("reasoning = %q, want thinkonlyrc (no duplication)", reasoning)
	}
}
