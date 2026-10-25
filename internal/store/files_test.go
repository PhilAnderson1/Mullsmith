package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gabkeep/internal/id"
)

func mkFile(userID, kind, rel string) *File {
	return &File{
		ID: id.New(), UserID: userID, Kind: kind, RelPath: rel,
		MediaType: "text/plain", Name: rel, Size: 12,
		SHA256:    []byte("0123456789abcdef0123456789abcdef"),
		CreatedAt: time.Now(),
	}
}

func TestFilesCRUDAndScoping(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	alice, bob := mkUser("a@x.io"), mkUser("b@x.io")
	db.CreateUser(ctx, alice)
	db.CreateUser(ctx, bob)

	f := mkFile(alice.ID, FileKindUpload, "uploads/"+alice.ID)
	if err := db.CreateFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetFileForUser(ctx, alice.ID, f.ID)
	if err != nil || got.Name != f.Name || got.Size != 12 {
		t.Fatalf("GetFileForUser: %+v, %v", got, err)
	}
	// Bob cannot see or delete Alice's file (§9.2).
	if _, err := db.GetFileForUser(ctx, bob.ID, f.ID); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("bob get = %v, want ErrFileNotFound", err)
	}
	if _, err := db.DeleteFileForUser(ctx, bob.ID, f.ID); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("bob delete = %v, want ErrFileNotFound", err)
	}

	deleted, err := db.DeleteFileForUser(ctx, alice.ID, f.ID)
	if err != nil || deleted.RelPath != f.RelPath {
		t.Fatalf("delete returned %+v, %v", deleted, err)
	}
	if _, err := db.GetFileForUser(ctx, alice.ID, f.ID); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestFilesPaginationAndKind(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("p@x.io")
	db.CreateUser(ctx, u)

	for i := range 7 {
		f := mkFile(u.ID, FileKindUpload, "uploads/f"+string(rune('a'+i)))
		f.CreatedAt = time.Unix(int64(1000+i), 0) // deterministic order
		if err := db.CreateFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	art := mkFile(u.ID, FileKindArtifact, "artifacts/report.md")
	art.CreatedAt = time.Unix(2000, 0)
	db.CreateFile(ctx, art)

	page1, next, err := db.ListFiles(ctx, u.ID, "", "", 3)
	if err != nil || len(page1) != 3 || next == "" {
		t.Fatalf("page1: %d, %q, %v", len(page1), next, err)
	}
	if page1[0].Kind != FileKindArtifact { // newest first
		t.Errorf("newest = %s/%s, want artifact", page1[0].Kind, page1[0].Name)
	}
	var total int
	for cur := next; ; {
		page, nxt, err := db.ListFiles(ctx, u.ID, "", cur, 3)
		if err != nil {
			t.Fatal(err)
		}
		total += len(page)
		if nxt == "" {
			break
		}
		cur = nxt
	}
	if total != 5 { // 8 files total (7 uploads + 1 artifact), minus page1's 3
		t.Errorf("remaining pages total = %d, want 5", total)
	}
	arts, _, _ := db.ListFiles(ctx, u.ID, FileKindArtifact, "", 50)
	if len(arts) != 1 {
		t.Errorf("kind filter: %d artifacts", len(arts))
	}
}

func TestAttachments(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	alice, bob := mkUser("a@x.io"), mkUser("b@x.io")
	db.CreateUser(ctx, alice)
	db.CreateUser(ctx, bob)
	conv := Conversation{ID: id.New(), UserID: alice.ID, Title: "t",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateConversation(ctx, &conv); err != nil {
		t.Fatal(err)
	}
	msg := Message{ConversationID: conv.ID, Role: "user", Content: "hi"}
	if err := db.AddMessage(ctx, alice.ID, &msg); err != nil {
		t.Fatal(err)
	}

	f1 := mkFile(alice.ID, FileKindUpload, "uploads/one.csv")
	f2 := mkFile(alice.ID, FileKindUpload, "uploads/two.csv")
	steal := mkFile(bob.ID, FileKindUpload, "uploads/bob.csv")
	db.CreateFile(ctx, f1)
	db.CreateFile(ctx, f2)
	db.CreateFile(ctx, steal)

	// Batch is all-or-nothing: one foreign id rejects everything.
	if err := db.AttachFilesToMessage(ctx, msg.ID, alice.ID, []string{f1.ID, steal.ID}); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("attach with foreign id = %v, want ErrFileNotFound", err)
	}
	if fs, err := db.FilesForMessage(ctx, msg.ID); err != nil || len(fs) != 0 {
		t.Fatalf("nothing must be attached after rejection: %v, %v", fs, err)
	}
	if err := db.AttachFilesToMessage(ctx, msg.ID, alice.ID, []string{f1.ID, f2.ID}); err != nil {
		t.Fatal(err)
	}
	fs, err := db.FilesForMessage(ctx, msg.ID)
	if err != nil || len(fs) != 2 {
		t.Fatalf("FilesForMessage = %d, %v", len(fs), err)
	}
}

func TestExpiredArtifacts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("e@x.io")
	db.CreateUser(ctx, u)

	past := mkFile(u.ID, FileKindArtifact, "artifacts/old.txt")
	past.ExpiresAt = time.Now().Add(-time.Hour)
	fresh := mkFile(u.ID, FileKindArtifact, "artifacts/new.txt")
	fresh.ExpiresAt = time.Now().Add(time.Hour)
	up := mkFile(u.ID, FileKindUpload, "uploads/forever.txt") // never expires
	db.CreateFile(ctx, past)
	db.CreateFile(ctx, fresh)
	db.CreateFile(ctx, up)

	exp, err := db.ExpiredArtifacts(ctx, time.Now(), 100)
	if err != nil || len(exp) != 1 || exp[0].ID != past.ID {
		t.Fatalf("ExpiredArtifacts = %+v, %v", exp, err)
	}
	if err := db.DeleteFilesByID(ctx, []string{past.ID}); err != nil {
		t.Fatal(err)
	}
	exp, _ = db.ExpiredArtifacts(ctx, time.Now(), 100)
	if len(exp) != 0 {
		t.Errorf("expected empty after delete, got %d", len(exp))
	}
	if _, err := db.GetFileForUser(ctx, u.ID, up.ID); err != nil {
		t.Errorf("uploads must survive expiry: %v", err)
	}
}
