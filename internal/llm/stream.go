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
	"sync/atomic"
	"syscall"
	"time"

	"mullsmith/internal/config"
)

// ChatMessage is one message in a chat/completions request. The tool fields
// carry the OpenAI tool protocol (spec §6.1 step 5): assistant rows replay
// their ToolCalls, and role "tool" rows answer one call via ToolCallID.
// Images carries inline pictures as data: URLs (the model entry's
// vision flag, §7.2): when non-empty,
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
// shown the array shape: ChatMessages only get Images when the running
// model's entry spells vision: true (§2.2).
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
	// Progress budgets (C8), off when 0: headerTimeout bounds the wait
	// for response headers, stallTimeout the silence between body bytes.
	// Never total stream timeouts — a running stream is only cut when it
	// stops moving.
	headerTimeout time.Duration
	stallTimeout  time.Duration
}

// Default upstream progress budgets (C8), used when a hand-built config
// leaves the knobs unset; config.setDefaults spells the same values out
// for loaded files.
const (
	DefaultHeaderTimeout = 90 * time.Second
	DefaultStallTimeout  = 120 * time.Second
)

// budgetFor maps the knob onto a runtime duration: absent (0) takes the
// code default, any negative is the house off-sentinel, positive stands.
func budgetFor(d, def time.Duration) time.Duration {
	switch {
	case d == 0:
		return def
	case d < 0:
		return 0 // off
	default:
		return d
	}
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
		model:       cfg.LLM.DefaultModelID(),
		maxTokens:   cfg.LLM.MaxTokensResponse,
		temperature: cfg.LLM.DefaultTemperature,
		http:        &http.Client{Transport: transport}, // no global timeout: streaming
		// The C8 budgets bind inside streamOnce, not on the transport:
		// an injected test transport gets them exactly like the real one.
		headerTimeout: budgetFor(cfg.LLM.UpstreamHeaderTimeout.D(), DefaultHeaderTimeout),
		stallTimeout:  budgetFor(cfg.LLM.UpstreamStallTimeout.D(), DefaultStallTimeout),
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
	// ErrStreamHeaderTimeout and ErrStreamStalled are the C8 progress
	// budgets tripping: headers that never arrived, and a stream that went
	// silent. Both wrap ErrUpstream, so the generation treats them like any
	// other endpoint failure — partial output stays durable, the terminal
	// outcome is upstream_error, the slot is released.
	ErrStreamHeaderTimeout = fmt.Errorf("%w: response headers never arrived", ErrUpstream)
	ErrStreamStalled       = fmt.Errorf("%w: the stream stopped sending data", ErrUpstream)
)

// StreamTools streams the chat route with stream:true and invokes onDelta
// for every parsed piece of content/reasoning (plus one final Done delta).
// Cancelling ctx aborts the upstream request. Lenient parsing per spec §6.2:
// unknown fields ignored, malformed chunk lines skipped, a stream that
// closes without [DONE] is tolerated.
//
// Retry policy: none mid-stream; one retry on connection-refused before the
// first delta. Tool specs are advertised (spec §6.1 step 3) along with an
// optional thinking fragment: the already-rendered JSON fields one
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
	// The C8 watchdog runs this attempt on a child context: one
	// cancellation path for both budgets, with the trip reason read back
	// on every exit below. A cancellation the CALLER issued stays
	// ErrAborted — the budgets only ever add their own verdicts.
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	w := newWatchdog(attemptCtx, cancelAttempt, c.headerTimeout, c.stallTimeout)

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.url(),
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
		if w.tripped.Load() == tripHeader {
			return ErrStreamHeaderTimeout
		}
		if ctx.Err() != nil {
			return ErrAborted
		}
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	w.startBody() // headers are here: the stall budget measures silence from now on
	if resp.StatusCode != http.StatusOK {
		// Drain a little for the log but never surface upstream bodies.
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = peek
		return fmt.Errorf("%w: HTTP %d", ErrUpstream, resp.StatusCode)
	}

	err = parseSSE(&touchReader{r: resp.Body, w: w}, onDelta, attemptCtx.Err)
	if err != nil {
		switch w.tripped.Load() {
		case tripHeader:
			return ErrStreamHeaderTimeout
		case tripStall:
			return ErrStreamStalled
		}
		if ctx.Err() != nil {
			return ErrAborted
		}
		return err
	}
	return nil
}

// Trip reasons for the watchdog (0 = it never fired).
const (
	tripNone   int32 = 0
	tripHeader int32 = 1
	tripStall  int32 = 2
)

// watchdog enforces one attempt's C8 progress budgets. It starts in the
// header phase; startBody() switches to stall timing where every byte
// counts as transport progress — SSE comment heartbeats included, per the
// documented policy (the budget measures whether the ENDPOINT is talking,
// not whether it has model content to show). When a budget expires it
// records the trip, cancels the attempt, and retires; the streamOnce
// classification turns that into a typed error.
type watchdog struct {
	last    atomic.Int64 // unix nanos of the last transport progress
	phase   atomic.Int32 // 0 = awaiting headers, 1 = streaming body
	tripped atomic.Int32 // tripNone | tripHeader | tripStall
	cancel  context.CancelFunc
	header  time.Duration // 0 = off
	stall   time.Duration // 0 = off
}

func newWatchdog(ctx context.Context, cancel context.CancelFunc, header, stall time.Duration) *watchdog {
	w := &watchdog{cancel: cancel, header: header, stall: stall}
	w.last.Store(time.Now().UnixNano())
	go w.run(ctx)
	return w
}

func (w *watchdog) run(ctx context.Context) {
	// Poll quarterly on the tighter budget so a trip lands clearly inside
	// it, with bounds for absurd configurations.
	interval := 250 * time.Millisecond
	if w.stall > 0 && w.stall/4 < interval {
		interval = w.stall / 4
	}
	if w.header > 0 && w.header/4 < interval {
		interval = w.header / 4
	}
	if interval < 20*time.Millisecond {
		interval = 20 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done(): // attempt over (success, failure or abort)
			return
		case <-t.C:
		}
		idle := time.Since(time.Unix(0, w.last.Load()))
		if w.phase.Load() == 0 && w.header > 0 && idle >= w.header {
			w.tripped.Store(tripHeader)
			w.cancel()
			return
		}
		if w.phase.Load() == 1 && w.stall > 0 && idle >= w.stall {
			w.tripped.Store(tripStall)
			w.cancel()
			return
		}
	}
}

// touch records one transport progress event (called per read that moved).
func (w *watchdog) touch() { w.last.Store(time.Now().UnixNano()) }

// startBody marks headers received and restarts the idle clock.
func (w *watchdog) startBody() {
	w.phase.Store(1)
	w.touch()
}

// touchReader feeds the watchdog: every byte that reaches the parser is
// transport progress (C8 policy — silence trips the budget, content type
// does not).
type touchReader struct {
	r io.Reader
	w *watchdog
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.w.touch()
	}
	return n, err
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
