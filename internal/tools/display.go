package tools

import (
	"encoding/json"
)

// DisplayBudget bounds one card payload (tool_calls.display, C7). The card
// and the model-facing `result` are separate concerns: `result` is
// middle-elided because the MODEL has a context window, a card is elided by
// DROP — never by cutting a JSON structure in half, which is exactly how
// links and download cards used to disappear on reload.
const DisplayBudget = 8 * 1024

// Card is the one stored-and-sent shape of a tool card's payload:
//
//	{"summary": "...", "data": ...}
//
// The SAME clamped values go into the live tool_end event and into
// tool_calls.display, which is what makes the two views agree by
// construction rather than by two implementations happening to match.
type Card struct {
	Summary   string `json:"summary"`
	Data      any    `json:"data"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ClampCard bounds a card's payload: over budget the structured data is
// dropped and flagged, keeping the summary; an absurdly long summary is then
// rune-elided to fit. Anything that cannot be marshalled degrades to the
// summary alone — a card with words beats a card with broken JSON.
func ClampCard(summary string, data any) Card {
	if b, err := json.Marshal(Card{Summary: summary, Data: data}); err == nil && len(b) <= DisplayBudget {
		return Card{Summary: summary, Data: data}
	}
	c := Card{Summary: summary, Truncated: true}
	if b, err := json.Marshal(c); err == nil && len(b) <= DisplayBudget {
		return c
	}
	// Only a giant summary can land here. Trim by the OVERSHOOT in bytes:
	// each pass removes at least that many runes, so multibyte text
	// converges in two or three passes instead of one rune at a time.
	r := []rune(c.Summary)
	for {
		c.Summary = string(r) + "…"
		b, err := json.Marshal(c)
		if err != nil {
			return Card{}
		}
		if len(b) <= DisplayBudget || len(r) == 0 {
			return c
		}
		over := len(b) - DisplayBudget
		if over > len(r) {
			over = len(r)
		}
		r = r[:len(r)-over]
	}
}

// JSON renders the stored form of an already-clamped card. Clamping happens
// exactly once, in ClampCard: re-clamping here would "fit" a dropped-payload
// card as a fresh one and quietly lose the truncated flag.
func (c Card) JSON() string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}
