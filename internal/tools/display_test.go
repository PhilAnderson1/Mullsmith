package tools

// C7 card budget: the live tool_end event and tool_calls.display carry the
// SAME clamped card, so what a reload shows is what the stream showed — and
// an over-budget payload degrades by DROPPING its data, never by cutting a
// JSON structure in half (which is how cards used to vanish on reload: the
// truncated string could not be parsed at all).

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustParse(t *testing.T, raw string) Card {
	t.Helper()
	if raw == "" {
		t.Fatal("empty card payload")
	}
	if len(raw) > DisplayBudget {
		t.Fatalf("payload is %d bytes, over the %d budget", len(raw), DisplayBudget)
	}
	var c Card
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("payload is not valid JSON (%v): %.120q", err, raw)
	}
	return c
}

func TestClampCardWithinBudgetKeepsEverything(t *testing.T) {
	data := []map[string]any{{"url": "https://example.org/a", "title": "A"}}
	c := ClampCard("searched", data)
	if c.Truncated {
		t.Error("small payload flagged truncated")
	}
	if c.Data == nil {
		t.Fatal("small payload lost its data")
	}
	c2 := mustParse(t, c.JSON())
	if c2.Summary != "searched" || c2.Truncated {
		t.Errorf("round trip = %+v", c2)
	}
}

func TestClampCardOverBudgetDropsDataKeepsSummary(t *testing.T) {
	big := make([]string, 0, 4000)
	for i := 0; i < 4000; i++ {
		big = append(big, strings.Repeat("x", 40))
	}
	c := ClampCard("Searched — 4000 result(s)", big)
	if !c.Truncated {
		t.Error("over-budget payload not flagged")
	}
	if c.Data != nil {
		t.Error("over-budget data kept anyway")
	}
	if c.Summary != "Searched — 4000 result(s)" {
		t.Errorf("summary trimmed with the data: %q", c.Summary)
	}
	c2 := mustParse(t, c.JSON()) // the point: it still PARSES
	if c2.Data != nil || !c2.Truncated {
		t.Errorf("stored round trip = %+v", c2)
	}
}

// A multibyte summary pushing the payload over the budget (2 bytes a rune,
// so a byte-sized guess is wrong) is rune-elided, stays valid UTF-8 and
// valid JSON, and keeps the truncated marker.
func TestClampCardMultibyteSummaryFits(t *testing.T) {
	summary := strings.Repeat("é", DisplayBudget/2+100)
	c := ClampCard(summary, nil)
	if !c.Truncated {
		t.Error("over-budget summary not flagged")
	}
	c2 := mustParse(t, c.JSON())
	if !utf8.ValidString(c2.Summary) {
		t.Error("summary was cut mid-rune")
	}
	if !strings.HasSuffix(c2.Summary, "…") || len(c2.Summary) < 2 {
		t.Errorf("summary elision marker wrong: %.40q", c2.Summary)
	}
}

// Unmarshalable data degrades to the summary alone rather than an empty
// event or a storage failure.
func TestClampCardUnmarshalableDataDegrades(t *testing.T) {
	c := ClampCard("ran", make(chan int))
	if !c.Truncated || c.Data != nil || c.Summary != "ran" {
		t.Errorf("degraded card = %+v", c)
	}
}
