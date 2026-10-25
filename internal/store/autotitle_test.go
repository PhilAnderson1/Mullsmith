package store

import (
	"context"
	"testing"
)

func TestSetTitleIfDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("autotitle@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	// Placeholder conversation gets named.
	c := mkConv(t, db, u.ID, "")
	ok, err := db.SetTitleIfDefault(ctx, c.ID, "Real Topic")
	if err != nil || !ok {
		t.Fatalf("name placeholder: ok=%v err=%v", ok, err)
	}
	got, err := db.ConversationForUser(ctx, u.ID, c.ID)
	if err != nil || got.Title != "Real Topic" {
		t.Fatalf("stored title = %+v, err=%v", got, err)
	}

	// Hand-named conversation is never touched.
	r := mkConv(t, db, u.ID, "Hand Named")
	ok, err = db.SetTitleIfDefault(ctx, r.ID, "Steamroller")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("wrote over a user-chosen title")
	}

	// Explicitly named "New chat" still counts as unnamed (retry path).
	n := mkConv(t, db, u.ID, DefaultTitle)
	if ok, err := db.SetTitleIfDefault(ctx, n.ID, "Retry Topic"); err != nil || !ok {
		t.Fatalf("retry naming: ok=%v err=%v", ok, err)
	}

	// Soft-deleted conversations are out of reach.
	d := mkConv(t, db, u.ID, "")
	if err := db.SoftDeleteConversation(ctx, u.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.SetTitleIfDefault(ctx, d.ID, "Ghost Title"); err != nil || ok {
		t.Fatalf("deleted row: ok=%v err=%v", ok, err)
	}
}
