package managed

import (
	"os"
	"path/filepath"
	"testing"

	"mullsmith/internal/id"
	"mullsmith/internal/scratch"
)

func TestStagePreservesSourceAliasesAndReplacesAtomically(t *testing.T) {
	root := t.TempDir()
	user := id.New()
	if err := scratch.EnsureUserDirs(root, user); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, user, "uploads", "input.csv")
	dest := filepath.Join(root, user, "work", "input.csv")
	const content = "valuable,data\n"
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, dest); err != nil {
		t.Fatal(err)
	}
	service := New(1 << 20)
	if _, err := service.Stage(root, user, []string{"uploads/input.csv", "work/input.csv"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{source, dest} {
		b, err := os.ReadFile(p)
		if err != nil || string(b) != content {
			t.Fatalf("source alias corrupted: %q %v", b, err)
		}
	}
	// A different source replaces the work entry, without truncating its
	// old inode (which is still owned by the original upload).
	other := filepath.Join(root, user, "artifacts", "input.csv")
	if err := os.WriteFile(other, []byte("new,data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Stage(root, user, []string{"artifacts/input.csv"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "new,data\n" {
		t.Fatalf("replacement=%q %v", b, err)
	}
	b, err = os.ReadFile(source)
	if err != nil || string(b) != content {
		t.Fatalf("upload corrupted=%q %v", b, err)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files leaked: %v %v", entries, err)
	}
}
