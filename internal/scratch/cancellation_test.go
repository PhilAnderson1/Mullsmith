package scratch

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWalkStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	if err := EnsureUserDirs(root, testULID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, testULID, "work", name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	visited := 0
	err := Walk(ctx, root, testULID, "work", 100, func(string, fs.FileInfo) error { visited++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || visited != 1 {
		t.Fatalf("walk continued: visited=%d err=%v", visited, err)
	}
	if _, err := DirSize(ctx, root, testULID, 1000); !errors.Is(err, context.Canceled) {
		t.Fatalf("quota scan ignored cancellation: %v", err)
	}
	if _, err := DirSizeExceeds(ctx, root, testULID, 1<<20, 1000); !errors.Is(err, context.Canceled) {
		t.Fatalf("quota watchdog scan ignored cancellation: %v", err)
	}
}
