package store

import (
	"context"
	"testing"
	"time"

	"mullsmith/internal/id"
)

func mkConvMsg(t *testing.T, db *DB, ctx context.Context, userID, title string, paths ...string) (Conversation, Message) {
	t.Helper()
	conv := Conversation{ID: id.New(), UserID: userID, Title: title,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateConversation(ctx, &conv); err != nil {
		t.Fatal(err)
	}
	msg := Message{ConversationID: conv.ID, Role: "user", Content: "hi"}
	if err := db.AddMessageWithFiles(ctx, userID, &msg, paths); err != nil {
		t.Fatal(err)
	}
	return conv, msg
}

func TestAttachmentsRoundTripAndProjection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	alice := mkUser("a@x.io")
	db.CreateUser(ctx, alice)
	conv, _ := mkConvMsg(t, db, ctx, alice.ID, "t", "uploads/report.pdf", "uploads/data.csv")
	got, err := db.ContextHistory(ctx, conv.ID, "")
	if err != nil || len(got) != 1 {
		t.Fatalf("history = %d, %v", len(got), err)
	}
	atts := got[0].Attachments
	if len(atts) != 2 || atts[0].Path != "uploads/data.csv" || atts[1].Path != "uploads/report.pdf" {
		t.Fatalf("attachments = %+v, want both paths ordered", atts)
	}
	if atts[0].Name() != "data.csv" {
		t.Errorf("Name() = %q, want basename", atts[0].Name())
	}

	// The row is a pure path fact: nothing else in the schema references it.
	// A second message can repeat the same path — it is a DIFFERENT pairing.
	mkConvMsg(t, db, ctx, alice.ID, "t2", "uploads/report.pdf")
}

// The attachments rows ride the users→conversations→messages cascade and
// nothing else: deleting a thread removes its pairings, deleting a file
// (a disk event) is not expressible here at all — that is the point.
func TestAttachmentsCascadeOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	alice := mkUser("c@x.io")
	db.CreateUser(ctx, alice)
	conv, _ := mkConvMsg(t, db, ctx, alice.ID, "t", "uploads/a.txt")
	count := func() int {
		var n int
		if err := db.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("attachments = %d, want 1", count())
	}

	if err := db.DeleteConversation(ctx, alice.ID, conv.ID); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Errorf("rows survived conversation delete")
	}

	// Conversation delete cascades through messages to attachments too.
	conv2, _ := mkConvMsg(t, db, ctx, alice.ID, "t2", "uploads/b.txt")
	if _, err := db.sqlDB.ExecContext(ctx, `DELETE FROM conversations WHERE id=?`, conv2.ID); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Errorf("rows survived conversation cascade")
	}
}

// Duplicate (message, path) pairs are PK violations; the batch is a
// transaction, so a mid-batch failure leaves NOTHING attached.
func TestAttachmentsBatchAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	alice := mkUser("d@x.io")
	db.CreateUser(ctx, alice)
	conv, _ := mkConvMsg(t, db, ctx, alice.ID, "t")

	_, _, err := db.BeginSend(ctx, alice.ID, conv.ID, "with files", []string{"uploads/first.txt", "uploads/first.txt"}, 1, id.New())
	if err == nil {
		t.Fatal("duplicate path accepted")
	}
	items, _, err := db.ListMessages(ctx, alice.ID, conv.ID, "", 10)
	if err != nil || len(items) != 1 || len(items[0].Attachments) != 0 {
		t.Fatalf("failed batch must leave the original message alone: %+v, %v", items, err)
	}

	// A nonexistent conversation is refused before any attachment is written.
	_, _, err = db.BeginSend(ctx, alice.ID, id.New(), "with files", []string{"uploads/x.txt", "uploads/y.txt"}, 1, id.New())
	if err == nil {
		t.Fatal("attach to nonexistent message accepted")
	}
	var n int
	db.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments`).Scan(&n)
	if n != 0 {
		t.Errorf("failed FK batch left %d rows", n)
	}
}

func TestUserIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	a, b := mkUser("u1@x.io"), mkUser("u2@x.io")
	db.CreateUser(ctx, a)
	db.CreateUser(ctx, b)
	ids, err := db.UserIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("UserIDs = %v, want 2 entries", ids)
	}
	set := map[string]bool{}
	for _, s := range ids {
		set[s] = true
	}
	if !set[a.ID] || !set[b.ID] {
		t.Errorf("UserIDs missing entries: %v", ids)
	}
}
