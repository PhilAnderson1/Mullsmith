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
		{"日本語テキスト", 2}, // runes, not bytes: 7 runes -> (7+3)/4 = 2
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBuildContextCarriesWholeHistory(t *testing.T) {
	// No trimming anymore: system prompt first, then every message,
	// oldest first, untouched.
	var history []llm.ChatMessage
	for i := 0; i < 6; i++ {
		history = append(history, msg([]string{"user", "assistant"}[i%2], strings.Repeat("x", 80)))
	}
	out := BuildContext("be nice", history)
	if len(out) != 7 {
		t.Fatalf("out = %d messages, want system + all 6", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "be nice" {
		t.Errorf("first message = %+v, want the system prompt", out[0])
	}
	for i := 1; i < len(out); i++ {
		if out[i].Role != history[i-1].Role {
			t.Errorf("out[%d].Role = %s, want %s (order must be preserved)", i, out[i].Role, history[i-1].Role)
		}
	}
}

func TestBuildContextEmptyHistory(t *testing.T) {
	out := BuildContext("", nil)
	if len(out) != 0 {
		t.Fatalf("out = %+v, want nothing", out)
	}
}

func TestToolGuidanceUploadsPngs(t *testing.T) {
	// The model must know fs_delete reaches uploads/ files (so it can offer
	// to clean up attachments) — and that those calls ask the user first.
	if !strings.Contains(ToolGuidance, "uploads/") || !strings.Contains(ToolGuidance, "confirmation") {
		t.Errorf("ToolGuidance must state the fs_delete uploads right and its confirmation gate:\n%s", ToolGuidance)
	}
}
