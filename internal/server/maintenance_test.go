package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gabkeep/internal/id"
	"gabkeep/internal/scratch"
	"gabkeep/internal/store"
)

func maintServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := scratchCfg(t)
	db := testStore(t)
	s := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", nil, nil)
	return s, cfg.Scratchpad.Root
}

func maintUser(t *testing.T, db *store.DB, root string) string {
	t.Helper()
	u := &store.User{ID: id.New(), Email: "maint-" + id.New()[:8] + "@test.io", Name: "M",
		PasswordHash: "x", Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if err := scratch.EnsureUserDirs(root, u.ID); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func aged(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// The sweep is pure mtime arithmetic now (disk-as-truth, spec §8): work/
// ages at work_ttl, artifacts/ at artifact_ttl, uploads/ NEVER. Files at
// any depth age on their own mtime — sweeping never reorganizes.
func TestSweepTTLDirs(t *testing.T) {
	s, root := maintServer(t)
	uid := maintUser(t, s.store, root)
	workOld := 48 * time.Hour // past work_ttl (24h)
	artOld := 200 * time.Hour // past artifact_ttl (168h)

	put := func(rel string, age time.Duration) string {
		p := filepath.Join(root, uid, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		if age > 0 {
			aged(t, p, age)
		}
		return p
	}

	expiredArt := put("artifacts/old.txt", artOld)
	expiredDeep := put("artifacts/sub/deep-old.bin", artOld) // any depth
	freshArt := put("artifacts/new.txt", 0)
	expiredWork := put("work/stale.tmp", workOld)
	freshWork := put("work/live.tmp", 0)
	// Uploads outlive every TTL — user originals are never swept.
	oldUpload := put("uploads/ancient.csv", 4000*workOld)

	s.sweepScratchpad(context.Background())

	for _, p := range []string{expiredArt, expiredDeep, expiredWork} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("aged file survived sweep: %s", p)
		}
	}
	for _, p := range []string{freshArt, freshWork, oldUpload} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("must survive sweep: %s (%v)", p, err)
		}
	}
}

// Empty directories are the sweep's only directory duty: once emptied, a
// folder rmdir's when its OWN mtime passes the dir's TTL (file removals
// bump the parent mtime), deepest-first. A folder with fresh content, or
// one that has not aged, survives whole.
func TestSweepEmptyDirs(t *testing.T) {
	s, root := maintServer(t)
	uid := maintUser(t, s.store, root)
	old := 48 * time.Hour
	userWork := filepath.Join(root, uid, "work")

	// emptied-ago: files were deleted long ago; the dir's own mtime is old
	// and it is empty ⇒ removed. The nested child goes the same pass; the
	// PARENT's mtime is bumped by that removal, so it rmdir's only once it
	// has aged again — the pass-cadence the doc block promises.
	deep := filepath.Join(userWork, "stale-a", "stale-b")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	aged(t, deep, old)
	aged(t, filepath.Join(userWork, "stale-a"), old)

	// fresh-empty: empty but young ⇒ stays (it may be mid-use).
	if err := os.Mkdir(filepath.Join(userWork, "fresh-dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	// aged-but-full: old dir mtime, but a FRESH file inside ⇒ the sweep
	// deletes nothing here; the rmdir would ENOTEMPTY and is skipped.
	full := filepath.Join(userWork, "full-dir")
	if err := os.Mkdir(full, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "live.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	aged(t, full, old)

	s.sweepScratchpad(context.Background())

	if _, err := os.Stat(deep); !os.IsNotExist(err) {
		t.Error("aged empty nested dir survived")
	}
	// stale-a was only emptied THIS pass (its mtime just moved) — it must
	// wait out its own ageing, then go on a later pass.
	if _, err := os.Stat(filepath.Join(userWork, "stale-a")); err != nil {
		t.Errorf("parent dir removed the same pass it was emptied: %v", err)
	}
	aged(t, filepath.Join(userWork, "stale-a"), old)
	s.sweepScratchpad(context.Background())
	if _, err := os.Stat(filepath.Join(userWork, "stale-a")); !os.IsNotExist(err) {
		t.Error("emptied-then-aged parent dir must go on a later pass")
	}
	if _, err := os.Stat(filepath.Join(userWork, "fresh-dir")); err != nil {
		t.Errorf("young empty dir must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(full, "live.txt")); err != nil {
		t.Errorf("fresh file in aged dir must survive: %v", err)
	}
	// The dir's mtime was bumped by… nothing (the file is fresh, untouched
	// this pass), but rmdir on ENOTEMPTY is skipped regardless.
	if _, err := os.Stat(full); err != nil {
		t.Errorf("dir holding a fresh file was removed: %v", err)
	}

	// Second pass scenario: sweep emptied mid-old/a earlier; the now-empty
	// dir ages and a LATER pass removes it. Simulate by aging and re-run.
	mid := filepath.Join(userWork, "mid")
	if err := os.Mkdir(mid, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(mid, "file.bin")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	aged(t, f, old) // the FILE is aged; the dir is fresh (just created)
	s.sweepScratchpad(context.Background())
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("aged file must go")
	}
	if _, err := os.Stat(mid); err != nil {
		t.Error("just-emptied dir must survive its own un-aged mtime")
	}
	aged(t, mid, old) // time passes
	s.sweepScratchpad(context.Background())
	if _, err := os.Stat(mid); !os.IsNotExist(err) {
		t.Error("emptied-then-aged dir must go on a later pass")
	}
}

// Abandoned temps live in the scratch ROOT: ".upload-*" (crashed upload)
// and ".share-*" (files_share staging). Aged ones go; young ones never —
// they may belong to an in-flight request.
func TestSweepTemps(t *testing.T) {
	s, root := maintServer(t)
	maintUser(t, s.store, root)
	old := 48 * time.Hour

	tmpOld := filepath.Join(root, ".upload-crashed")
	os.WriteFile(tmpOld, []byte("x"), 0o600)
	aged(t, tmpOld, old)
	tmpShare := filepath.Join(root, ".share-abc123")
	os.WriteFile(tmpShare, []byte("x"), 0o600)
	aged(t, tmpShare, old)
	tmpNew := filepath.Join(root, ".upload-active")
	os.WriteFile(tmpNew, []byte("x"), 0o600)
	tmpShareNew := filepath.Join(root, ".share-justnow")
	os.WriteFile(tmpShareNew, []byte("x"), 0o600)

	s.sweepScratchpad(context.Background())

	for _, p := range []string{tmpOld, tmpShare} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("aged temp survived: %s", p)
		}
	}
	for _, p := range []string{tmpNew, tmpShareNew} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("in-flight-age temp must survive: %s (%v)", p, err)
		}
	}
}

