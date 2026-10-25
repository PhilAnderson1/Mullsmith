package agent

import (
	"strings"
	"testing"
)

func TestSanitizeTitle(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"plain", "Rust async patterns", "Rust async patterns"},
		{"quotes", `"Garden Planning"`, "Garden Planning"},
		{"curly-quotes", "“Quantum Basics”.", "Quantum Basics"},
		{"markdown-heading", "# Trip to Kyoto", "Trip to Kyoto"},
		{"bullet", "- fix the dockerfile", "fix the dockerfile"},
		{"block-quote", "> regex help needed", "regex help needed"},
		{"multiline", "First Idea\nsecond line junk", "First Idea"},
		{"blank-first-line", "\n  Actual Title\nmore", "Actual Title"},
		{"backticks", "`go vet` warnings", "go vet warnings"},
		{"collapses-spaces", "too    many\tspaces  here", "too many spaces here"},
		{"empty", "   ", ""},
		{"punctuation-only", "...:...", ""},
		{"long", strings.Repeat("word ", 40), strings.TrimSpace(strings.Repeat("word ", 12))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeTitle(tc.raw); got != tc.want {
				t.Errorf("SanitizeTitle(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSanitizeTitleCapsLength(t *testing.T) {
	got := SanitizeTitle(strings.Repeat("ü", 200)) // multi-byte: rune cap, not byte cap
	if len([]rune(got)) != 60 {
		t.Errorf("rune length = %d, want 60 (bytes: %d)", len([]rune(got)), len(got))
	}
}
