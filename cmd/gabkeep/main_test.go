package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gabkeep/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "gabkeep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func putUser(t *testing.T, db *store.DB, email, pending, status string) *store.User {
	t.Helper()
	u := &store.User{
		Email:            email,
		Name:             "T",
		PasswordHash:     "x",
		Status:           status,
		UnconfirmedEmail: pending,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := db.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// --- list-users (plan §8): pending rows must be visible and nameable -----

func TestAdminListUsersShowsPendingRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	active := putUser(t, db, "shown@cli.io", "", "active")
	pending := putUser(t, db, "", "typo@cli.io", "disabled")

	var buf bytes.Buffer
	if code := adminListUsers(ctx, db, &buf, nil); code != 0 {
		t.Fatalf("list-users exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{
		active.ID + "  shown@cli.io  active",
		pending.ID + "  <pending: typo@cli.io>  disabled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list-users output missing %q:\n%s", want, out)
		}
	}
}

// --- delete-user: id names a pending row, email names a confirmed one ----

func TestAdminDeleteUserByIdAndEmail(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pending := putUser(t, db, "", "gone@cli.io", "disabled")
	active := putUser(t, db, "bye@cli.io", "", "active")

	// Pending sign-up: ONLY its id names it (the address lives in
	// unconfirmed_email, which no delete lookup may match).
	var buf bytes.Buffer
	if code := adminDeleteUser(ctx, db, &buf, []string{pending.ID}); code != 0 {
		t.Fatalf("delete pending by id = %d, want 0; out=%s", code, buf.String())
	}
	if u, _ := db.UserByID(ctx, pending.ID); u != nil {
		t.Error("pending row survived delete")
	}

	// Confirmed account by (uppercase-insensitive on storage) email.
	buf.Reset()
	if code := adminDeleteUser(ctx, db, &buf, []string{"bye@cli.io"}); code != 0 {
		t.Fatalf("delete by email = %d, want 0; out=%s", code, buf.String())
	}
	if u, _ := db.UserByEmail(ctx, "bye@cli.io"); u != nil {
		t.Error("confirmed row survived delete")
	}
	if !strings.Contains(buf.String(), "deleted user "+active.ID) {
		t.Errorf("delete output = %q", buf.String())
	}
}

func TestAdminDeleteUserUnknownTarget(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	confirmed := putUser(t, db, "keep@cli.io", "", "active")

	// Unknown id, and the pending address (deletable ONLY by id — naming
	// the pending address must miss, same invariant as login).
	pending := putUser(t, db, "", "wait@cli.io", "disabled")
	for _, target := range []string{"nosuchid", "wait@cli.io"} {
		var buf bytes.Buffer
		if code := adminDeleteUser(ctx, db, &buf, []string{target}); code == 0 {
			t.Errorf("delete %q = 0, want non-zero", target)
		}
	}
	// Nothing was deleted.
	for _, u := range []*store.User{confirmed, pending} {
		if got, _ := db.UserByID(ctx, u.ID); got == nil {
			t.Errorf("%s vanished after a refused delete", adminEmailLabel(*u))
		}
	}
	// No argument: usage error.
	if code := adminDeleteUser(ctx, db, &bytes.Buffer{}, nil); code != 2 {
		t.Errorf("delete-user with no target = %d, want 2", code)
	}
}
