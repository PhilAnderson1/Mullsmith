package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"gabkeep/internal/config"
)

// ChatMessage is one message in a chat/completions request. The tool fields
// carry the OpenAI tool protocol (spec §6.1 step 5): assistant rows replay
// their ToolCalls, and role "tool" rows answer one call via ToolCallID.
// Images carries inline pictures as data: URLs (llm.vision): when non-empty,
// MarshalJSON sends Content plus Images as OpenAI content parts. Only user
// messages ever carry images — a message never mixes parts with the tool
// protocol.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Images     []string       `json:"-"`
}

// MarshalJSON speaks both content shapes: the plain string content every
// protocol row uses, and — when Images is set — the OpenAI multimodal parts
// array [{type:"text"},{type:"image_url"}…]. Text-only endpoints are never
// shown the array shape: ChatMessages only get Images when llm.vision is on.
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	if len(m.Images) == 0 {
		type plain ChatMessage
		return json.Marshal(plain(m))
	}
	parts := make([]any, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, url := range m.Images {
		parts = append(parts, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": url},
		})
	}
	return json.Marshal(map[string]any{"role": m.Role, "content": parts})
}

// ChatToolCall is a function call as replayed inside an assistant message.
type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ChatToolCallFunc `json:"function"`
}

type ChatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string from the model
}

// ToolSpec advertises one tool to the model (OpenAI function-calling form).
type ToolSpec struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"` // JSON Schema
}

// Delta is one parsed streaming update.
type Delta struct {
	Content    string // answer text
	Reasoning  string // chain-of-thought, never part of Content (spec §6.2)
	ToolCalls  []ToolCallDelta
	Finish     string // finish_reason when seen ("stop", "tool_calls", …)
	Done       bool   // finish_reason seen
	Usage      int64  // total tokens if the upstream reported one
	Completion int64  // completion tokens if the upstream reported them
}

// ToolCallDelta is a fragment of a streamed tool call. Fragments of the same
// call share Index; the first carries ID/Name, later ones append Arguments
// (OpenAI streaming convention).
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Client talks to one OpenAI-compatible endpoint (spec §6.2).
type Client struct {
	endpoint    string
	route       string // request path appended to endpoint (default /chat/completions)
	apiKey      string
	model       string
	maxTokens   int
	temperature float64
	http        *http.Client
}

// NewClient builds the streaming client. Transport may be nil for defaults
// (tests inject a fake one to exercise the retry path). With the thinking
// control configured, the endpoint definition's path replaces the default
// route — joined to the base URL exactly once, keeping any reverse-proxy
// prefix the URL carries (thinking-control-specification.md §3).
func NewClient(cfg *config.Config, transport http.RoundTripper) *Client {
	route := "/chat/completions"
	if r := cfg.Thinking; r.Enabled() && r.Route() != "" {
		route = r.Route()
	}
	return &Client{
		endpoint:    strings.TrimRight(cfg.LLM.Endpoint, "/"),
		route:       route,
		apiKey:      cfg.LLM.APIKey,
		model:       cfg.LLM.Model,
		maxTokens:   cfg.LLM.MaxTokensResponse,
		temperature: cfg.LLM.DefaultTemperature,
		http:        &http.Client{Transport: transport}, // no global timeout: streaming
	}
}

// url is the request target: base URL with its path intact, route suffix
// appended once.
func (c *Client) url() string { return c.endpoint + c.route }

var (
	// ErrUpstream is a sanitized upstream failure (spec §9.12: no internals
	// in client-visible messages; the detail goes to the server log only).
	ErrUpstream = errors.New("llm upstream error")
	// ErrAborted means the caller's context was cancelled mid-stream.
	ErrAborted = errors.New("generation aborted")
)

// Stream posts the chat route with stream:true and invokes onDelta for
// every parsed piece of content/reasoning (plus one final Done delta).
// Cancelling ctx aborts the upstream request. Lenient parsing per spec §6.2:
// unknown fields ignored, malformed chunk lines skipped, a stream that
// closes without [DONE] is tolerated.
//
// Retry policy: none mid-stream; one retry on connection-refused before the
// first delta.
func (c *Client) Stream(ctx context.Context, model string, msgs []ChatMessage,
	onDelta func(Delta) error,
) error {
	return c.StreamTools(ctx, model, msgs, nil, nil, onDelta)
}

// StreamTools is Stream with tool specs advertised (spec §6.1 step 3) and
// an optional thinking fragment: the already-rendered JSON fields one
// thinking choice adds (thinking-control-specification.md §6), deep-merged
// into every request attempt. Callers render it once per generation, so a
// selection change mid-flight cannot reach this stream's continuations;
// nil means Default — no override is ever sent.
func (c *Client) StreamTools(ctx context.Context, model string, msgs []ChatMessage,
	tools []ToolSpec, thinking map[string]any, onDelta func(Delta) error,
) error {
	if model == "" {
		model = c.model
	}
	var (
		sawDelta bool
		err      error
	)
	for attempt := 0; attempt < 2; attempt++ {
		sawDelta = false
		err = c.streamOnce(ctx, model, msgs, tools, thinking, func(d Delta) error {
			if d.Content != "" || d.Reasoning != "" || len(d.ToolCalls) > 0 {
				sawDelta = true
			}
			return onDelta(d)
		})
		if err == nil {
			return nil
		}
		// Retry only connection-refused before the first token (spec §6.2).
		if sawDelta || !isConnRefused(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ErrAborted
		case <-time.After(300 * time.Millisecond):
		}
	}
	return err
}

