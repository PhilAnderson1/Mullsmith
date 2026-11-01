package store

import (
	"context"
	"errors"
	"testing"

	"mullsmith/internal/id"
)

// Finding 2: exceeding the ContextHistory row cap is an EXPLICIT refusal,
// never a silently-truncated oldest slice — the caller must not be able to
// mistake a partial history for the whole one.
func TestContextHistoryCapRefusesExplicitly(t *testing.T) {
	defer func(old int) { contextHistoryCap = old }(contextHistoryCap)
	contextHistoryCap = 3

	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("cap@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv := Conversation{ID: id.New(), UserID: u.ID}
	if err := db.CreateConversation(ctx, &conv); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < contextHistoryCap; i++ {
		m := &Message{ConversationID: conv.ID, Role: "user", Content: "turn"}
		if err := db.AddMessage(ctx, u.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := db.ContextHistory(ctx, conv.ID, ""); err != nil || len(got) != contextHistoryCap {
		t.Fatalf("at-cap history = %d rows, %v; want %d, nil", len(got), err, contextHistoryCap)
	}

	// One row past the cap: the read fails loudly.
	if err := db.AddMessage(ctx, u.ID, &Message{ConversationID: conv.ID, Role: "assistant", Content: "one too many"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ContextHistory(ctx, conv.ID, ""); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("past-cap ContextHistory = %v, want ErrHistoryLimit", err)
	}
}

// Finding 4: AddMessageWithFiles lands the turn and its attachment links
// together — the projection sees both, or (as one transaction) neither.
func TestAddMessageWithFiles(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("files@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv := Conversation{ID: id.New(), UserID: u.ID}
	if err := db.CreateConversation(ctx, &conv); err != nil {
		t.Fatal(err)
	}

	m := &Message{ConversationID: conv.ID, Role: "user", Content: "see uploads/a.txt"}
	paths := []string{"uploads/a.txt", "uploads/b.txt"}
	if err := db.AddMessageWithFiles(ctx, u.ID, m, paths); err != nil {
		t.Fatal(err)
	}
	got, err := db.ContextHistory(ctx, conv.ID, "")
	if err != nil || len(got) != 1 {
		t.Fatalf("history = %d rows, %v", len(got), err)
	}
	if len(got[0].Attachments) != 2 {
		t.Errorf("attachments = %+v, want both paths in the same read", got[0].Attachments)
	}

	// Ownership still guards the combined insert.
	other := mkUser("nope@x.io")
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	err = db.AddMessageWithFiles(ctx, other.ID,
		&Message{ConversationID: conv.ID, Role: "user", Content: "intruder"}, paths)
	if !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("foreign insert = %v, want ErrConversationNotFound", err)
	}
}

// Finding 4: BeginRegeneration swaps the newest assistant reply for a
// streaming placeholder in ONE transaction — newest-check included, so a
// raced send answers ErrNotLastMessage and NOTHING is lost.
func TestBeginRegeneration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("regen@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	conv, userMsg := mkConvMsg(t, db, ctx, u.ID, "t")

	old := &Message{ConversationID: conv.ID, Role: "assistant", Content: "old answer"}
	if err := db.AddMessage(ctx, u.ID, old); err != nil {
		t.Fatal(err)
	}
	// A cascade companion: the audit row must disappear WITH the old reply.
	tc := &ToolCall{MessageID: old.ID, LLMCallID: "call_1", Tool: "fs_read",
		Arguments: `{}`, Status: "ok"}
	if err := db.AddToolCall(ctx, tc); err != nil {
		t.Fatal(err)
	}

	ph, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, old.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if ph.Role != "assistant" || ph.Status != "streaming" {
		t.Errorf("placeholder = %s/%s, want assistant/streaming", ph.Role, ph.Status)
	}

	// The old reply and its audit row are gone (cascade); the placeholder
	// is the newest row but INVISIBLE to context replay until finalized.
	if _, err := db.GetMessageForUser(ctx, u.ID, old.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("old reply read = %v, want gone", err)
	}
	if calls, err := db.ToolCallsForMessages(ctx, []string{old.ID}); err != nil || len(calls[old.ID]) != 0 {
		t.Errorf("audit row survived the swap: %+v (%v)", calls[old.ID], err)
	}
	hist, err := db.ContextHistory(ctx, conv.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].ID != userMsg.ID {
		t.Errorf("history = %+v, want only the user turn (streaming row must not replay)", hist)
	}
	page, _, err := db.ListMessages(ctx, u.ID, conv.ID, "", 10)
	if err != nil || len(page) == 0 || page[len(page)-1].ID != ph.ID {
		t.Fatalf("placeholder is not the newest row: %+v (%v)", page, err)
	}

	// The generation completes into the placeholder — final history is the
	// user turn plus the regenerated answer, exactly one reply deep.
	if err := db.FinalizeMessage(ctx, ph.ID, "new answer", "complete", "", 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	hist, _ = db.ContextHistory(ctx, conv.ID, "")
	if len(hist) != 2 || hist[1].Content != "new answer" {
		t.Fatalf("history after finalize = %+v", hist)
	}

	// Re-running against the retired id cannot resurrect anything.
	if _, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, old.ID, ""); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("retired id = %v, want ErrConversationNotFound", err)
	}

	// The racy case the finding names: by the time regeneration asks, a
	// NEWER user turn exists. The swap refuses with ErrNotLastMessage and
	// the conversation is untouched — no placeholder leak, no lost reply.
	nudge := &Message{ConversationID: conv.ID, Role: "user", Content: "wait, new question"}
	if err := db.AddMessage(ctx, u.ID, nudge); err != nil {
		t.Fatal(err)
	}
	before := countMessages(t, db, conv.ID)
	if _, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, ph.ID, ""); !errors.Is(err, ErrNotLastMessage) {
		t.Fatalf("not-newest swap = %v, want ErrNotLastMessage", err)
	}
	if countMessages(t, db, conv.ID) != before {
		t.Error("refused swap changed the conversation")
	}
	if _, err := db.GetMessageForUser(ctx, u.ID, ph.ID); err != nil {
		t.Errorf("refused swap damaged the existing reply: %v", err)
	}

	// Ownership: another user cannot swap inside someone's thread.
	other := mkUser("other@x.io")
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginTurnRegeneration(ctx, other.ID, conv.ID, nudge.ID, ""); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("foreign swap = %v, want ErrConversationNotFound", err)
	}
	// A USER row is not regenerable through the store either (belt to the
	// handler's 400): same indistinguishable not-found.
	if _, err := db.BeginTurnRegeneration(ctx, u.ID, conv.ID, nudge.ID, ""); !errors.Is(err, ErrConversationNotFound) ||
		countMessages(t, db, conv.ID) != before {
		t.Error("user-row swap must be refused without side effects")
	}
}

func countMessages(t *testing.T, db *DB, convID string) int {
	t.Helper()
	var n int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
