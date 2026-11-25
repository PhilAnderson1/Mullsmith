package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPinnedConversationOrderingAndPagination(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	user := mkUser("pins@example.com")
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	var convs []*Conversation
	for i := 0; i < 5; i++ {
		c := &Conversation{UserID: user.ID, Title: "matching", CreatedAt: time.Unix(int64(100+i), 0)}
		if err := db.CreateConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
		convs = append(convs, c)
	}
	yes, no := true, false
	for _, i := range []int{0, 2} {
		saved, err := db.UpdateConversation(ctx, user.ID, convs[i].ID, ConversationPatch{Pinned: &yes})
		if err != nil {
			t.Fatal(err)
		}
		if !saved.Pinned || !saved.UpdatedAt.Equal(convs[i].UpdatedAt) {
			t.Fatal("pin changed activity or did not persist")
		}
	}
	expected := []string{convs[2].ID, convs[0].ID, convs[4].ID, convs[3].ID, convs[1].ID}
	for _, limit := range []int{1, 2, 3} {
		var got []string
		cursor := ""
		for {
			items, next, err := db.ListConversations(ctx, user.ID, "matching", cursor, limit)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range items {
				got = append(got, c.ID)
			}
			if next == "" {
				break
			}
			cursor = next
			if len(got) > len(expected) {
				t.Fatal("pagination did not finish")
			}
		}
		if len(got) != len(expected) {
			t.Fatalf("got %v", got)
		}
		for i := range got {
			if got[i] != expected[i] {
				t.Fatalf("limit %d: got %v want %v", limit, got, expected)
			}
		}
	}
	if _, err := db.UpdateConversation(ctx, "other", convs[0].ID, ConversationPatch{Pinned: &no}); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("ownership: %v", err)
	}
	if _, err := db.UpdateConversation(ctx, user.ID, convs[2].ID, ConversationPatch{Pinned: &no}); err != nil {
		t.Fatal(err)
	}
	items, _, err := db.ListConversations(ctx, user.ID, "", "", 10)
	if err != nil || items[0].ID != convs[0].ID || items[1].ID != convs[4].ID {
		t.Fatalf("unpin order: %v %v", items, err)
	}
	// Activity moves a thread only within its pinned group.
	if err := db.AddMessage(ctx, user.ID, &Message{ConversationID: convs[1].ID, Role: "user", Content: "hello", Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	items, _, err = db.ListConversations(ctx, user.ID, "", "", 10)
	if err != nil || items[0].ID != convs[0].ID || items[1].ID != convs[1].ID {
		t.Fatal("activity moved thread ahead of pins")
	}
}

func TestPinnedMigrationAndReopen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "v4.db")
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{migrationV1SQL, migrationV2SQL, migrationV3SQL, migrationV4SQL, "PRAGMA user_version = 4"} {
		if _, err := raw.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	db, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	user := mkUser("migrate-pin@example.com")
	ctx := context.Background()
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	conv := mkConv(t, db, user.ID, "pin")
	saved, err := db.ConversationForUser(ctx, user.ID, conv.ID)
	if err != nil || saved.Pinned {
		t.Fatal("default is not unpinned")
	}
	yes := true
	if _, err := db.UpdateConversation(ctx, user.ID, conv.ID, ConversationPatch{Pinned: &yes}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	saved, err = db.ConversationForUser(ctx, user.ID, conv.ID)
	if err != nil || !saved.Pinned {
		t.Fatal("pin did not survive reopen")
	}
}
