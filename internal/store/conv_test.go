package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func mkConv(t *testing.T, db *DB, userID, title string) *Conversation {
	t.Helper()
	c := &Conversation{UserID: userID, Title: title}
	if err := db.CreateConversation(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConversationCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("conv@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	c := mkConv(t, db, u.ID, "First")
	if c.ID == "" || c.Title != "First" {
		t.Fatalf("create: %+v", c)
	}
	// Empty title defaults to "New chat".
	d := &Conversation{UserID: u.ID}
	if err := db.CreateConversation(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.Title != "New chat" {
		t.Errorf("default title = %q", d.Title)
	}

	got, err := db.ConversationForUser(ctx, u.ID, c.ID)
	if err != nil || got.Title != "First" {
		t.Fatalf("get: %+v %v", got, err)
	}

	newTitle := "Renamed"
	newModel := "llama-3"
	up, err := db.UpdateConversation(ctx, u.ID, c.ID, ConversationPatch{Title: &newTitle, Model: &newModel})
	if err != nil || up.Title != "Renamed" || up.Model != "llama-3" {
		t.Fatalf("update: %+v %v", up, err)
	}

	// No-op patch returns the row unchanged.
	same, err := db.UpdateConversation(ctx, u.ID, c.ID, ConversationPatch{})
	if err != nil || same.Title != "Renamed" {
		t.Fatalf("empty patch: %+v %v", same, err)
	}

	// Ownership: another user cannot see or touch it.
	other := mkUser("other@x.io")
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConversationForUser(ctx, other.ID, c.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("cross-user get: %v", err)
	}
	if _, err := db.UpdateConversation(ctx, other.ID, c.ID, ConversationPatch{Title: &newTitle}); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("cross-user update: %v", err)
	}
	if err := db.SoftDeleteConversation(ctx, other.ID, c.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("cross-user delete: %v", err)
	}

	// Soft delete removes from reads and lists; second delete is not-found.
	if err := db.SoftDeleteConversation(ctx, u.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConversationForUser(ctx, u.ID, c.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("get after delete: %v", err)
	}
	if err := db.SoftDeleteConversation(ctx, u.ID, c.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("double delete: %v", err)
	}
	items, _, err := db.ListConversations(ctx, u.ID, "", "", 10)
	if err != nil || len(items) != 1 || items[0].ID != d.ID {
		t.Errorf("list after delete: %+v %v", items, err)
	}
}

func TestConversationListOrderSearchAndPages(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("list@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	titles := []string{"alpha", "beta", "gamma one", "delta one", "epsilon"}
	var ids []string
	for _, title := range titles {
		c := mkConv(t, db, u.ID, title)
		ids = append(ids, c.ID)
	}

	// Move "epsilon" clearly ahead in the updated_at ordering (in-package
	// SQL: timestamps inside one test second would otherwise tie).
	x := "epsilon!"
	if _, err := db.UpdateConversation(ctx, u.ID, ids[4], ConversationPatch{Title: &x}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sqlDB.ExecContext(ctx,
		`UPDATE conversations SET updated_at = updated_at + 3600 WHERE id = ?`, ids[4]); err != nil {
		t.Fatal(err)
	}

	items, next, err := db.ListConversations(ctx, u.ID, "", "", 2)
	if err != nil || len(items) != 2 || items[0].Title != "epsilon!" {
		t.Fatalf("page 1: %+v %v", items, err)
	}
	if next == "" {
		t.Fatal("expected next cursor")
	}
	page2, next2, err := db.ListConversations(ctx, u.ID, "", next, 2)
	if err != nil || len(page2) != 2 {
		t.Fatalf("page 2: %+v %v", page2, err)
	}
	page3, next3, err := db.ListConversations(ctx, u.ID, "", next2, 2)
	if err != nil || len(page3) != 1 || next3 != "" {
		t.Fatalf("page 3: %+v %q", page3, next3)
	}
	// No duplicates across pages.
	seen := map[string]bool{}
	for _, p := range [][]Conversation{items, page2, page3} {
		for _, c := range p {
			if seen[c.ID] {
				t.Fatalf("duplicate across pages: %s", c.ID)
			}
			seen[c.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Errorf("coverage = %d, want 5", len(seen))
	}

	// Search matches substring; LIKE metacharacters are literal.
	m, _, err := db.ListConversations(ctx, u.ID, "one", "", 10)
	if err != nil || len(m) != 2 {
		t.Errorf(`search "one": %+v %v`, m, err)
	}
	m, _, err = db.ListConversations(ctx, u.ID, "%", "", 10)
	if err != nil || len(m) != 0 {
		t.Errorf(`literal %% search: %+v %v`, m, err)
	}
	// A conversation literally named "100%" is found by that query.
	pc := mkConv(t, db, u.ID, "100%")
	m, _, err = db.ListConversations(ctx, u.ID, "100%", "", 10)
	if err != nil || len(m) != 1 || m[0].ID != pc.ID {
		t.Errorf(`search "100%%": %+v %v`, m, err)
	}

	// Garbage cursor is rejected.
	if _, _, err := db.ListConversations(ctx, u.ID, "", "!!!not-base64!!!", 5); err == nil ||
		!strings.Contains(err.Error(), "invalid cursor") {
		t.Errorf("bad cursor: %v", err)
	}
}

func TestMessagesPaginationAndTouch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("msg@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	c := mkConv(t, db, u.ID, "Thread")

	base := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	for i := 0; i < 5; i++ {
		m := &Message{
			ConversationID: c.ID, Role: "user", Content: strings.Repeat("m", i+1),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := db.AddMessage(ctx, u.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	// First page = newest 2 in chronological order.
	page, next, err := db.ListMessages(ctx, u.ID, c.ID, "", 2)
	if err != nil || len(page) != 2 || page[0].Content != "mmmm" || page[1].Content != "mmmmm" {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	// Cursor walks older; final page has no cursor.
	page2, next2, err := db.ListMessages(ctx, u.ID, c.ID, next, 2)
	if err != nil || len(page2) != 2 || page2[0].Content != "mm" || page2[1].Content != "mmm" {
		t.Fatalf("page 2: %+v %v", page2, err)
	}
	page3, next3, err := db.ListMessages(ctx, u.ID, c.ID, next2, 2)
	if err != nil || len(page3) != 1 || page3[0].Content != "m" || next3 != "" {
		t.Fatalf("page 3: %+v %q", page3, next3)
	}
	// limit larger than the history: everything, no cursor.
	all, nextAll, err := db.ListMessages(ctx, u.ID, c.ID, "", 50)
	if err != nil || len(all) != 5 || nextAll != "" {
		t.Errorf("full page: %d %q %v", len(all), nextAll, err)
	}

	// A fresh message bumps updated_at to now.
	freshBefore, _ := db.ConversationForUser(ctx, u.ID, c.ID)
	if err := db.AddMessage(ctx, u.ID, &Message{ConversationID: c.ID, Role: "user", Content: "fresh"}); err != nil {
		t.Fatal(err)
	}
	live, _ := db.ConversationForUser(ctx, u.ID, c.ID)
	if !live.UpdatedAt.After(freshBefore.UpdatedAt) {
		t.Errorf("updated_at not bumped: %v vs %v", live.UpdatedAt, freshBefore.UpdatedAt)
	}

	// Ownership: another user gets not-found; writing into a foreign thread is refused.
	other := mkUser("noke@x.io")
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ListMessages(ctx, other.ID, c.ID, "", 10); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("cross-user list: %v", err)
	}
	if err := db.AddMessage(ctx, other.ID, &Message{ConversationID: c.ID, Role: "user", Content: "x"}); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("cross-user append: %v", err)
	}

	// Soft-deleted conversations reject appends.
	if err := db.SoftDeleteConversation(ctx, u.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessage(ctx, u.ID, &Message{ConversationID: c.ID, Role: "user", Content: "x"}); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("append to deleted: %v", err)
	}
}