// A ULID dir with no user behind it (deleted account) loses every file
// older than the 24h safety window; fresh bytes wait out the window.
// Non-ULID directories are operator territory: never swept.
func TestSweepUnknownUserDir(t *testing.T) {
	s, root := maintServer(t)
	live := maintUser(t, s.store, root) // keeps UserIDs() non-empty
	old := 48 * time.Hour

	ghost := id.New()
	if err := scratch.EnsureUserDirs(root, ghost); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ghost, "uploads", "old.bin")
	os.MkdirAll(filepath.Dir(stale), 0o700)
	os.WriteFile(stale, []byte("x"), 0o600)
	aged(t, stale, old)
	recent := filepath.Join(root, ghost, "artifacts", "recent.bin")
	os.WriteFile(recent, []byte("x"), 0o600)

	// The live user's aged artifact must NOT be touched by this rule
	// (it is governed by artifact_ttl, and it is fresh here anyway).
	liveArt := filepath.Join(root, live, "artifacts", "fresh.bin")
	os.WriteFile(liveArt, []byte("x"), 0o600)

	// A non-ULID directory is operator territory: never swept.
	os.MkdirAll(filepath.Join(root, "not-a-user"), 0o700)
	keep := filepath.Join(root, "not-a-user", "keep")
	os.WriteFile(keep, []byte("x"), 0o600)
	aged(t, keep, old)

	s.sweepScratchpad(context.Background())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("aged file in unknown-user dir survived")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("fresh file in unknown-user dir must survive the window: %v", err)
	}
	if _, err := os.Stat(liveArt); err != nil {
		t.Errorf("live user's bytes touched by the unknown-user rule: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-ULID dir was swept: %v", err)
	}
}

func TestSweepSymlinkLeftAlone(t *testing.T) {
	s, root := maintServer(t)
	uid := maintUser(t, s.store, root)

	target := filepath.Join(t.TempDir(), "precious.txt")
	os.WriteFile(target, []byte("do not delete"), 0o600)
	link := filepath.Join(root, uid, "work", "stray.lnk")
	os.Symlink(target, link)
	aged(t, link, 48*time.Hour)

	s.sweepScratchpad(context.Background())

	if _, err := os.Lstat(link); err != nil {
		t.Error("sweep may not delete symlinks silently — operator must see it")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "do not delete" {
		t.Error("symlink target must not be touched")
	}
}

// Fail-closed doctrine: if the liveness set cannot be read, EVERY user dir
// could look like a deleted account's — so nothing moves at all.
func TestSweepFailsClosedWithoutUserSet(t *testing.T) {
	s, root := maintServer(t)
	uid := maintUser(t, s.store, root)

	stale := filepath.Join(root, uid, "artifacts", "aged.bin")
	os.WriteFile(stale, []byte("x"), 0o600)
	aged(t, stale, 4000*48*time.Hour) // far past every TTL

	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	s.sweepScratchpad(context.Background())

	if _, err := os.Stat(stale); err != nil {
		t.Errorf("sweep ran without the user set and deleted bytes: %v", err)
	}
}
