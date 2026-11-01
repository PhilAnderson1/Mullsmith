package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mullsmith/internal/scratch"
	"mullsmith/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "mullsmith.db"))
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

// The default config path is the whole point of a "default install": bare
// `mullsmith` (and bare `mullsmith admin …`) must find /etc/mullsmith/config.yaml
// without being told (spec §2.1).
func TestDefaultConfigPath(t *testing.T) {
	if defaultConfigPath != "/etc/mullsmith/config.yaml" {
		t.Errorf("defaultConfigPath = %q, want /etc/mullsmith/config.yaml", defaultConfigPath)
	}
}

func TestAdminListUsersShowsPendingRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	active := putUser(t, db, "shown@cli.io", "", "enabled")
	pending := putUser(t, db, "", "typo@cli.io", "unconfirmed")

	var buf bytes.Buffer
	if code := adminListUsers(ctx, db, &buf, nil); code != 0 {
		t.Fatalf("list-users exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{
		active.ID + "  shown@cli.io  enabled",
		pending.ID + "  <pending: typo@cli.io>  unconfirmed",
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
	pending := putUser(t, db, "", "gone@cli.io", "unconfirmed")
	active := putUser(t, db, "bye@cli.io", "", "enabled")

	// Pending sign-up: ONLY its id names it (the address lives in
	// unconfirmed_email, which no delete lookup may match).
	var buf bytes.Buffer
	if code := adminDeleteUser(ctx, db, t.TempDir(), &buf, []string{pending.ID}); code != 0 {
		t.Fatalf("delete pending by id = %d, want 0; out=%s", code, buf.String())
	}
	if u, _ := db.UserByID(ctx, pending.ID); u != nil {
		t.Error("pending row survived delete")
	}

	// Confirmed account by (uppercase-insensitive on storage) email.
	buf.Reset()
	if code := adminDeleteUser(ctx, db, t.TempDir(), &buf, []string{"bye@cli.io"}); code != 0 {
		t.Fatalf("delete by email = %d, want 0; out=%s", code, buf.String())
	}
	if u, _ := db.UserByEmail(ctx, "bye@cli.io"); u != nil {
		t.Error("confirmed row survived delete")
	}
	if !strings.Contains(buf.String(), "deleted user "+active.ID) {
		t.Errorf("delete output = %q", buf.String())
	}
}

// The user's scratchpad tree goes with the row — not a day later with
// the unknown-user sweep.
func TestAdminDeleteUserRemovesScratchpad(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	u := putUser(t, db, "scratch@cli.io", "", "enabled")
	if err := scratch.EnsureUserDirs(root, u.ID); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := adminDeleteUser(ctx, db, root, &buf, []string{u.ID}); code != 0 {
		t.Fatalf("delete = %d; out=%s", code, buf.String())
	}
	if _, err := os.Stat(filepath.Join(root, u.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scratchpad tree survived delete (stat err = %v)", err)
	}
}

func TestAdminDeleteUserUnknownTarget(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	confirmed := putUser(t, db, "keep@cli.io", "", "enabled")

	// Unknown id, and the pending address (deletable ONLY by id — naming
	// the pending address must miss, same invariant as login).
	pending := putUser(t, db, "", "wait@cli.io", "unconfirmed")
	for _, target := range []string{"nosuchid", "wait@cli.io"} {
		var buf bytes.Buffer
		if code := adminDeleteUser(ctx, db, t.TempDir(), &buf, []string{target}); code == 0 {
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
	if code := adminDeleteUser(ctx, db, t.TempDir(), &bytes.Buffer{}, nil); code != 2 {
		t.Errorf("delete-user with no target = %d, want 2", code)
	}
}

// --- disable-user / enable-user: suspension round-trip and refusals ------

func TestAdminDisableEnableUser(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	u := putUser(t, db, "sus@cli.io", "", "enabled")

	var buf bytes.Buffer
	if code := adminSetSuspended(ctx, db, &buf, []string{"sus@cli.io"}, true); code != 0 {
		t.Fatalf("disable by email = %d, want 0; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "disabled user "+u.ID) {
		t.Errorf("disable output = %q", buf.String())
	}
	if got, _ := db.UserByID(ctx, u.ID); got.Status != store.StatusDisabled {
		t.Errorf("status after disable = %q", got.Status)
	}

	// Idempotence is a refusal the operator can read, not silence.
	buf.Reset()
	if code := adminSetSuspended(ctx, db, &buf, []string{u.ID}, true); code != 1 {
		t.Errorf("re-disable = %d, want 1", code)
	}
	if got, _ := db.UserByID(ctx, u.ID); got.Status != store.StatusDisabled {
		t.Errorf("re-disable changed status to %q", got.Status)
	}

	buf.Reset()
	if code := adminSetSuspended(ctx, db, &buf, []string{u.ID}, false); code != 0 {
		t.Fatalf("enable = %d, want 0; out=%s", code, buf.String())
	}
	if got, _ := db.UserByID(ctx, u.ID); got.Status != store.StatusEnabled {
		t.Errorf("status after enable = %q", got.Status)
	}
	if code := adminSetSuspended(ctx, db, &buf, []string{u.ID}, false); code != 1 {
		t.Errorf("enable of an enabled account = %d, want 1", code)
	}

	// Pending sign-ups are already unusable — suspending one is refused,
	// and enabling one must never happen (confirmation is the only door).
	pending := putUser(t, db, "", "wait2@cli.io", "unconfirmed")
	if code := adminSetSuspended(ctx, db, &buf, []string{pending.ID}, true); code != 1 {
		t.Errorf("disable of pending = %d, want 1", code)
	}
	if got, _ := db.UserByID(ctx, pending.ID); got.Status != store.StatusUnconfirmed {
		t.Errorf("pending status changed to %q", got.Status)
	}
	if code := adminSetSuspended(ctx, db, &buf, []string{pending.ID}, false); code != 1 {
		t.Errorf("enable of pending = %d, want 1", code)
	}

	if code := adminSetSuspended(ctx, db, &buf, []string{"nobody@cli.io"}, true); code != 1 {
		t.Errorf("unknown target = %d, want 1", code)
	}
}

// Every shutdown stage must live inside ONE shared grace deadline (review
// follow-up to issue 5): sequential WithTimeout(grace) calls gave each
// stage the full grace again, so the total could stretch to a multiple of
// the setting.
func TestShutdownStagesShareOneDeadline(t *testing.T) {
	const grace = 200 * time.Millisecond
	var (
		deadlines []time.Time
		expired   []bool
	)
	stage := func(burn time.Duration) func(context.Context) {
		return func(ctx context.Context) {
			dl, _ := ctx.Deadline()
			deadlines = append(deadlines, dl)
			time.Sleep(burn)
			expired = append(expired, ctx.Err() != nil)
		}
	}
	start := time.Now()
	shutdownWithin(grace,
		stage(grace+100*time.Millisecond), // overruns the shared budget
		stage(time.Millisecond),
		stage(time.Millisecond),
	)
	if d := time.Since(start); d > 2*grace {
		t.Errorf("total shutdown %v exceeded the shared grace %v", d, grace)
	}
	for i, dl := range deadlines {
		if !dl.Equal(deadlines[0]) {
			t.Errorf("stage %d saw deadline %v, want the shared %v", i, dl, deadlines[0])
		}
	}
	if !expired[1] || !expired[2] {
		t.Error("a stage starting after the shared deadline must inherit the expiry, not a fresh grace period")
	}
}
