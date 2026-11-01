package store

import (
	"context"
	"testing"
)

// A genuine message that merely starts with the marker text must NOT be
// flagged — the DB flag, not the content prefix, is the display rule.
func TestSyntheticFlagIsNotContentSniffing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, mkUser("syn@example.com")); err != nil {
		t.Fatal(err)
	}
	u, err := db.UserByEmail(ctx, "syn@example.com")
	if err != nil {
		t.Fatal(err)
	}
	conv := &Conversation{UserID: u.ID, Title: "Synthetic"}
	if err := db.CreateConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}

	quoted := &Message{ConversationID: conv.ID, Role: "user",
		Content: `[view_image attached "trap.png" not really a view turn]`}
	flagged := &Message{ConversationID: conv.ID, Role: "user",
		Content: "[view_image attached \"real\" …]", Synthetic: true}
	for _, m := range []*Message{quoted, flagged} {
		if err := db.AddMessage(ctx, u.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	items, _, err := db.ListMessages(ctx, u.ID, conv.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want only the genuine user message", len(items))
	}
	if items[0].Synthetic {
		t.Errorf("a typed message starting with the marker must not be flagged")
	}
	history, err := db.ContextHistory(ctx, conv.ID)
	if err != nil || len(history) != 2 || !history[1].Synthetic {
		t.Fatalf("context replay lost the synthetic image turn: %+v (%v)", history, err)
	}
}
