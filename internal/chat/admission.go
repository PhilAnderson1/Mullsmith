package chat

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
)

func RequestTokens(msgs []llm.ChatMessage, specs []llm.ToolSpec) int64 {
	var used int64
	for _, m := range msgs {
		used += int64(agent.EstimateTokens(m.Content))
		for _, tc := range m.ToolCalls {
			used += int64(agent.EstimateTokens(tc.Function.Name + tc.Function.Arguments))
		}
	}
	if len(specs) > 0 {
		if b, err := json.Marshal(specs); err == nil {
			used += int64(agent.EstimateTokens(string(b)))
		}
	}
	return used
}

// admission is the typed verdict of the send gate (C1). A successful
// admission carries the exact history the accepted request is built from —
// the rows read under the reservation plus the turn about to land — so the
// generation never re-reads, and a nil slice can never double as an
// error indicator: failures return (nil, err), refusals return a verdict
// with full=true and a reason.
type Admission struct {
	History []store.Message // valid only when !full
	Full    bool
	Reason  string // "context_full" | "history_limit" when full
}

// admitSend answers "would the next request overflow?" — measured, not
// approximated (finding 7): the gate builds the SAME request the send would
// make (buildRequest: system prompt, guidance, tool schemas, manifests)
// plus the turn about to land (extra/attPaths; empty on regenerate), and
// compares the estimate with the window minus the output reserve.
// excludeIDs drop the rows a regeneration retires — counting them would
// refuse a replacement whose request excludes it.
//
// The read runs on the CALLER's context — for a real send that is the
// generation's own reservation context (C1): the snapshot belongs to the
// reserved operation, so no other generation can complete between admission
// and acceptance, and no accepted send omits a completed predecessor.
//
// Still honest-by-estimate: agent.EstimateTokens is runes/4, so a send may
// cross the real line a little early or late. The refusal reason is
// "context_full", or "history_limit" when the conversation has outgrown
// context replay itself (finding 2: a past-cap history is an explicit
// refusal, never a silently-truncated success; the row cap binds even when
// the window is unknown, and the incoming turn counts toward it — review2
// finding 1). A failed store read is a typed error, not an open gate with a
// nil history: the caller refuses without persisting anything.
func (s *Service) AdmitSend(ctx context.Context, conv *store.Conversation,
	userID, extra string, attPaths []string, excludeIDs []string,
) (*Admission, error) {
	_, known := s.overflowLine(s.Model(conv.Model))
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	hist, err := s.store.ContextHistory(ctx, conv.ID, excludeIDs...)
	if errors.Is(err, store.ErrHistoryLimit) {
		return &Admission{Full: true, Reason: "history_limit"}, nil
	}
	if err != nil {
		return nil, err
	}
	// The row cap binds EVEN WITHOUT a discovered window (review2
	// finding 1): it limits context REPLAY, not token cost, so the
	// unknown-window case cannot skip it. The refusal point is where the
	// NEXT ContextHistory read would fail — persisted rows (hist, already
	// excluding the regenerated reply) PLUS the turn about to land
	// (send: one row; regenerate: none, the placeholder replaces the
	// retired one) past the cap. len+incoming > cap, not >=: a history
	// AT the cap is still readable, and a send that lands exactly ON it
	// is the last one the cap allows rather than one refused early.
	incoming := 0
	if extra != "" || len(attPaths) > 0 {
		incoming = 1
	}
	if len(hist)+incoming > store.ContextHistoryRows() {
		return &Admission{Full: true, Reason: "history_limit"}, nil
	}
	// The turn about to land rides the history the generation builds its
	// prompt from — one read for decision AND request.
	if incoming > 0 {
		turn := store.Message{Role: "user", Content: extra}
		for _, p := range attPaths {
			turn.Attachments = append(turn.Attachments, store.Attachment{Path: p})
		}
		hist = append(hist, turn)
	}
	if !known {
		// Open token gate (window unknown: the endpoint arbitrates),
		// but the row-cap verdict above stands.
		return &Admission{History: hist}, nil
	}
	msgs, specs := s.BuildRequest(conv, userID, hist)
	line, _ := s.overflowLine(s.Model(conv.Model))
	if RequestTokens(msgs, specs) >= line {
		return &Admission{Full: true, Reason: "context_full"}, nil
	}
	return &Admission{History: hist}, nil
}

// continuationFull re-measures a growing tool continuation against the same
// overflow line admission applied (finding 7): by round N the transcript
// carries every result so far. Tool definitions remain advertised when the
// execution limit is reached; refusal feedback lets the model choose its reply.
//
// The line already subtracts the FULL output reserve, so a continuation
// under it always leaves the request's max_tokens affordable — one figure,
// gate and wire agree (review2 finding 6's consistency requirement). When
// the reserve no longer fits the window at all, the line is at or below
// zero and every send — first or continuation — is refused with
// context_full instead of the reserve being silently dropped.
func (s *Service) continuationFull(conv *store.Conversation, msgs []llm.ChatMessage, roundTools []llm.ToolSpec) bool {
	line, known := s.overflowLine(s.Model(conv.Model))
	return known && RequestTokens(msgs, roundTools) >= line
}
