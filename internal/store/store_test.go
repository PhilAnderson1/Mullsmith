package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mullsmith/internal/auth"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mkUser(email string) *User {
	now := time.Now()
	return &User{
		Email:        email,
		Name:         "Test",
		PasswordHash: "x",
		Status:       "enabled",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestInitializeSchemaIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db1.sqlDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("fresh database version = %d, want %d", version, currentSchemaVersion)
	}
	db1.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer db2.Close()
}

func TestSchemaRejectsNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 999`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "newer than this application supports") {
		t.Fatalf("Open newer database error = %v, want unsupported-version error", err)
	}
}

func TestSchemaMigratesExistingMessagesForReasoning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(migrationV1SQL); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'reasoning_data'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reasoning_data columns = %d, want 1", count)
	}
	for _, column := range []string{"first_answer_ms", "generation_ms"} {
		if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = ?`, column).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s columns = %d, want 1", column, count)
		}
	}
}

func TestAssistantReasoningRoundTripsThroughContextHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("reasoning@example.test")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	c := &Conversation{UserID: u.ID}
	if err := db.CreateConversation(ctx, c); err != nil {
		t.Fatal(err)
	}
	m := &Message{ConversationID: c.ID, Role: "assistant", Status: "streaming"}
	if err := db.AddMessage(ctx, u.ID, m); err != nil {
		t.Fatal(err)
	}
	const trace = `{"version":1,"text":"kept"}`
	if err := db.FinalizeMessage(ctx, m.ID, "answer", trace, "complete", "", 2, 1, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	hist, err := db.ContextHistory(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].ReasoningData != trace {
		t.Fatalf("history = %+v", hist)
	}
}

func TestSchemaRejectsUnversionedNonemptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE legacy (id INTEGER PRIMARY KEY)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "unversioned but not empty") {
		t.Fatalf("Open unversioned database error = %v, want recreate error", err)
	}
}

func TestCreateGetUser(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := mkUser("Alice@Example.com")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if u.ID == "" {
		t.Error("expected generated id")
	}

	// Case-insensitive lookup (spec: email uniqueness is NOCASE).
	got, err := db.UserByEmail(ctx, "alice@example.com")
	if err != nil || got == nil {
		t.Fatalf("lookup: %v %v", got, err)
	}
	if got.Email != "Alice@Example.com" {
		t.Errorf("email roundtrip = %q", got.Email)
	}

	dup := mkUser("alice@example.com")
	if err := db.CreateUser(ctx, dup); !errors.Is(err, ErrUserExists) {
		t.Errorf("duplicate: got %v, want ErrUserExists", err)
	}

	missing, err := db.UserByEmail(ctx, "nobody@example.com")
	if err != nil || missing != nil {
		t.Errorf("missing user: %v %v, want nil nil", missing, err)
	}
}

func TestRegisterConsumesInvite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	if err := db.CreateInvite(ctx, "gk-testcode", now); err != nil {
		t.Fatal(err)
	}

	u := mkUser("a@b.io")
	if err := db.Register(ctx, u, "gk-testcode"); err != nil {
		t.Fatalf("register with invite: %v", err)
	}

	// Same code cannot be reused.
	u2 := mkUser("c@d.io")
	if err := db.Register(ctx, u2, "gk-testcode"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("invite reuse: got %v, want ErrInviteInvalid", err)
	}
	// Unknown code rejected, user not created.
	u3 := mkUser("e@f.io")
	if err := db.Register(ctx, u3, "gk-unknown"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("unknown invite: got %v", err)
	}
	if got, _ := db.UserByEmail(ctx, "e@f.io"); got != nil {
		t.Error("user must not exist after failed invite register")
	}

	// The consumption itself is the point of this test: read the row
	// directly — there is no listing endpoint on purpose.
	var usedBy *string
	if err := db.sqlDB.QueryRowContext(ctx,
		`SELECT used_by FROM invites WHERE code = 'gk-testcode'`).Scan(&usedBy); err != nil {
		t.Fatalf("invite row after register: %v", err)
	}
	if usedBy == nil || *usedBy != u.ID {
		t.Errorf("invite used_by = %v, want %q", usedBy, u.ID)
	}
}

