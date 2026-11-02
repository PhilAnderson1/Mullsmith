package scratch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageCountsAllEntriesInOneBoundedWalk(t *testing.T) {
	root := newRoot(t)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(root, testULID, "work", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, testULID, "work", "nested", "one"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, testULID, "uploads", "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, testULID, "artifacts", "link")); err != nil {
		t.Fatal(err)
	}
	usage, err := DirUsage(ctx, root, testULID, 7)
	if err != nil || usage.Entries != 7 || usage.Bytes != 1 {
		t.Fatalf("usage=%+v %v", usage, err)
	}
	if _, err := DirUsage(ctx, root, testULID, 6); !errors.Is(err, ErrEntryLimit) {
		t.Fatalf("smaller policy=%v", err)
	}
	// The smaller policy is local to its caller and does not alter this scan.
	if _, err := DirUsage(ctx, root, testULID, 7); err != nil {
		t.Fatal(err)
	}
}
