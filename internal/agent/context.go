// Package agent orchestrates one chat turn: context assembly within the
// token budget, then the streamed generation (spec §6.1). The tool loop of
// §6.1 step 5 extends Generate in M4.
package agent

import (
	"unicode/utf8"

	"gabkeep/internal/llm"
)

// EstimateTokens approximates token counts for budgeting: plain text is
// roughly 4 runes/token, and over/under-counting only shifts how much
// history we keep.
func EstimateTokens(s string) int {
	return (utf8.RuneCountInString(s) + 3) / 4
}

// BuildContext assembles the request per spec §6.1 step 2: conversation
// system prompt first, then the most recent whole turns that fit
// max_context_tokens minus max_tokens_response headroom; oldest dropped
// first. history must be chronological and excludes the streaming
// placeholder message.
func BuildContext(systemPrompt string, history []llm.ChatMessage, maxContext, maxResponse int) []llm.ChatMessage {
	budget := maxContext - maxResponse
	if budget <= 0 {
		// Pathological config: keep something rather than nothing.
		budget = maxContext / 2
		if budget < 1024 {
			budget = 1024
		}
	}

	out := make([]llm.ChatMessage, 0, len(history)+1)
	used := 0
	if systemPrompt != "" {
		out = append(out, llm.ChatMessage{Role: "system", Content: systemPrompt})
		used += EstimateTokens(systemPrompt)
	}

	kept := make([]llm.ChatMessage, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		t := EstimateTokens(history[i].Content)
		if used+t > budget {
			break // whole-turn granularity: drop this and everything older
		}
		used += t
		kept = append(kept, history[i])
	}
	// Restore chronological order.
	for i := len(kept) - 1; i >= 0; i-- {
		out = append(out, kept[i])
	}
	// The current user message must never be dropped, even if it alone
	// exceeds the budget — a turn without it makes no sense.
	if len(kept) == 0 && len(history) > 0 {
		out = append(out, history[len(history)-1])
	}
	return out
}
