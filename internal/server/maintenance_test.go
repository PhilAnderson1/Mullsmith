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
	s := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", nil)
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

func TestSweepExpiredArtifacts(t *testing.T) {
	s, root := maintServer(t)
	uid := maintUser(t, s.store, root)
	ctx := context.Background()

	put := func(rel string, expires time.Time) string {
		p := filepath.Join(root, uid, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("bytes"), 0o600)
		f := &store.File{ID: id.New(), UserID: uid, Kind: store.FileKindArtifact,
			RelPath: rel, MediaType: "text/plain", Name: filepath.Base(rel), Size: 5,
			SHA256: make([]byte, 32), CreatedAt: time.Now(), ExpiresAt: expires}
		if err := s.store.CreateFile(ctx, f); err != nil {
			t.Fatal(err)
		}
		return f.ID
	}
	expired := put("artifacts/old.txt", time.Now().Add(-time.Minute))
	fresh := put("artifacts/new.txt", time.Now().Add(time.Hour))

	// Uploads never expire.
	upRel := "uploads/keep.csv"
	os.WriteFile(filepath.Join(root, uid, upRel), []byte("bytes"), 0o600)
	s.store.CreateFile(ctx, &store.File{ID: id.New(), UserID: uid, Kind: store.FileKindUpload,
		RelPath: upRel, MediaType: "text/csv", Name: "keep.csv", Size: 5,
		SHA256: make([]byte, 32), CreatedAt: time.Now()})

	s.sweepScratchpad(ctx)

	if _, err := os.Stat(filepath.Join(root, uid, "artifacts/old.txt")); !os.IsNotExist(err) {
		t.Error("expired artifact bytes survived")
	}
	if _, err := s.store.GetFileForUser(ctx, uid, expired); err == nil {
		t.Error("expired artifact row survived")
	}
	if _, err := os.Stat(filepath.Join(root, uid, "artifacts/new.txt")); err != nil {
		t.Error("fresh artifact must survive")
	}
	if _, err := s.store.GetFileForUser(ctx, uid, fresh); err != nil {
		t.Error("fresh row must survive")
	}
	if _, err := os.Stat(filepath.Join(root, uid, "uploads/keep.csv")); err != nil {
		t.Error("upload must survive")
	}
}

func TestSweepOrphans(t *testing.T) {
	s, root := maintServer(t)
	uid := maintUser(t, s.store, root)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)

	// An orphan: bytes with no row (crash between rename and insert).
	orphan := filepath.Join(root, uid, "artifacts", "orphan.bin")
	os.WriteFile(orphan, []byte("x"), 0o600)
	os.Chtimes(orphan, old, old)

	// A fresh orphan inside work/ must wait out the 24 h safety gate.
	fresh := filepath.Join(root, uid, "work", "mid-upload.bin")
	os.WriteFile(fresh, []byte("x"), 0o600)

	// A row-backed file must never be touched.
	backed := filepath.Join(root, uid, "artifacts", "backed.txt")
	os.WriteFile(backed, []byte("x"), 0o600)
	s.store.CreateFile(ctx, &store.File{ID: id.New(), UserID: uid, Kind: store.FileKindArtifact,
		RelPath: "artifacts/backed.txt", MediaType: "text/plain", Name: "backed.txt",
		Size: 1, SHA256: make([]byte, 32), CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour)})

	// Abandoned upload temps live at the scratch root.
	tmpOld := filepath.Join(root, ".upload-crashed")
	os.WriteFile(tmpOld, []byte("x"), 0o600)
	os.Chtimes(tmpOld, old, old)
	tmpNew := filepath.Join(root, ".upload-active")
	os.WriteFile(tmpNew, []byte("x"), 0o600)

	// A non-ULID directory is operator territory: never swept.
	os.MkdirAll(filepath.Join(root, "not-a-user"), 0o700)
	os.WriteFile(filepath.Join(root, "not-a-user", "keep"), []byte("x"), 0o600)
	os.Chtimes(filepath.Join(root, "not-a-user", "keep"), old, old)

	s.sweepScratchpad(ctx)

	for _, p := range []string{orphan, tmpOld} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale orphan survived sweep: %s", p)
		}
	}
	for _, p := range []string{fresh, backed, filepath.Join(root, "not-a-user", "keep"), tmpNew} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("must survive sweep: %s (%v)", p, err)
		}
	}
}

func TestSweepSymlinkLeftAlone(t *testing.T) {
	s, root := maintServer(t)
	uid := maintUser(t, s.store, root)

	target := filepath.Join(t.TempDir(), "precious.txt")
	os.WriteFile(target, []byte("do not delete"), 0o600)
	link := filepath.Join(root, uid, "work", "stray.lnk")
	os.Symlink(target, link)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(link, old, old) // lstat-based mtime on the link itself

	s.sweepScratchpad(context.Background())

	if _, err := os.Lstat(link); err != nil {
		t.Error("sweep may not delete symlinks silently — operator must see it")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "do not delete" {
		t.Error("symlink target must not be touched")
	}
}
