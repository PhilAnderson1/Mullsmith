package managed

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mullsmith/internal/id"
	"mullsmith/internal/scratch"
)

func TestCancelledWriteWaitDoesNotModifyFile(t *testing.T) {
	root, user := t.TempDir(), id.New()
	if err := scratch.EnsureUserDirs(root, user); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, user, "work", "keep.txt")
	if err := os.WriteFile(dest, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	service := New(1<<20, 1000)
	if err := service.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := service.WriteText(ctx, root, user, "work/keep.txt", "replacement"); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled write remained behind the held gate")
	}
	content, err := os.ReadFile(dest)
	if err != nil || string(content) != "original" {
		t.Fatalf("original changed: %q %v", content, err)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
}

func TestCancelledDownloadCommitKeepsStagingFile(t *testing.T) {
	root, user := t.TempDir(), id.New()
	if err := scratch.EnsureUserDirs(root, user); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(root, ".download-test")
	if err := os.WriteFile(temp, []byte("download"), 0600); err != nil {
		t.Fatal(err)
	}
	service := New(1<<20, 1000)
	if err := service.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.release()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.CommitDownload(ctx, root, user, temp, "work/result.txt", 8) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("download commit ignored its deadline")
	}
	if _, err := os.Stat(filepath.Join(root, user, "work", "result.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists: %v", err)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("caller-owned staging file lost: %v", err)
	}
}

type cancellingReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	return copy(p, "chunk"), nil
}
func TestCopyStopsBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &cancellingReader{cancel: cancel}
	n, err := copyContext(ctx, io.Discard, source)
	if !errors.Is(err, context.Canceled) || source.reads != 1 || n != 5 {
		t.Fatalf("copy continued: bytes=%d reads=%d err=%v", n, source.reads, err)
	}
}
