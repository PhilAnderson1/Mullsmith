package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mullsmith/internal/config"
	"mullsmith/internal/llm"
)

// LM Studio + Qwen3.8 ignores chat_template_kwargs.enable_thinking and
// thinks anyway: at the old 160-token budget the reasoning ate every token
// and content came back empty, so conversations kept "New chat" forever
// (the auto-title quest treats any error as skip-and-retry). The fake
// reproduces that boundary — a title only appears once the budget covers
// the thinking preamble.
func TestTitleBudgetSurvivesAlwaysThinkingEndpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.MaxTokens <= 160 { // reasoning runs out the budget first
			fmt.Fprint(w, `{"choices":[{"message":{"content":"","reasoning_content":"We need answer user's message..."}}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Baking sourdough bread","reasoning_content":"We need answer user's message..."}}]}`)
	}))
	defer ts.Close()
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL
	cfg.LLM.Models = []config.LLMModel{{ID: "qwen/qwen3.8-27b", Default: true}}
	g := &Generator{Cfg: cfg, LLM: llm.NewClient(cfg, nil)}
	title, err := g.Title(context.Background(), "", "how do I bake sourdough", nil)
	if err != nil {
		t.Fatalf("Title: %v", err)
	}
	if title != "Baking sourdough bread" {
		t.Errorf("title = %q, want the endpoint's title", title)
	}
}

// The side-quest fragment rides the title request in place of the
// fallback hint: one thinking control on the wire, the catalogue's.
func TestTitleCarriesQuietFragment(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Sourdough basics"}}]}`)
	}))
	defer ts.Close()
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL
	cfg.LLM.Models = []config.LLMModel{{ID: "m", Default: true}}
	g := &Generator{Cfg: cfg, LLM: llm.NewClient(cfg, nil)}
	title, err := g.Title(context.Background(), "m", "how do I bake sourdough",
		map[string]any{"reasoning_effort": "none"})
	if err != nil {
		t.Fatalf("Title: %v", err)
	}
	if title != "Sourdough basics" {
		t.Errorf("title = %q", title)
	}
	if got["reasoning_effort"] != "none" {
		t.Errorf("fragment did not ride: %v", got)
	}
	if _, present := got["chat_template_kwargs"]; present {
		t.Error("the fallback hint must step aside for the catalogue fragment")
	}
}

// Without a fragment the fallback convention stands on its own.
func TestTitleFallbackHintWithoutFragment(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Sourdough basics"}}]}`)
	}))
	defer ts.Close()
	cfg := &config.Config{}
	cfg.LLM.Endpoint = ts.URL
	cfg.LLM.Models = []config.LLMModel{{ID: "m", Default: true}}
	g := &Generator{Cfg: cfg, LLM: llm.NewClient(cfg, nil)}
	if _, err := g.Title(context.Background(), "m", "how do I bake sourdough", nil); err != nil {
		t.Fatalf("Title: %v", err)
	}
	kw, ok := got["chat_template_kwargs"].(map[string]any)
	if !ok || kw["enable_thinking"] != false {
		t.Errorf("fallback hint missing: %v", got)
	}
}

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
		{"long", strings.Repeat("word ", 40), "word word word word word"},
		{"echoed-question", "To find the number of four-digit positive integers that contain", "To find the number of"},
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
