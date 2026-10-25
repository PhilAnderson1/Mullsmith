package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"gabkeep/internal/config"
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