// Every account opens with exactly one untitled "New chat" conversation,
// inserted in the Register transaction itself (spec §8): the SPA lands a
// fresh login on the newest row, so a brand-new user can type immediately.
func TestRegisterSeedsFirstConversation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := mkUser("seed@x.io")
	if err := db.Register(ctx, u, ""); err != nil {
		t.Fatal(err)
	}
	items, _, err := db.ListConversations(ctx, u.ID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("fresh account has %d conversations, want exactly the seeded one", len(items))
	}
	if items[0].Title != DefaultTitle || items[0].SystemPrompt != "" || items[0].Model != "" {
		t.Errorf("seeded conversation = %+v, want placeholder defaults", items[0])
	}

	// A rolled-back Register (dead invite) leaves no orphan conversation:
	// the seed rides the user insert's transaction.
	bad := mkUser("seed2@x.io")
	if err := db.Register(ctx, bad, "no-such-code"); err == nil {
		t.Fatal("invite should have failed")
	}
	var n int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("failed register left %d conversations, want the one from the successful sign-up", n)
	}
}

func TestSessionsLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	u := mkUser("s@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	_, hashA, _ := auth.NewSessionToken()
	if err := db.CreateSession(ctx, &Session{
		IDHash: hashA, UserID: u.ID, CreatedAt: now,
		ExpiresAt: now.Add(time.Hour), Remembered: true, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	rawB, hashB, _ := auth.NewSessionToken()
	_ = rawB
	if err := db.CreateSession(ctx, &Session{
		IDHash: hashB, UserID: u.ID, CreatedAt: now,
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	s, err := db.SessionByHash(ctx, hashA)
	if err != nil || s == nil {
		t.Fatalf("lookup A: %v %v", s, err)
	}
	if !s.Remembered || s.UserID != u.ID {
		t.Errorf("session A = %+v", s)
	}

	// Touch extends.
	later := now.Add(2 * time.Hour)
	if err := db.TouchSession(ctx, hashA, later, later); err != nil {
		t.Fatal(err)
	}
	s2, _ := db.SessionByHash(ctx, hashA)
	if s2.ExpiresAt.Unix() != later.Unix() || s2.LastSeenAt.Unix() != later.Unix() {
		t.Errorf("touch did not persist: %+v", s2)
	}

	// Password-change semantics: delete all except one.
	n, err := db.DeleteSessionsForUser(ctx, u.ID, hashA)
	if err != nil || n != 1 {
		t.Errorf("delete except: n=%d err=%v, want 1", n, err)
	}
	if s, _ := db.SessionByHash(ctx, hashB); s != nil {
		t.Error("session B should be revoked")
	}

	// Expiry purge.
	if err := db.TouchSession(ctx, hashA, now.Add(-time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if n, err := db.DeleteExpired(ctx, now); err != nil || n != 1 {
		t.Errorf("DeleteExpired n=%d err=%v, want 1", n, err)
	}
}

func TestSetPassword(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("p@x.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPassword(ctx, u.ID, "newhash", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := db.UserByID(ctx, u.ID)
	if got.PasswordHash != "newhash" {
		t.Errorf("hash not updated: %+v", got)
	}
	if err := db.SetPassword(ctx, "01NONEXISTENT00000000000000", "x", time.Now()); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user: got %v, want ErrUserNotFound", err)
	}
}

func TestSchemaUserStatusDefault(t *testing.T) {
	db := openTestDB(t)
	// The schema default must use the application's current account vocabulary.
	if _, err := db.sqlDB.Exec(`INSERT INTO users(id,email,name,password_hash,created_at,updated_at)
 VALUES('default-status','default@example.com','Default','hash',1,1)`); err != nil {
		t.Fatal(err)
	}
	user, err := db.UserByID(context.Background(), "default-status")
	if err != nil || user == nil || user.Status != "enabled" {
		t.Fatalf("default user=%+v err=%v", user, err)
	}
}

func TestMCPLoadingMigratesAndSurvivesReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v3.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{migrationV1SQL, migrationV2SQL, migrationV3SQL, "PRAGMA user_version = 3"} {
		if _, err := raw.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user := mkUser("mcp@example.com")
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	conv := &Conversation{UserID: user.ID}
	if err := db.CreateConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.LoadMCPServer(ctx, user.ID, conv.ID, "tasks"); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	saved, err := db.ConversationForUser(ctx, user.ID, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.LoadedMCP) != 1 || saved.LoadedMCP[0] != "tasks" {
		t.Fatalf("saved activation: %v", saved.LoadedMCP)
	}
}
