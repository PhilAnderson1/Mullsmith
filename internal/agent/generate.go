package agent

import (
	"context"
	"errors"
	"sort"
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
	Content    string
	Reasoning  string
	Status     string // complete | aborted | error
	Usage      int64  // upstream total_tokens, else an estimate
	Completion int64  // upstream completion_tokens, else an estimate
	Err        error  // non-nil only when Status == "error"
	// ToolCalls holds the complete calls the model proposed this round
	// (empty when it just answered), ordered by stream index.
	ToolCalls []llm.ChatToolCall
	Finish    string // upstream finish_reason ("stop", "tool_calls", …)
}

// Generator streams one assistant turn against the configured endpoint.
type Generator struct {
	Cfg *config.Config
	LLM *llm.Client
}

// Generate runs one round without tools (kept for callers that predate M4).
func (g *Generator) Generate(ctx context.Context, model string,
	msgs []llm.ChatMessage, emit func(kind, text string) error,
) Result {
	return g.Round(ctx, model, msgs, nil, emit, nil)
}

// Round streams one model round (spec §6.1 step 3–5). Tool deltas are
// forwarded raw through toolDelta (may be nil) for live UI cards and are
// accumulated by index into Result.ToolCalls. Reasoning is accumulated but
// only emitted as a separate event kind — it is never part of the answer
// content (spec §6.2).
func (g *Generator) Round(ctx context.Context, model string, msgs []llm.ChatMessage,
	tools []llm.ToolSpec, emit func(kind, text string) error,
	toolDelta func(callIndex int, d llm.ToolCallDelta),
) Result {
	var (
		content    strings.Builder
		reasoning  strings.Builder
		usage      int64
		completion int64
		finish     string
	)
	acc := map[int]*callAccumulator{}
	var order []int

	err := g.LLM.StreamTools(ctx, model, msgs, tools, func(d llm.Delta) error {
		if d.Usage > 0 {
			usage = d.Usage
		}
		if d.Completion > 0 {
			completion = d.Completion
		}
		if d.Finish != "" {
			finish = d.Finish
		}
		for _, tc := range d.ToolCalls {
			a := acc[tc.Index]
			if a == nil {
				a = &callAccumulator{}
				acc[tc.Index] = a
				order = append(order, tc.Index)
			}
			a.absorb(tc)
			if toolDelta != nil {
				toolDelta(tc.Index, tc)
			}
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

	res := Result{
		Content:    content.String(),
		Reasoning:  reasoning.String(),
		Usage:      usage,
		Completion: completion,
		Finish:     finish,
	}
	sort.Ints(order) // fragments may arrive out of index order; calls follow it
	for _, idx := range order {
		if call, ok := acc[idx].result(); ok {
			res.ToolCalls = append(res.ToolCalls, call)
		}
	}
	if len(res.ToolCalls) > 0 && res.Finish == "" {
		// Some endpoints omit an explicit tool_calls finish; the presence
		// of a complete call implies it.
		res.Finish = "tool_calls"
	}
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
	if res.Completion == 0 {
		// Generated output = answer + reasoning; the same estimate the
		// total falls back to (an endpoint may report only one or neither).
		res.Completion = int64(EstimateTokens(res.Content) + EstimateTokens(res.Reasoning))
	}
	return res
}

// callAccumulator stitches streamed ToolCallDelta fragments (shared index)
// back into one complete call.
type callAccumulator struct {
	id   string
	name string
	args strings.Builder
}

func (a *callAccumulator) absorb(d llm.ToolCallDelta) {
	if d.ID != "" {
		a.id = d.ID
	}
	if d.Name != "" {
		a.name = d.Name
	}
	a.args.WriteString(d.Arguments)
}

func (a *callAccumulator) result() (llm.ChatToolCall, bool) {
	if a.name == "" {
		return llm.ChatToolCall{}, false // nameless garbage: drop the call
	}
	return llm.ChatToolCall{
		ID:       a.id,
		Type:     "function",
		Function: llm.ChatToolCallFunc{Name: a.name, Arguments: a.args.String()},
	}, true
}
