package agent

import (
	"context"
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
	Content    string
	Reasoning  string
	Status     string // complete | aborted | error
	Usage      int64  // upstream total_tokens, else an estimate
	Completion int64  // upstream completion_tokens, else an estimate
	Err        error  // upstream transport/protocol failure; expected stops use Note
	Note       string // durable explanation when upstream stops an incomplete reply
	// ToolCalls holds the complete calls the model proposed this round
	// (empty when it just answered), ordered by stream index.
	ToolCalls []llm.ChatToolCall
}

// Generator streams one assistant turn against the configured endpoint.
type Generator struct {
	LLM *llm.Client
}

// Round streams one model round (spec §6.1 step 3–5). Tool deltas are
// forwarded raw through toolDelta (may be nil) for live UI cards and are
// accumulated by index into Result.ToolCalls. Reasoning is accumulated but
// only emitted as a separate event kind — it is never part of the answer
// content (spec §6.2). thinking carries the rendered request fragment of
// the generation's snapshotted choice (nil = Default, no override); the
// caller renders it once, so every round of one generation — tool
// continuations included — speaks the same thinking level.
func (g *Generator) Round(ctx context.Context, model string, msgs []llm.ChatMessage,
	tools []llm.ToolSpec, thinking map[string]any,
	emit func(kind, text string) error,
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
				a = &callAccumulator{}
				acc[tc.Index] = a
				order = append(order, tc.Index)
			}
			// Canonical identity (finding 6): an endpoint that omits the call
			// id gets ONE minted here, so the live card, the completed call,
			// the audit row and the replayed protocol all carry the same id.
			// Minting downstream instead gave the delta stream and the final
			// assembly two ids — the first card never received its final
			// tool event and a fallback card appeared under the second one.
			// Absorb runs FIRST so the endpoint's own id wins when it
			// arrives before anything is published; once published, absorb
			// freezes and later ids cannot overwrite it (review2 finding 5).
			a.absorb(tc)
			if a.id == "" {
				a.id = id.New()
			}
			tc.ID = a.id
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
	// The ID freezes once published (review2 finding 5): Round mints a
	// canonical id at the first delta and every later delta, the completed
	// call, the audit row and the replayed protocol must agree on it. A
	// late fragment carrying the endpoint's own id therefore does NOT
	// replace it — live cards keyed by the published identity would never
	// receive their argument and end events.
	if a.id == "" && d.ID != "" {
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
