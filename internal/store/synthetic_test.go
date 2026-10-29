package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seedThrough0011 builds a database exactly as it stood before the
// synthetic flag existed: migrations 0001..0011 applied through the same
// DSN pragmas the app uses, one user, one conversation, one genuine user
// row, one legacy view_image row (identifiable only by its note prefix)
// and one assistant reply.
func seedThrough0011(t *testing.T, path string) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=data_sync(NORMAL)",
		filepath.Clean(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, e := range entries {
		var ver int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &ver); err != nil {
			t.Fatal(err)
		}
		if ver >= 12 {
			continue // 0012 must be applied by Open/migrate, not seeded
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			t.Fatalf("seed %s: %v", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version, applied_at) VALUES(?, ?)`, ver, now); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO users(id, email, name, password_hash, status, default_system_prompt, created_at, updated_at)
		VALUES ('u-ana', 'ana@example.com', 'Ana', 'h1', 'active', 'be brief', 100, 100);
		INSERT INTO conversations(id, user_id, title, model, system_prompt, created_at, updated_at)
		VALUES ('c-1', 'u-ana', 'T', '', '', 100, 100);
		INSERT INTO messages(id, conversation_id, role, content, status, token_count, created_at)
		VALUES ('m-real',   'c-1', 'user',      'look at the photo',        'complete', 5,  100),
		       ('m-legacy', 'c-1', 'user',      '[view_image attached "photo.png" so you can see it; it stays in this conversation for later turns]', 'complete', 20, 101),
		       ('m-reply',  'c-1', 'assistant', 'A red bicycle.',           'complete', 8,  102);`); err != nil {
		t.Fatal(err)
	}
}

// Migration 0012 must flag pre-flag view turns: unflagged they would
// render as "You" bubbles AND lose their image on replay, a silent
// regression for every conversation that ever used view_image.
func TestMigration0012BackfillsLegacyViewTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	seedThrough0011(t, path)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	items, _, err := db.ListMessages(ctx, "u-ana", "c-1", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"m-real": false, "m-legacy": true, "m-reply": false}
	if len(items) != len(want) {
		t.Fatalf("items = %d, want %d", len(items), len(want))
	}
	for _, m := range items {
		exp, ok := want[m.ID]
		if !ok {
			t.Fatalf("unexpected row %q", m.ID)
		}
		if m.Synthetic != exp {
			t.Errorf("row %s: Synthetic = %v, want %v", m.ID, m.Synthetic, exp)
		}
	}

	// Replay reads ContextHistory — the flag must survive there too,
	// or the legacy picture silently stops embedding.
	hist, err := db.ContextHistory(ctx, "c-1", "m-reply")
	if err != nil {
		t.Fatal(err)
	}
	var legacy *Message
	for i := range hist {
		if hist[i].ID == "m-legacy" {
			legacy = &hist[i]
		}
	}
	if legacy == nil {
		t.Fatal("legacy view turn missing from ContextHistory")
	}
	if !legacy.Synthetic {
		t.Errorf("legacy view turn lost its flag in ContextHistory")
	}
}

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
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Synthetic {
		t.Errorf("a typed message starting with the marker must not be flagged")
	}
	if !items[1].Synthetic {
		t.Errorf("AddMessage lost the Synthetic flag")
	}
}
