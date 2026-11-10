package chat

import "mullsmith/internal/tools"

// Event is a normalized application event. Only the HTTP adapter turns it
// into an SSE frame; consumers can inspect its concrete payload type.
type Event interface{ Kind() string }
type Emit func(Event) error
type sink struct{ emit Emit }

func (s sink) Send(event Event) error { return s.emit(event) }

type RoundStarted struct {
	Initial        bool   `json:"-"`
	GenerationID   string `json:"generation_id,omitempty"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	Round          int    `json:"round"`
}

func (e RoundStarted) Kind() string {
	if e.Initial {
		return "start"
	}
	return "round"
}

type TextDelta struct {
	Reasoning bool   `json:"-"`
	Text      string `json:"t"`
}

func (e TextDelta) Kind() string {
	if e.Reasoning {
		return "reasoning"
	}
	return "token"
}

type ToolStarted struct {
	CallID     string `json:"call_id"`
	MessageID  string `json:"message_id"`
	Tool       string `json:"tool"`
	Arguments  string `json:"arguments"`
	StatusOnly bool   `json:"status_only,omitempty"`
}

func (ToolStarted) Kind() string { return "tool_start" }

type ToolFinished struct {
	CallID    string `json:"call_id"`
	MessageID string `json:"message_id"`
	Tool      string `json:"tool"`
	Status    string `json:"status"`
	tools.Card
}

func (ToolFinished) Kind() string { return "tool_end" }

type ApprovalRequested struct {
	ApprovalID     string `json:"approval_id"`
	CallID         string `json:"call_id"`
	MessageID      string `json:"message_id"`
	Tool           string `json:"tool"`
	Arguments      string `json:"arguments"`
	Rememberable   bool   `json:"rememberable"`
	RememberServer string `json:"remember_server,omitempty"`
}

func (ApprovalRequested) Kind() string { return "awaiting_approval" }

type TitleChanged struct {
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
}

func (TitleChanged) Kind() string { return "title" }

type Failed struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func (Failed) Kind() string { return "error" }

type Finished struct {
	GenerationID string         `json:"generation_id"`
	Status       string         `json:"status"`
	Rounds       []RoundOutcome `json:"rounds"`
	Usage        int64          `json:"usage_tokens"`
}

func (Finished) Kind() string { return "done" }
