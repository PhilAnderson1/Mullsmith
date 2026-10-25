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

// ChatMessage is one message in a chat/completions request. Only the plain
// text form is needed before the tool loop lands in M4.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Delta is one parsed streaming update.
type Delta struct {
	Content   string // answer text
	Reasoning string // chain-of-thought, never part of Content (spec §6.2)
	Done      bool   // finish_reason seen
	Usage     int64  // total tokens if the upstream reported one
}

// Client talks to one OpenAI-compatible endpoint (spec §6.2).
type Client struct {
	endpoint    string
	apiKey      string
	model       string
	maxTokens   int
	temperature float64
	http        *http.Client
}

// NewClient builds the streaming client. Transport may be nil for defaults
// (tests inject a fake one to exercise the retry path).
func NewClient(cfg *config.Config, transport http.RoundTripper) *Client {
	return &Client{
		endpoint:    strings.TrimRight(cfg.LLM.Endpoint, "/"),
		apiKey:      cfg.LLM.APIKey,
		model:       cfg.LLM.Model,
		maxTokens:   cfg.LLM.MaxTokensResponse,
		temperature: cfg.LLM.DefaultTemperature,
		http:        &http.Client{Transport: transport}, // no global timeout: streaming
	}
}

var (
	// ErrUpstream is a sanitized upstream failure (spec §9.12: no internals
	// in client-visible messages; the detail goes to the server log only).
	ErrUpstream = errors.New("llm upstream error")
	// ErrAborted means the caller's context was cancelled mid-stream.
	ErrAborted = errors.New("generation aborted")
)

// Stream posts /chat/completions with stream:true and invokes onDelta for
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
	if model == "" {
		model = c.model
	}
	var (
		sawDelta bool
		err      error
	)
	for attempt := 0; attempt < 2; attempt++ {
		sawDelta = false
		err = c.streamOnce(ctx, model, msgs, func(d Delta) error {
			if d.Content != "" || d.Reasoning != "" {
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
	onDelta func(Delta) error,
) error {
	payload := map[string]any{
		"model":          model,
		"messages":       msgs,
		"stream":         true,
		"max_tokens":     c.maxTokens,
		"stream_options": map[string]any{"include_usage": true},
	}
	if c.temperature > 0 {
		payload["temperature"] = c.temperature
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/chat/completions",
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

// chunk mirrors the subset of an OpenAI streaming chunk we consume; unknown
// fields are ignored by construction (forward-compat, spec §6.2).
type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Role             string `json:"role"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		TotalTokens int `json:"total_tokens"`
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
		}
		for _, cch := range ch.Choices {
			d.Content = cch.Delta.Content
			d.Reasoning = cch.Delta.ReasoningContent
			if cch.FinishReason != "" {
				d.Done = true
				finished = true
			}
		}
		if d.Content == "" && d.Reasoning == "" && d.Usage == 0 && !d.Done {
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
