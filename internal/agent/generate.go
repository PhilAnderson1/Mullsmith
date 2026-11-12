package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"mullsmith/internal/id"
	"mullsmith/internal/llm"
)

// Event kinds handed to the SSE layer (spec §6.1 taxonomy; tool_* events
// arrive in M4).
const (
	EventToken     = "token"
	EventReasoning = "reasoning"
)

// ErrClientGone is returned by the emit callback when the browser
// disconnects; Round maps it to status "aborted".
var ErrClientGone = errors.New("client disconnected")

// Result carries the final state of a generation for persistence.
type Result struct {
	Content          string
	Reasoning        string
	ReasoningDetails []json.RawMessage
	Status           string // complete | aborted | error
	Usage            int64  // upstream total_tokens, else an estimate
	Completion       int64  // upstream completion_tokens, else an estimate
	Err              error  // upstream transport/protocol failure; expected stops use Note
	Note             string // durable explanation when upstream stops an incomplete reply
	// ToolCalls holds the complete calls the model proposed this round
	// (empty when it just answered), ordered by stream index.
	ToolCalls []llm.ChatToolCall
}

// Generator streams one assistant turn against the configured endpoint.
type Generator struct {
	LLM *llm.Client
}

// Round streams one model round (spec §6.1 step 3–5). Tool deltas are
// accumulated privately by index into complete Result.ToolCalls. Endpoint
// fragmentation is deliberately contained here: consumers only see calls
// after the model has finished the round. Reasoning is accumulated but
// only emitted as a separate event kind — it is never part of the answer
// content (spec §6.2). thinking carries the rendered request fragment of
// the generation's snapshotted choice (nil = Default, no override); the
// caller renders it once, so every round of one generation — tool
// continuations included — speaks the same thinking level.
func (g *Generator) Round(ctx context.Context, model string, msgs []llm.ChatMessage,
	tools []llm.ToolSpec, thinking map[string]any,
	emit func(kind, text string) error,
) Result {
	var (
		content          strings.Builder
		reasoning        strings.Builder
		usage            int64
		completion       int64
		reasoningDetails []json.RawMessage
		finish           string
	)
	acc := map[int]*callAccumulator{}
	var order []int

	err := g.LLM.StreamTools(ctx, model, msgs, tools, thinking, func(d llm.Delta) error {
		if d.Finish != "" {
			finish = d.Finish
		}
		if d.Usage > 0 {
			usage = d.Usage
		}
		if d.Completion > 0 {
			completion = d.Completion
		}
		for _, tc := range d.ToolCalls {
			a := acc[tc.Index]
			if a == nil {
				a = &callAccumulator{id: id.New()}
				acc[tc.Index] = a
				order = append(order, tc.Index)
			}
			// Own the identity: endpoint IDs can repeat across calls or rounds.
			// This same ID names persisted calls, UI events and protocol replay.
			a.absorb(tc)
		}
		if d.Reasoning != "" {
			reasoning.WriteString(d.Reasoning)
			if err := emit(EventReasoning, d.Reasoning); err != nil {
				return err
			}
		}
		reasoningDetails = append(reasoningDetails, d.ReasoningDetails...)
		if d.Content != "" {
			content.WriteString(d.Content)
			if err := emit(EventToken, d.Content); err != nil {
				return err
			}
		}
		return nil
	})

	res := Result{
		Content:          content.String(),
		Reasoning:        reasoning.String(),
		ReasoningDetails: reasoningDetails,
		Usage:            usage,
		Completion:       completion,
	}
	sort.Ints(order) // fragments may arrive out of index order; calls follow it
	for _, idx := range order {
		if call, ok := acc[idx].result(); ok {
			res.ToolCalls = append(res.ToolCalls, call)
		}
	}
	switch {
	case err == nil:
		res.Status = "complete"
		switch finish {
		case "", "stop", "tool_calls":
			// Some endpoints omit finish_reason; retain the streamed answer.
		case "length":
			res.Status = "error"
			res.Note = "The model reached its output limit before finishing this reply."
		case "content_filter":
			res.Status = "error"
			res.Note = "The model stopped this reply because of its content filter."
		default:
			res.Status = "error"
			res.Note = "The model stopped this reply for an unexpected reason."
		}
	case errors.Is(err, ErrClientGone), errors.Is(err, llm.ErrAborted), ctx.Err() != nil:
		res.Status = "aborted"
	default:
		res.Status = "error"
		res.Err = err
	}
	completionEstimate := EstimateTokens(res.Content) + EstimateTokens(res.Reasoning)
	// Structured reasoning normally replaces the plain reasoning field on
	// replay. Some endpoints emit both views of the same trace, so count the
	// structured form only when no plain text was supplied rather than
	// double-counting one thought.
	if res.Reasoning == "" && len(res.ReasoningDetails) > 0 {
		if b, err := json.Marshal(res.ReasoningDetails); err == nil {
			completionEstimate += EstimateTokens(string(b))
		}
	}
	for _, tc := range res.ToolCalls {
		completionEstimate += EstimateTokens(tc.Function.Name + tc.Function.Arguments)
	}
	if res.Usage == 0 {
		completionForTotal := int64(completionEstimate)
		if res.Completion > 0 {
			completionForTotal = res.Completion
		}
		res.Usage = EstimateRequestTokens(msgs, tools) + completionForTotal
	}
	if res.Completion == 0 {
		// Generated output = answer + reasoning + any tool proposal.
		res.Completion = int64(completionEstimate)
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
