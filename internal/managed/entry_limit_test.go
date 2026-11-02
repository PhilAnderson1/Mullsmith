package managed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"mullsmith/internal/id"
	"mullsmith/internal/scratch"
)

func entryFixture(t *testing.T, limit int) (*Service, string, string) {
	t.Helper()
	root, user := t.TempDir(), id.New()
	if err := scratch.EnsureUserDirs(root, user); err != nil {
		t.Fatal(err)
	}
	return New(1<<20, limit), root, user
}
func emptyEntryTemp(t *testing.T, root string) string {
	t.Helper()
	f, err := os.CreateTemp(root, ".upload-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

func TestConcurrentEntryAdmissionAndDeletionRecovery(t *testing.T) {
	s, root, user := entryFixture(t, 5)
	ctx := context.Background()
	if _, err := s.CommitUpload(ctx, root, user, emptyEntryTemp(t, root), "existing.txt", 0); err != nil {
		t.Fatal(err)
	}
	temps := []string{emptyEntryTemp(t, root), emptyEntryTemp(t, root)}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, temp := range temps {
		wg.Add(1)
		go func(temp string) {
			defer wg.Done()
			_, err := s.CommitUpload(ctx, root, user, temp, "empty.txt", 0)
			results <- err
		}(temp)
	}
	wg.Wait()
	close(results)
	accepted, refused := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, scratch.ErrEntryLimit) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("accepted=%d refused=%d", accepted, refused)
	}
	usage, err := scratch.DirUsage(ctx, root, user, 5)
	if err != nil || usage.Entries != 5 || usage.Bytes != 0 {
		t.Fatalf("usage=%+v %v", usage, err)
	}
	if err := scratch.Remove(root, user, "uploads/existing.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUpload(ctx, root, user, emptyEntryTemp(t, root), "after-delete.txt", 0); err != nil {
		t.Fatal(err)
	}
}

func TestEntryLimitAllowsReplacementAndRejectsMissingParents(t *testing.T) {
	s, root, user := entryFixture(t, 4)
	ctx := context.Background()
	if _, err := s.WriteText(ctx, root, user, "work/one.txt", "original"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteText(ctx, root, user, "work/one.txt", "replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteText(ctx, root, user, "work/new/sub/file.txt", ""); !errors.Is(err, scratch.ErrEntryLimit) {
		t.Fatalf("nested growth=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, user, "work", "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal created parents: %v", err)
	}
	if _, err := s.Share(ctx, root, user, "work/one.txt"); !errors.Is(err, scratch.ErrEntryLimit) {
		t.Fatalf("share at limit=%v", err)
	}
	if err := s.CommitDownload(ctx, root, user, emptyEntryTemp(t, root), "work/download.txt", 0); !errors.Is(err, scratch.ErrEntryLimit) {
		t.Fatalf("download at limit=%v", err)
	}
	usage, err := scratch.DirUsage(ctx, root, user, 4)
	if err != nil || usage.Entries != 4 {
		t.Fatalf("replacement left extra entries: %+v %v", usage, err)
	}
	b, err := os.ReadFile(filepath.Join(root, user, "work", "one.txt"))
	if err != nil || string(b) != "replacement" {
		t.Fatalf("replacement=%q %v", b, err)
	}
}

func TestStagingEmptyFilesSpendsEntriesAndReplacementDoesNot(t *testing.T) {
	s, root, user := entryFixture(t, 4)
	ctx := context.Background()
	if _, err := s.CommitUpload(ctx, root, user, emptyEntryTemp(t, root), "empty.txt", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(ctx, root, user, []string{"uploads/empty.txt"}); !errors.Is(err, scratch.ErrEntryLimit) {
		t.Fatalf("zero-byte staging=%v", err)
	}
	s = New(1<<20, 5)
	for i := 0; i < 2; i++ {
		if _, err := s.Stage(ctx, root, user, []string{"uploads/empty.txt", "uploads/empty.txt"}); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := scratch.DirUsage(ctx, root, user, 5)
	if err != nil || usage.Entries != 5 {
		t.Fatalf("stage usage=%+v %v", usage, err)
	}
}

func TestEntryWatchdogWorksWithoutByteQuota(t *testing.T) {
	s, root, user := entryFixture(t, 3)
	s.quota = 0
	if err := os.WriteFile(filepath.Join(root, user, "work", "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	over, err := s.Exceeds(context.Background(), root, user)
	if err != nil || !over {
		t.Fatalf("entry watchdog=%v %v", over, err)
	}
}
