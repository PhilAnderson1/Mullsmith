package scratch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// audit2 finding 3 (accounting bypass): a script that chmods a directory
// 000 used to make its bytes invisible AND free — walks skipped what they
// could not read. Quota scans must fail CLOSED instead: an incomplete scan
// returns an error so callers refuse to authorize anything.

// requireReadableAsNonRoot skips the test when the reader is root:
// CAP_DAC_OVERRIDE walks straight through chmod 000, so the scenario would
// assert nothing.
func requireReadableAsNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; unreadable-scan tests need an unprivileged reader")
	}
}

func TestDirSizeFailsClosedOnUnreadableDir(t *testing.T) {
	requireReadableAsNonRoot(t)
	root := newRoot(t)
	hidden := filepath.Join(root, testULID, "work", "hidden")
	if err := os.MkdirAll(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "secret.bin"), bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o700) }) // t.TempDir could not remove it otherwise

	if _, err := DirSize(context.Background(), root, testULID, 1000); err == nil {
		t.Error("DirSize returned nil error over an unreadable directory; want fail closed")
	}
	if over, err := DirSizeExceeds(context.Background(), root, testULID, 1<<30, 1000); err == nil {
		t.Errorf("DirSizeExceeds returned (%v, nil) over an unreadable directory; want an error, not 'under quota'", over)
	}

	// Restoring access restores accurate accounting (no state to heal —
	// the bytes were never deleted or touched).
	if err := os.Chmod(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	size, err := DirSize(context.Background(), root, testULID, 1000)
	if err != nil {
		t.Fatalf("after restore: %v", err)
	}
	if size < 4096 {
		t.Errorf("size = %d, want ≥ 4096 (the hidden file must count)", size)
	}
	if over, err := DirSizeExceeds(context.Background(), root, testULID, size+1, 1000); err != nil || over {
		t.Errorf("restored scan within quota = (%v, %v)", over, err)
	}
	if over, err := DirSizeExceeds(context.Background(), root, testULID, size-1, 1000); err != nil || !over {
		t.Errorf("restored scan past quota = (%v, %v), want (true, nil)", over, err)
	}
}

// Entries vanishing mid-walk (user delete, maintenance sweep) are not an
// accounting error — the bytes are gone; their contribution is zero. This
// must NOT regress with the fail-closed change, or routine races would
// cancel live runs.
func TestDirSizeToleratesVanishingEntries(t *testing.T) {
	root := newRoot(t)
	for _, rel := range []string{"work/a.bin", "work/b.bin", "work/c.bin"} {
		f, err := CreateFile(root, testULID, rel, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(bytes.Repeat([]byte("y"), 100))
		f.Close()
	}
	size, err := DirSize(context.Background(), root, testULID, 1000)
	if err != nil || size < 300 {
		t.Fatalf("baseline = %d (%v)", size, err)
	}
	if err := os.RemoveAll(filepath.Join(root, testULID, "work")); err != nil {
		t.Fatal(err)
	}
	if size, err := DirSize(context.Background(), root, testULID, 1000); err != nil {
		t.Errorf("scan after concurrent-style deletion errored: %v", err)
	} else if size > 200 { // artifacts/ + uploads/ are empty dirs here
		t.Errorf("size = %d after work/ removal, want the deleted bytes gone", size)
	}
	// A user root that never existed legitimately reports zero usage.
	if size, err := DirSize(context.Background(), root, "01ARZ3NDEKTSV4RRFFQ69G5FAW", 1000); err != nil || size != 0 {
		t.Errorf("missing root = (%d, %v), want (0, nil)", size, err)
	}
	if over, err := DirSizeExceeds(context.Background(), root, "01ARZ3NDEKTSV4RRFFQ69G5FAW", 1, 1000); err != nil || over {
		t.Errorf("missing root exceeds = (%v, %v), want (false, nil)", over, err)
	}
}
