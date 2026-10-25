package agent

import (
	"context"
	"errors"
	"strings"

	"gabkeep/internal/config"
	"gabkeep/internal/llm"
)

// Event kinds handed to the SSE layer (spec §6.1 taxonomy; tool_* events
// arrive in M4).
const (
	EventToken     = "token"
	EventReasoning = "reasoning"
)

// ErrClientGone is returned by the emit callback when the browser
// disconnects; Generate maps it to status "aborted".
var ErrClientGone = errors.New("client disconnected")

// Result carries the final state of a generation for persistence.
type Result struct {
	Content   string
	Reasoning string
	Status    string // complete | aborted | error
	Usage     int64  // upstream total_tokens, else an estimate
	Err       error  // non-nil only when Status == "error"
}

// Generator streams one assistant turn against the configured endpoint.
type Generator struct {
	Cfg *config.Config
	LLM *llm.Client
}

// Generate runs the stream and forwards content/reasoning deltas through
// emit (which owns the SSE writing). Reasoning is accumulated but only
// emitted as a separate event kind — it is never part of the answer
// content (spec §6.2).
func (g *Generator) Generate(ctx context.Context, model string,
	msgs []llm.ChatMessage, emit func(kind, text string) error,
) Result {
	var (
		content   strings.Builder
		reasoning strings.Builder
		usage     int64
	)

	err := g.LLM.Stream(ctx, model, msgs, func(d llm.Delta) error {
		if d.Usage > 0 {
			usage = d.Usage
		}
		if d.Reasoning != "" {
			reasoning.WriteString(d.Reasoning)
			if err := emit(EventReasoning, d.Reasoning); err != nil {
				return err
			}
		}
		if d.Content != "" {
			content.WriteString(d.Content)
			if err := emit(EventToken, d.Content); err != nil {
				return err
			}
		}
		return nil
	})

	res := Result{Content: content.String(), Reasoning: reasoning.String(), Usage: usage}
	switch {
	case err == nil:
		res.Status = "complete"
	case errors.Is(err, ErrClientGone), errors.Is(err, llm.ErrAborted), ctx.Err() != nil:
		res.Status = "aborted"
	default:
		res.Status = "error"
		res.Err = err
	}
	if res.Usage == 0 {
		res.Usage = int64(EstimateTokens(res.Content) + EstimateTokens(res.Reasoning))
	}
	return res
}
