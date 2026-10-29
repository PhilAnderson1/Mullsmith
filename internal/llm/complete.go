package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// completionResponse is the subset of a non-streaming chat completion we read.
type completionResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete runs one small non-streaming chat completion (side quests such as
// conversation titling). Thinking is disabled via chat_template_kwargs so
// reasoning models answer promptly; endpoints that ignore the kwarg still
// work, just slower. maxTokens bounds the reply; ctx bounds the wait.
//
// This hardcoded disable is deliberate and NOT the thinking selector: a
// title quest is not a conversation turn, never carries a choice, and the
// selector owns only the chat request body (thinking-control-specification.md
// §6 — one owner per control).
func (c *Client) Complete(ctx context.Context, model string, msgs []ChatMessage,
	maxTokens int,
) (string, error) {
	if model == "" {
		model = c.model
	}
	payload := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"chat_template_kwargs": map[string]any{
			"enable_thinking": false,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(),
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ErrAborted
		}
		return "", fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: HTTP %d", ErrUpstream, resp.StatusCode)
	}
	var out completionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: bad completion body", ErrUpstream)
	}
	for _, ch := range out.Choices {
		if ch.Message.Content != "" {
			return ch.Message.Content, nil
		}
	}
	return "", fmt.Errorf("%w: empty completion", ErrUpstream)
}
