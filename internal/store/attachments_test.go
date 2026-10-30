package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mullsmith/internal/id"
)

func mkConvMsg(t *testing.T, db *DB, ctx context.Context, userID, title string) (Conversation, Message) {
	t.Helper()
	conv := Conversation{ID: id.New(), UserID: userID, Title: title,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateConversation(ctx, &conv); err != nil {
		t.Fatal(err)
	}
	msg := Message{ConversationID: conv.ID, Role: "user", Content: "hi"}
	if err := db.AddMessage(ctx, userID, &msg); err != nil {
		t.Fatal(err)
	}
	return conv, msg
}

func TestAttachmentsRoundTripAndProjection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	alice := mkUser("a@x.io")
	db.CreateUser(ctx, alice)
	conv, msg := mkConvMsg(t, db, ctx, alice.ID, "t")

	paths := []string{"uploads/report.pdf", "uploads/data.csv"}
	if err := db.AttachFilesToMessage(ctx, msg.ID, alice.ID, paths); err != nil {
		t.Fatal(err)
	}
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
	_, msg2 := mkConvMsg(t, db, ctx, alice.ID, "t2")
	if err := db.AttachFilesToMessage(ctx, msg2.ID, alice.ID, []string{"uploads/report.pdf"}); err != nil {
		t.Errorf("same path on another message: %v", err)
	}
}

// The attachments rows ride the users→conversations→messages cascade and
// nothing else: deleting a thread removes its pairings, deleting a file
// (a disk event) is not expressible here at all — that is the point.
func TestAttachmentsCascadeOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	alice := mkUser("c@x.io")
	db.CreateUser(ctx, alice)
	_, msg := mkConvMsg(t, db, ctx, alice.ID, "t")

	if err := db.AttachFilesToMessage(ctx, msg.ID, alice.ID, []string{"uploads/a.txt"}); err != nil {
		t.Fatal(err)
	}
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

	if err := db.DeleteMessage(ctx, msg.ID); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Errorf("rows survived message delete")
	}

	// Conversation delete cascades through messages to attachments too.
	conv2, msg2 := mkConvMsg(t, db, ctx, alice.ID, "t2")
	if err := db.AttachFilesToMessage(ctx, msg2.ID, alice.ID, []string{"uploads/b.txt"}); err != nil {
		t.Fatal(err)
	}
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
	conv, msg := mkConvMsg(t, db, ctx, alice.ID, "t")

	if err := db.AttachFilesToMessage(ctx, msg.ID, alice.ID,
		[]string{"uploads/first.txt", "uploads/first.txt"}); err == nil {
		t.Fatal("duplicate path accepted")
	}
	items, _, err := db.ListMessages(ctx, alice.ID, conv.ID, "", 10)
	if err != nil || len(items) != 1 || len(items[0].Attachments) != 0 {
		t.Fatalf("failed batch must attach nothing: %+v, %v", items[0].Attachments, err)
	}

	// A bogus message id is an FK failure, likewise atomic.
	if err := db.AttachFilesToMessage(ctx, id.New(), alice.ID,
		[]string{"uploads/x.txt", "uploads/y.txt"}); err == nil {
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

// The upgrade path the deploy actually takes: a production DB sits at 0005
// with registry rows and old-shape attachment rows, foreign_keys is ON, and
// store.Open must apply 0006 cleanly — attachments dropped BEFORE files,
// the table pair replaced by the path-keyed join, no leftover index.
func TestMigration0006DropsFilesRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prod.db")
	dsn := "file:" + path + "?_pragma=foreign_keys(1)"
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		ver  int
		file string
	}{
		{1, "0001_init.sql"}, {2, "0002_conv.sql"}, {3, "0003_tools.sql"},
		{4, "0004_files.sql"}, {5, "0005_artifact_path_unique.sql"},
	} {
		body, err := migrationsFS.ReadFile("migrations/" + m.file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", m.file, err)
		}
		if _, err := raw.Exec(`INSERT INTO schema_version(version, applied_at) VALUES(?, ?)`,
			m.ver, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	// Populate the registry exactly like the old server did.
	uid, mid, fid := id.New(), id.New(), id.New()
	now := time.Now().Unix()
	if _, err := raw.Exec(`INSERT INTO users(id,email,name,password_hash,status,created_at,updated_at)
		VALUES(?,?,'t','h','enabled',?,?)`, uid, "old@x.io", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO conversations(id,user_id,title,model,created_at,updated_at)
		VALUES(?,?,'t','m',?,?)`, "conv1", uid, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO messages(id,conversation_id,role,content,status,created_at)
		VALUES(?,?,'user','hi','complete',?)`, mid, "conv1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO files(id,user_id,kind,rel_path,media_type,name,size,sha256,created_at)
		VALUES(?,?, 'upload','uploads/x.txt','text/plain','x.txt',5,x'00',?)`, fid, uid, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO attachments(message_id,file_id) VALUES(?,?)`, mid, fid); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// The production open: applies 0006 on top of the old schema.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open over 0005 data: %v", err)
	}
	defer db.Close()

	var name string
	err = db.sqlDB.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='files'`).Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("files table survived migration: %q, %v", name, err)
	}
	// The old indexes dropped with the table.
	var idxCount int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name LIKE 'idx_files%'`).Scan(&idxCount); err != nil {
		t.Fatal(err)
	}
	if idxCount != 0 {
		t.Errorf("leftover files indexes: %d", idxCount)
	}
	// attachments now has exactly the (message_id, path) shape.
	cols := map[string]bool{}
	rows, err := db.sqlDB.Query(`SELECT name FROM pragma_table_info('attachments')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols[c] = true
	}
	rows.Close()
	if len(cols) != 2 || !cols["message_id"] || !cols["path"] {
		t.Errorf("attachments columns = %v, want message_id+path", cols)
	}
	// The chat fact survived; the pairing did not (no data migration — the
	// owner sanctioned discarding it together with the bytes).
	var attCount int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attCount); err != nil {
		t.Fatal(err)
	}
	if attCount != 0 {
		t.Errorf("old pairing rows survived: %d", attCount)
	}
	// And the new shape works on the migrated file, FKs still enforced.
	if err := db.AttachFilesToMessage(context.Background(), mid, uid, []string{"uploads/new.txt"}); err != nil {
		t.Errorf("attach after migration: %v", err)
	}
}
