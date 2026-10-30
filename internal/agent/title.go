package agent

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"mullsmith/internal/llm"
)

// TitleSystemPrompt asks the model for a conversation name. The phrase
// "titles for chat conversations" doubles as the marker tests use to
// tell title requests apart from real chat requests. The user's message
// is attached as the following user turn. The "5 words" rule is a
// request, not a guarantee — SanitizeTitle enforces the same cap in code.
const TitleSystemPrompt = "You produce 5 word titles for chat conversations. " +
	"Generate a title with at maximum 5 words that describes the task involved " +
	"in answering the following prompt. For complex tasks, just provide a " +
	"simple abstract title for the task."

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
	// An echo of the question's opening is not a name. When the reply
	// was longer than the cap *and* starts with the user's own first
	// words, the model answered instead of titling: the truncated cut
	// ("To find the number of") reads worse than no title at all, so
	// keep the placeholder and let the next message retry.
	if truncatesEcho(raw, title, firstUser) {
		return "", llm.ErrUpstream
	}
	return title, nil
}

// truncatesEcho reports whether title is the trimmed head of a rambling
// reply that merely restated the start of the user's message. Case and
// punctuation are ignored; the reply must have had more than the cap's
// worth of words, so a deliberate short title in the user's phrasing
// stays acceptable.
func truncatesEcho(raw, title, firstUser string) bool {
	rw := echoWords(strings.SplitN(raw, "\n", 2)[0])
	if len(rw) <= titleMaxWords {
		return false
	}
	tw := echoWords(title)
	uw := echoWords(firstUser)
	if len(tw) == 0 || len(tw) > titleMaxWords || len(uw) < len(tw) {
		return false
	}
	for i, w := range tw {
		if rw[i] != w || uw[i] != w {
			return false
		}
	}
	return true
}

// echoWords lowercases a string into word tokens of letters and digits,
// so "To find," and "to find" compare equal.
func echoWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
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