func isConnRefused(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) &&
		(errors.Is(opErr.Err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED))
}

func (c *Client) streamOnce(ctx context.Context, model string, msgs []ChatMessage,
	tools []ToolSpec, thinking map[string]any, onDelta func(Delta) error,
) error {
	payload := map[string]any{
		"model":          model,
		"messages":       msgs,
		"stream":         true,
		"max_tokens":     c.maxTokens,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	if c.temperature > 0 {
		payload["temperature"] = c.temperature
	}
	// The thinking fragment rides the normal body — no extra_body
	// wrapper, that name belongs to SDK conventions, not the HTTP
	// request (thinking-control-specification.md §6).
	if len(thinking) > 0 {
		mergeJSON(payload, thinking)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ErrAborted
		}
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a little for the log but never surface upstream bodies.
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = peek
		return fmt.Errorf("%w: HTTP %d", ErrUpstream, resp.StatusCode)
	}

	return parseSSE(resp.Body, onDelta, ctx.Err)
}

// mergeJSON deep-merges src into dst: nested objects merge key by key so
// unrelated fields already in either tree survive; anything else replaces
// (thinking-control-specification.md §6: setting
// chat_template_kwargs.enable_thinking must not drop another template
// argument). src comes from the thinking renderer — already a fresh
// copy — and is never mutated.
func mergeJSON(dst, src map[string]any) {
	for k, v := range src {
		sm, ok := v.(map[string]any)
		if !ok {
			dst[k] = v
			continue
		}
		dm, ok := dst[k].(map[string]any)
		if !ok {
			dst[k] = v
			continue
		}
		mergeJSON(dm, sm)
	}
}

// chunk mirrors the subset of an OpenAI streaming chunk we consume; unknown
// fields are ignored by construction (forward-compat, spec §6.2).
type chunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning arrives under two aliases across providers:
			// reasoning_content (vLLM/llama.cpp/Gufo lineage) and
			// reasoning (newer Chat surfaces). When both carry the same
			// trace, only one may reach the stream — double-appending a
			// thought duplicates every word of it (thinking-control-
			// specification.md §7).
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Role             string `json:"role"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// parseSSE consumes an SSE body. readErr is consulted between lines so an
// aborted context stops the pump promptly.
func parseSSE(r io.Reader, onDelta func(Delta) error, abort func() error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // long chunks tolerated

	var (
		dataLines []string
		finished  bool
	)
	handleEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			finished = true
			return nil
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil // malformed chunk: skip, stay forward-compatible
		}
		var d Delta
		if ch.Usage != nil {
			d.Usage = int64(ch.Usage.TotalTokens)
			d.Completion = int64(ch.Usage.CompletionTokens)
		}
		for _, cch := range ch.Choices {
			d.Content = cch.Delta.Content
			// One alias per trace: reasoning_content wins when both are
			// set (the longer-established spelling), never concatenated.
			d.Reasoning = cch.Delta.ReasoningContent
			if d.Reasoning == "" {
				d.Reasoning = cch.Delta.Reasoning
			}
			for pos, tc := range cch.Delta.ToolCalls {
				idx := pos // servers may omit index on the first fragment
				if tc.Index != nil {
					idx = *tc.Index
				}
				d.ToolCalls = append(d.ToolCalls, ToolCallDelta{
					Index:     idx,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
			if cch.FinishReason != "" {
				d.Done = true
				d.Finish = cch.FinishReason
				finished = true
			}
		}
		if d.Content == "" && d.Reasoning == "" && d.Usage == 0 && d.Completion == 0 && !d.Done && len(d.ToolCalls) == 0 {
			return nil
		}
		return onDelta(d)
	}

	for sc.Scan() {
		if abort != nil {
			if err := abort(); err != nil {
				return ErrAborted
			}
		}
		line := sc.Text()
		switch {
		case line == "": // event boundary (also absorbs \r leftovers below)
			if err := handleEvent(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"): // comment / heartbeat
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line, "data:")
			dataLines = append(dataLines, strings.TrimPrefix(v, " "))
		default: // event:, id:, retry: — not needed; ignore
		}
	}
	if err := sc.Err(); err != nil {
		if abort != nil && abort() != nil {
			return ErrAborted
		}
		return fmt.Errorf("upstream stream: %w", err)
	}
	// Flush a trailing event that never got its blank line, then accept EOF
	// even without [DONE] (spec §6.2 tolerance).
	if err := handleEvent(); err != nil {
		return err
	}
	_ = finished
	return nil
}
