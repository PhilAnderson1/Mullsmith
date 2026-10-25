package agent

import (
	"strings"
	"testing"

	"gabkeep/internal/llm"
)

func msg(role, content string) llm.ChatMessage { return llm.ChatMessage{Role: role, Content: content} }

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"日本語テキスト", 2}, // runes, not bytes: 7 runes -> 2
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBuildContextKeepsSystemAndRecentSuffix(t *testing.T) {
	// Budget: 100 - 20 = 80 tokens = 320 runes. Each message is 80 runes
	// (20 tokens), so exactly the four newest survive; the oldest two drop.
	const word = "x"
	var history []llm.ChatMessage
	for i := 0; i < 6; i++ {
		history = append(history, msg([]string{"user", "assistant"}[i%2], strings.Repeat(word, 80)))
	}
	// Make each message identifiable at its head without changing length.
	for i := range history {
		history[i].Content = string(rune('A'+i)) + history[i].Content[1:]
	}

	out := BuildContext("", history, 100, 20)
	if len(out) != 4 {
		t.Fatalf("kept %d messages, want 4: %+v", len(out), out)
	}
	// Chronological order restored, oldest-first of the kept suffix.
	for i, m := range out {
		if want := string(rune('C' + i)); m.Content[0] != want[0] {
			t.Errorf("out[%d] starts %q, want %q", i, m.Content[:1], want)
		}
	}
}

func TestBuildContextSystemPromptCountsTowardBudget(t *testing.T) {
	sys := strings.Repeat("s", 160) // 40 tokens of the 80-token budget
	history := []llm.ChatMessage{
		msg("user", strings.Repeat("a", 80)),      // 20
		msg("assistant", strings.Repeat("b", 80)), // 20
		msg("user", strings.Repeat("c", 80)),      // 20  -> 40+20+20=80 fits
		msg("user", strings.Repeat("d", 80)),      // pushes to 100 -> dropped with all older
	}
	out := BuildContext(sys, history, 100, 20)
	if len(out) != 3 {
		t.Fatalf("out = %d messages, want system + 2", len(out))
	}
	if out[0].Role != "system" || out[0].Content != sys {
		t.Errorf("first message = %+v, want the system prompt", out[0])
	}
	if out[1].Content[0] != 'c' || out[2].Content[0] != 'd' {
		t.Errorf("kept wrong window: %q…", out[1].Content[:1])
	}
}

func TestBuildContextNeverDropsCurrentMessage(t *testing.T) {
	huge := strings.Repeat("z", 10_000) // far past any budget
	history := []llm.ChatMessage{msg("user", "old"), msg("assistant", "older"), msg("user", huge)}

	out := BuildContext("be nice", history, 50, 40) // budget clamps to 25 tokens
	var last *llm.ChatMessage
	for i := range out {
		if out[i].Content == huge {
			last = &out[i]
		}
	}
	if last == nil {
		t.Fatal("the current user message was dropped")
	}
	if out[len(out)-1].Content != huge {
		t.Error("current message must be last")
	}
	// System prompt survives alongside it.
	if out[0].Role != "system" {
		t.Errorf("system prompt lost: %+v", out[0])
	}
}

func TestBuildContextWholeTurnGranularity(t *testing.T) {
	// If the newest pair does not both fit, the loop stops at the first
	// miss: no split-turn partial windows from the middle of history.
	history := []llm.ChatMessage{
		msg("user", strings.Repeat("m", 40)),       // 10 tokens
		msg("assistant", strings.Repeat("m", 400)), // 100 > 60-token budget
	}
	out := BuildContext("", history, 100, 40)
	// assistant (newest) alone busts the budget; but it is NOT the current
	// user message… BuildContext keeps the last message unconditionally,
	// then walks oldest-ward: nothing else fits.
	if len(out) != 1 || out[0].Role != "assistant" {
		t.Fatalf("out = %+v, want just the newest (last) message", out)
	}
}
