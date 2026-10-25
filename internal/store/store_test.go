package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gabkeep/internal/auth"
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
		Status:       "active",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db1.Close()
	if _, err := Open(path); err != nil {
		t.Fatalf("re-open (second migration pass): %v", err)
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

	list, err := db.ListInvites(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListInvites: %v %v", list, err)
	}
	if list[0].UsedBy != u.ID {
		t.Errorf("invite used_by = %q, want %q", list[0].UsedBy, u.ID)
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
