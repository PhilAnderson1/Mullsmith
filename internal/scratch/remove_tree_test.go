package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// RemoveTree backs the Files page's folder delete. Same §9.7 guarantees as
// Remove for every element of the tree, plus: an unreadable subtree means
// NOTHING is deleted (an honest "everything inside is gone" promise).

func TestRemoveTreeNested(t *testing.T) {
	root := newRoot(t)
	for _, rel := range []string{
		"work/top/a.bin", "work/top/mid/leaf.txt", "work/sibling.txt",
	} {
		f, err := CreateFile(root, testULID, rel, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("x"))
		f.Close()
	}
	if err := MkdirAll(root, testULID, "work/top/emptydir"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(root, testULID, "work/top"); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	for _, gone := range []string{"work/top", "work/top/a.bin", "work/top/mid",
		"work/top/mid/leaf.txt", "work/top/emptydir"} {
		if _, err := Stat(root, testULID, gone); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s survived tree delete: %v", gone, err)
		}
	}
	if _, err := Stat(root, testULID, "work/sibling.txt"); err != nil {
		t.Errorf("outside the tree must survive: %v", err)
	}
}

// A symlink is a thing to unlink, never a corridor: RemoveTree must not
// traverse into (or delete anything within) targets outside the tree.
func TestRemoveTreeDoesNotFollowSymlinks(t *testing.T) {
	root := newRoot(t)
	// Victim lives OUTSIDE the scratchpad root entirely.
	outside := t.TempDir()
	victimDir := filepath.Join(outside, "precious")
	if err := os.Mkdir(victimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victimFile := filepath.Join(outside, "payload.bin")
	if err := os.WriteFile(victimFile, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := CreateFile(root, testULID, "work/tree/real.txt", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("y"))
	f.Close()
	if err := os.Symlink(victimDir, filepath.Join(root, testULID, "work", "tree", "escape-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, filepath.Join(root, testULID, "work", "tree", "escape.bin")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveTree(root, testULID, "work/tree"); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := Stat(root, testULID, "work/tree"); !errors.Is(err, ErrNotFound) {
		t.Errorf("tree root survived: %v", err)
	}
	if entries, err := os.ReadDir(victimDir); err != nil || len(entries) != 0 {
		t.Errorf("symlinked-out directory was traversed: %v (%v)", entries, err)
	}
	if _, err := os.Stat(victimFile); err != nil {
		t.Errorf("symlink target file outside root was deleted: %v", err)
	}
}

func TestRemoveTreeMissingAndNonDir(t *testing.T) {
	root := newRoot(t)
	if err := RemoveTree(root, testULID, "work/ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tree = %v, want ErrNotFound", err)
	}
	f, err := CreateFile(root, testULID, "work/plain.txt", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := RemoveTree(root, testULID, "work/plain.txt"); !errors.Is(err, ErrNotDir) {
		t.Errorf("file target = %v, want ErrNotDir", err)
	}
	if _, err := Stat(root, testULID, "work/plain.txt"); err != nil {
		t.Errorf("refused target must be untouched: %v", err)
	}
}

// The fail-closed counterpart of the quota-scan fix: if any directory in
// the tree cannot be listed, RemoveTree refuses with NOTHING deleted —
// half-cutting a tree while claiming full removal would hide survivors.
func TestRemoveTreeRefusesUnreadableSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; needs an unprivileged reader")
	}
	root := newRoot(t)
	f, err := CreateFile(root, testULID, "work/t/sub/hidden.bin", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("z"))
	f.Close()
	if err := MkdirAll(root, testULID, "work/t/ok"); err != nil {
		t.Fatal(err)
	}
	blinded := filepath.Join(root, testULID, "work", "t", "sub")
	if err := os.Chmod(blinded, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blinded, 0o700) })

	if err := RemoveTree(root, testULID, "work/t"); err == nil {
		t.Fatal("RemoveTree over an unreadable subtree returned nil; want refusal")
	}
	// Nothing deleted — visible siblings still there, whole tree intact.
	for _, kept := range []string{"work/t", "work/t/ok", "work/t/sub"} {
		if _, err := os.Stat(filepath.Join(root, testULID, kept)); err != nil {
			t.Errorf("%s should be untouched after refusal: %v", kept, err)
		}
	}
	// Restore access → the same delete now succeeds completely.
	if err := os.Chmod(blinded, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(root, testULID, "work/t"); err != nil {
		t.Fatalf("RemoveTree after restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, testULID, "work/t")); !os.IsNotExist(err) {
		t.Errorf("tree survived post-restore delete: %v", err)
	}
}
