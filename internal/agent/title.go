package agent

import (
	"context"
	"strings"
	"unicode/utf8"

	"gabkeep/internal/llm"
)

// TitleSystemPrompt asks the model for a conversation name. The phrase
// "name chat conversations" doubles as the marker tests use to tell title
// requests apart from real chat requests.
const TitleSystemPrompt = "You name chat conversations. Reply with ONLY a short title " +
	"of at most 5 words that captures the topic of the user's message. " +
	"No quotes, no trailing punctuation. Use the same language as the message."

// titleMaxRunes caps stored titles; generous but keeps sidebar rows sane.
const titleMaxRunes = 60

// Title turns the first user message of a conversation into a short topic
// title. It is a best-effort side quest: any upstream failure is returned
// and the caller keeps the placeholder title.
func (g *Generator) Title(ctx context.Context, model, firstUser string) (string, error) {
	msgs := []llm.ChatMessage{
		{Role: "system", Content: TitleSystemPrompt},
		{Role: "user", Content: clipRunes(firstUser, 600)},
	}
	// Reasoning is disabled for this call (llm.Complete), so a small token
	// budget suffices; the extra headroom covers endpoints that ignore it.
	raw, err := g.LLM.Complete(ctx, model, msgs, 160)
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
// capped at titleMaxRunes. Returns "" when nothing usable remains.
func SanitizeTitle(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		t = strings.ReplaceAll(t, "`", "")
		t = strings.Trim(t, "*_#")
		t = strings.TrimPrefix(t, ">")
		t = strings.TrimLeft(t, "-• \t")
		t = strings.Trim(t, "\"'“”‘’「」『』.:;,!")
		t = strings.Join(strings.Fields(t), " ")
		if t == "" {
			continue
		}
		return strings.TrimSpace(clipRunes(t, titleMaxRunes))
	}
	return ""
}

func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
