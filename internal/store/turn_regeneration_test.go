package store

// Turn regeneration retires every row a generation wrote: its assistant
// rounds, the
// role='tool' answers of its calls, the synthetic view_image turns — and
// admission/prompt building has to see the SAME set before the swap, or the
// retry quotes rows that are seconds from deletion.

import (
	"context"
	"errors"
	"testing"

	"mullsmith/internal/id"
)

// seedGenerationTurn lays down one generation's worth of rows under genID: an
// assistant round, its tool call (audit row + role='tool' answer), a
// synthetic image turn, and the answering round. Returns their ids.
func seedGenerationTurn(t *testing.T, db *DB, ctx context.Context, userID, convID, genID string,
) (round1, toolRow, synthRow, round2 string) {
	t.Helper()
	round1 = id.New()
	m1 := &Message{ID: round1, ConversationID: convID, Role: "assistant",
		Content: "let me search", Status: "complete", GenerationID: genID}
	if err := db.AddMessage(ctx, userID, m1); err != nil {
		t.Fatal(err)
	}
	tc := &ToolCall{MessageID: round1, LLMCallID: "call_" + genID, Tool: "web_search",
		Arguments: `{}`, Status: "ok"}
	if err := db.AddToolCall(ctx, tc); err != nil {
		t.Fatal(err)
	}
	toolRow = id.New()
	tr := &Message{ID: toolRow, ConversationID: convID, Role: "tool",
		Content: `{"r":1}`, Status: "complete", ToolCallID: tc.LLMCallID,
		GenerationID: genID}
	if err := db.AddMessage(ctx, userID, tr); err != nil {
		t.Fatal(err)
	}
	synthRow = id.New()
	sy := &Message{ID: synthRow, ConversationID: convID, Role: "user",
		Content: "image attached", Status: "complete", Synthetic: true,
		GenerationID: genID}
	if err := db.AddMessage(ctx, userID, sy); err != nil {
		t.Fatal(err)
	}
	round2 = id.New()
	m2 := &Message{ID: round2, ConversationID: convID, Role: "assistant",
		Content: "the answer", Status: "complete", GenerationID: genID}
	if err := db.AddMessage(ctx, userID, m2); err != nil {
		t.Fatal(err)
	}
	return
}

func idSet(ids []string) map[string]bool {
	s := map[string]bool{}
	for _, i := range ids {
		s[i] = true
	}
	return s
}

func TestRegenerationNeverRetiresAnotherGenerationsResult(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("repeated-call@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv, _ := mkConvMsg(t, db, ctx, u.ID, "t")
	_, oldResult, _, _ := seedGenerationTurn(t, db, ctx, u.ID, conv.ID, "old-generation")
	_, _, _, last := seedGenerationTurn(t, db, ctx, u.ID, conv.ID, "new-generation")
	if err := db.ExecForTest("UPDATE tool_calls SET llm_call_id='repeated'"); err != nil {
		t.Fatal(err)
	}
	if err := db.ExecForTest("UPDATE messages SET tool_call_id='repeated' WHERE role='tool'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, last, "retry-generation"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetMessageForUser(ctx, u.ID, oldResult); err != nil {
		t.Fatalf("earlier result deleted: %v", err)
	}
}

// TurnRetireIDs must name exactly what BeginTurnRegeneration deletes, and
// ContextHistory minus that set must be exactly the history the retry is
// built from.
func TestTurnRetireSetPredictsTheSwap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("turn-retirement@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv, userMsg := mkConvMsg(t, db, ctx, u.ID, "t")
	r1, toolRow, synth, r2 := seedGenerationTurn(t, db, ctx, u.ID, conv.ID, "gen_A")

	ids, err := db.TurnRetireIDs(ctx, conv.ID, r2)
	if err != nil {
		t.Fatal(err)
	}
	want := idSet([]string{r1, toolRow, synth, r2})
	if len(ids) != len(want) {
		t.Fatalf("retire set = %v, want the four rows of the turn", ids)
	}
	for _, i := range ids {
		if !want[i] {
			t.Errorf("retire set names foreign row %q", i)
		}
	}

	// The retry's history: exclude the predicted set, and only the user's
	// own turn remains — no round, no tool answer, no synthetic image.
	hist, err := db.ContextHistory(ctx, conv.ID, ids...)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].ID != userMsg.ID {
		t.Fatalf("history minus the turn = %+v, want only the user row", hist)
	}

	// The swap deletes exactly that set (plus the audit row by cascade).
	ph, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, r2, "gen_B")
	if err != nil {
		t.Fatal(err)
	}
	if ph.GenerationID != "gen_B" {
		t.Errorf("placeholder generation = %q, want the new id", ph.GenerationID)
	}
	for _, gone := range []string{r1, toolRow, synth, r2} {
		if _, err := db.GetMessageForUser(ctx, u.ID, gone); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("row %q survived the turn retire: %v", gone, err)
		}
	}
	hist, _ = db.ContextHistory(ctx, conv.ID, "")
	if len(hist) != 1 || hist[0].ID != userMsg.ID {
		t.Errorf("history after the swap = %+v, want only the user row", hist)
	}
}

