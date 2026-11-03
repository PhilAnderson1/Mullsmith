package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Complete runs one small non-streaming chat completion (side quests such
// as conversation titling). maxTokens bounds the reply; ctx bounds the
// wait. thinking is the catalogue-rendered fragment for this model's
// quietest option (off, else the lightest rung — §12): when present it
// IS the thinking control, sent in place of the fallback hint below.
// With no selector configured, the fallback stays: chat_template_kwargs
// enable_thinking is the de facto llama.cpp-family convention (the very
// field the gufo/llama_cpp catalogues use for their off recipe), and
// servers that do not know it ignore it harmlessly. Endpoints that
// ignore both still work, just slower.
//
// This fallback is deliberate and NOT the chat selector: a title quest
// is not a conversation turn, never carries the user's choice, and the
// selector owns only the chat request body
// (thinking-control-specification.md §6 — one owner per control).
func (c *Client) Complete(ctx context.Context, model string, msgs []ChatMessage,
	maxTokens int, thinking map[string]any,
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
	if len(thinking) > 0 {
		// The catalogue's word outranks the fallback: one thinking
		// control on the wire, never two.
		delete(payload, "chat_template_kwargs")
		for k, v := range thinking {
			payload[k] = v
		}
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
		return "", endpointConnectionFailure(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", endpointHTTPFailure(resp.StatusCode)
	}
	var out completionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", endpointResponseFailure(errors.New("bad completion body"))
	}
	for _, ch := range out.Choices {
		if ch.Message.Content != "" {
			return ch.Message.Content, nil
		}
	}
	return "", endpointResponseFailure(errors.New("empty completion"))
}
