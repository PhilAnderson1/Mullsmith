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
	// Dropping stops at turn boundaries: when the newest turn does not fit
	// alongside older ones, nothing from the middle of history leaks in —
	// but the trailing turn itself (user + its tool furniture) is kept
	// whole, even over budget, because a request without the current user
	// message is meaningless and split tool groups break the protocol.
	history := []llm.ChatMessage{
		msg("user", strings.Repeat("m", 40)),       // 10 tokens
		msg("assistant", strings.Repeat("m", 400)), // 100 > 60-token budget
	}
	out := BuildContext("", history, 100, 40)
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "assistant" {
		t.Fatalf("out = %+v, want the whole trailing turn (user + assistant)", out)
	}
}

func TestBuildContextPricesInlineImages(t *testing.T) {
	// Three plain user turns of 200 runes (50 tokens) each, and a newest
	// turn with 80 runes (20 tokens) plus one inline image. Budget 1100:
	// the image turn costs 1024+20 — an older 50-token turn still fits,
	// the one before it does not. If images were free, everything would
	// fit and nothing would drop, so the eviction below pins the pricing.
	pad := strings.Repeat("y", 200)
	pic := llm.ChatMessage{Role: "user", Content: strings.Repeat("x", 80),
		Images: []string{"data:image/png;base64,AAAA"}}
	history := []llm.ChatMessage{
		{Role: "user", Content: "A" + pad[1:]},
		{Role: "user", Content: "B" + pad[1:]},
		{Role: "user", Content: "C" + pad[1:]},
		pic,
	}
	out := BuildContext("", history, 1150, 50) // budget 1100
	if len(out) != 2 {
		t.Fatalf("kept %d messages, want 2 (A and B dropped): %+v", len(out), out)
	}
	if out[0].Content[0] != 'C' {
		t.Errorf("first kept = %q, want the turn starting 'C'", out[0].Content[:1])
	}
	if len(out[1].Images) != 1 {
		t.Errorf("image turn must survive with its picture, got %+v", out[1])
	}

	// The newest user message is never dropped even when its images alone
	// blow the budget — it rides through with its pictures intact.
	four := llm.ChatMessage{Role: "user", Content: "all",
		Images: []string{"1", "2", "3", "4"}} // 4×1024 > 200 budget
	out = BuildContext("", []llm.ChatMessage{
		{Role: "user", Content: "older"}, four,
	}, 250, 50)
	if len(out) != 1 || len(out[0].Images) != 4 {
		t.Fatalf("rescue = %+v, want only the image-heavy newest user turn", out)
	}
}