// Newestness is measured against OTHER turns. A multi-round answer's own
// later rows must not block a regenerate aimed at the turn — while a newer
// row of a different generation still must.
func TestTurnNewestnessIgnoresOwnRoundsOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("turn-newestness@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv, _ := mkConvMsg(t, db, ctx, u.ID, "t")
	r1, _, _, _ := seedGenerationTurn(t, db, ctx, u.ID, conv.ID, "gen_A")

	// Same turn, later rows exist: still the newest TURN, so the swap goes
	// through (aimed at its first round).
	ph, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, r1, "gen_B")
	if err != nil {
		t.Fatalf("swap inside the newest turn = %v, want success", err)
	}

	// A newer row belonging to another turn
	// blocks again.
	later := &Message{ConversationID: conv.ID, Role: "user", Content: "new question",
		Status: "complete"}
	if err := db.AddMessage(ctx, u.ID, later); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, ph.ID, "gen_C"); !errors.Is(err, ErrNotLastMessage) {
		t.Fatalf("swap under a newer foreign row = %v, want ErrNotLastMessage", err)
	}
}

// SetStatusNote is the durable half of "the stream's reason survives a
// reload": it sticks on the row, and a row deleted underneath it is
// ErrRowGone rather than a silent success.
func TestSetStatusNote(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("status-note@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv, _ := mkConvMsg(t, db, ctx, u.ID, "t")
	m := &Message{ConversationID: conv.ID, Role: "assistant",
		Content: "partial", Status: "complete"}
	if err := db.AddMessage(ctx, u.ID, m); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatusNote(ctx, m.ID, "stopped short"); err != nil {
		t.Fatal(err)
	}
	items, _, err := db.ListMessages(ctx, u.ID, conv.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == m.ID && it.StatusNote != "stopped short" {
			t.Errorf("note on reload = %q", it.StatusNote)
		}
	}
	if err := db.SetStatusNote(ctx, id.New(), "nowhere"); !errors.Is(err, ErrRowGone) {
		t.Errorf("note on a missing row = %v, want ErrRowGone", err)
	}
}

// ContextHistory with several exclusions keeps every other row — the
// regenerate path passes the whole predicted retire set, and a send passes
// none (variadic call with "" excluded, as before).
func TestContextHistoryMultipleExcludes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("history-exclusions@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv, _ := mkConvMsg(t, db, ctx, u.ID, "t")
	ids := []string{}
	for i := 0; i < 4; i++ {
		m := &Message{ConversationID: conv.ID, Role: "user", Content: "t",
			Status: "complete"}
		if err := db.AddMessage(ctx, u.ID, m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	// exclude ids[1] and ids[3] (+ an empty id, meaning "nothing")
	hist, err := db.ContextHistory(ctx, conv.ID, "", ids[1], ids[3])
	if err != nil {
		t.Fatal(err)
	}
	// 1 user row from mkConvMsg + 4 - 2 excluded = 3
	if len(hist) != 3 {
		t.Fatalf("history = %d rows, want 3", len(hist))
	}
	for _, h := range hist {
		if h.ID == ids[1] || h.ID == ids[3] {
			t.Errorf("excluded row %q replayed anyway", h.ID)
		}
	}
}
