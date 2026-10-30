package agent

import (
	"context"
	"strings"
	"unicode/utf8"

	"mullsmith/internal/llm"
)

// TitleSystemPrompt asks the model for a conversation name. The phrase
// "name chat conversations" doubles as the marker tests use to tell title
// requests apart from real chat requests. The "5 words or fewer" rule is
// a request, not a guarantee — quiet endpoints ignore it often enough
// that SanitizeTitle enforces the same cap in code.
const TitleSystemPrompt = "You name chat conversations. Reply with ONLY a title of " +
	"5 words or fewer that captures the topic of the user's message. " +
	"No quotes, no trailing punctuation, never repeat the message. " +
	"Use the same language as the message. Example: Sourdough baking basics"

// titleMaxRunes caps stored titles; generous but keeps sidebar rows sane.
const titleMaxRunes = 60

// titleMaxWords is the word ceiling the prompt asks for, enforced here so
// a literal-minded model cannot hand back an echoed question.
const titleMaxWords = 5

// Title turns the first user message of a conversation into a short topic
// title. It is a best-effort side quest: any upstream failure is returned
// and the caller keeps the placeholder title. thinking is the catalogue's
// quietest fragment for the model (off, else the lowest rung, §12) — nil
// when no selector covers it, leaving llm.Complete's fallback hint.
func (g *Generator) Title(ctx context.Context, model, firstUser string, thinking map[string]any) (string, error) {
	msgs := []llm.ChatMessage{
		{Role: "system", Content: TitleSystemPrompt},
		{Role: "user", Content: clipRunes(firstUser, 600)},
	}
	// The quietest option asks the endpoint to skip reasoning; some
	// ignore every thinking control outright (measured on LM Studio +
	// Qwen3.8: ~90 reasoning tokens for this very prompt before any
	// content), so the budget must also cover a thinking preamble; 512
	// is generous headroom without risking a rambling model owning the
	// whole timeout.
	raw, err := g.LLM.Complete(ctx, model, msgs, 512, thinking)
	if err != nil {
		return "", err
	}
	title := SanitizeTitle(raw)
	if title == "" {
		return "", llm.ErrUpstream // model said nothing usable
	}
	return title, nil
}

// SanitizeTitle reduces a model reply to a single clean title line: first
// non-empty line, markdown/quote decorations stripped, whitespace collapsed,
// capped at titleMaxWords whole words and titleMaxRunes runes (whole words
// first so a trim can never cut a word in half). Returns "" when nothing
// usable remains.
func SanitizeTitle(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		t = strings.ReplaceAll(t, "`", "")
		t = strings.Trim(t, "*_#")
		t = strings.TrimPrefix(t, ">")
		t = strings.TrimLeft(t, "-• \t")
		t = strings.Trim(t, "\"'“”‘’「」『』.:;,!")
		words := strings.Fields(t)
		if len(words) > titleMaxWords {
			words = words[:titleMaxWords]
		}
		t = strings.Join(words, " ")
		if t == "" {
			continue
		}
		t = clipRunes(t, titleMaxRunes)
		// The caps can leave a dangling separator; tidy the tail.
		return strings.TrimRight(t, " \"'“”‘’「」『』.:;,!?—–-")
	}
	return ""
}

func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
