// Astra2 finding 2 regressions: quota scans and RemoveTree's plan build
// stream through pinned descriptors in batches. Peak work is O(batch +
// depth), never O(directory); the accounting doctrine (vanished ⇒ zero,
// unreadable ⇒ unknown/fail-closed) is preserved.
package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestScanBatchesStreamCorrectly(t *testing.T) {
	old := scanBatch
	scanBatch = 4 // 30 files across three levels: many batches, none full-sized
	t.Cleanup(func() { scanBatch = old })

	root := newRoot(t)
	tree := filepath.Join(root, testULID, "work", "top", "inner")
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	// 30 files of 10 bytes: 10 at work/, 10 at work/top/, 10 nested.
	for _, dir := range []string{
		filepath.Join(root, testULID, "work"),
		filepath.Join(root, testULID, "work", "top"),
		tree,
	} {
		for i := 0; i < 10; i++ {
			p := filepath.Join(dir, string(rune('a'+i))+".bin")
			if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	size, err := DirSize(root, testULID)
	if err != nil || size != 300 {
		t.Fatalf("DirSize = %d (%v), want 300 across batched reads", size, err)
	}
	if over, err := DirSizeExceeds(root, testULID, 300); err != nil || over {
		t.Errorf("at-limit = (%v, %v), want (false, nil)", over, err)
	}
	if over, err := DirSizeExceeds(root, testULID, 299); err != nil || !over {
		t.Errorf("past-limit = (%v, %v), want (true, nil)", over, err)
	}
	// The budget trips mid-directory too, not just between directories.
	oldB := scanEntryBudget
	scanEntryBudget = 12
	t.Cleanup(func() { scanEntryBudget = oldB })
	if _, err := DirSize(root, testULID); !errors.Is(err, errScanBudget) {
		t.Errorf("DirSize under budget 12 = %v, want errScanBudget", err)
	}

	// A RemoveTree plan built in tiny batches still deletes everything.
	if err := RemoveTree(root, testULID, "work/top"); err != nil {
		t.Fatalf("RemoveTree batched: %v", err)
	}
	mustGone(t, filepath.Join(root, testULID, "work", "top"))
	ents, err := os.ReadDir(filepath.Join(root, testULID, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 10 {
		t.Errorf("%d files survived outside the deleted subtree, want 10", len(ents))
	}
}

func TestScanDepthLimitFailsClosed(t *testing.T) {
	oldD, oldB := scanMaxDepth, scanBatch
	scanMaxDepth, scanBatch = 3, 8
	t.Cleanup(func() { scanMaxDepth, scanBatch = oldD, oldB })

	root := newRoot(t)
	deep := filepath.Join(root, testULID, "work")
	for i := 0; i < 5; i++ { // depth 5 > cap 3
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "bottom.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DirSize(root, testULID); err == nil {
		t.Error("over-deep tree scanned clean; want fail-closed unknown")
	}
	scanMaxDepth = 8 // a workable cap reads the same tree accurately
	size, err := DirSize(root, testULID)
	if err != nil || size != 1 {
		t.Fatalf("after raising cap: %d (%v), want 1 byte", size, err)
	}
}

// Symlinks are entries with ~no metered bytes and never corridors — the
// WalkDir semantic, kept: the link is counted (budget), its target is not,
// and no descriptor is opened through it.
func TestScanDoesNotFollowSymlinks(t *testing.T) {
	root := newRoot(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "big.bin")
	if err := os.WriteFile(victim, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(outside, "dir")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "nested.bin"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, testULID, "work")
	os.Symlink(victim, filepath.Join(work, "link-file"))
	os.Symlink(outDir, filepath.Join(work, "link-dir"))

	size, err := DirSize(root, testULID)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Errorf("scan metered %d bytes through symlinks, want 0", size)
	}
}

// A user root substituted for a symlink refuses the scan as unknown —
// consistent with every other pinned operation (finding 4), rather than
// walking whatever tree the link addresses.
func TestScanRefusesSymlinkedUserRoot(t *testing.T) {
	root := t.TempDir()
	os.Symlink(t.TempDir(), filepath.Join(root, testULID))
	if _, err := DirSize(root, testULID); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("DirSize = %v, want ErrPathUnsafe", err)
	}
	if _, err := DirSizeExceeds(root, testULID, 1); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("DirSizeExceeds = %v, want ErrPathUnsafe", err)
	}
}
