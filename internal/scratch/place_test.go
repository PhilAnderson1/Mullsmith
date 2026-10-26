package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// mkTemp writes a host temp file in the scratch root (like the upload
// pipeline does) and returns its path.
func mkTemp(t *testing.T, root, content string) string {
	t.Helper()
	f, err := os.CreateTemp(root, ".upload-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestPlaceNoReplaceNewFile(t *testing.T) {
	root := newRoot(t)
	tmp := mkTemp(t, root, "payload")

	if err := PlaceNoReplace(root, testULID, "uploads/report.txt", tmp); err != nil {
		t.Fatalf("PlaceNoReplace: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, testULID, "uploads", "report.txt"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("placed content = %q, %v", got, err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp should be consumed by the rename, stat err = %v", err)
	}
}

func TestPlaceNoReplaceRefusesExisting(t *testing.T) {
	root := newRoot(t)
	if err := os.WriteFile(filepath.Join(root, testULID, "uploads", "a.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := mkTemp(t, root, "attacker")

	if err := PlaceNoReplace(root, testULID, "uploads/a.txt", tmp); !errors.Is(err, ErrExists) {
		t.Fatalf("PlaceNoReplace over existing = %v, want ErrExists", err)
	}
	// The existing bytes are untouched…
	got, _ := os.ReadFile(filepath.Join(root, testULID, "uploads", "a.txt"))
	if string(got) != "original" {
		t.Errorf("existing file was modified: %q", got)
	}
	// …and the caller keeps the temp for a bumped retry.
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("temp must survive a refused place: %v", err)
	}
}

func TestPlaceNoReplaceNestedAndHostile(t *testing.T) {
	root := newRoot(t)

	// Missing containing directory ⇒ ErrNotFound (callers MkdirAll first).
	if err := PlaceNoReplace(root, testULID, "work/deep/dir/f.txt", mkTemp(t, root, "x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing dir: %v, want ErrNotFound", err)
	}
	// The user root itself is never a placement target.
	if err := PlaceNoReplace(root, testULID, ".", mkTemp(t, root, "x")); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("user root: %v, want ErrPathUnsafe", err)
	}
	// A symlinked containing dir cannot be traversed (§9.7).
	outside := t.TempDir()
	link := filepath.Join(root, testULID, "uploads", "out")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := PlaceNoReplace(root, testULID, "uploads/out/f.txt", mkTemp(t, root, "x")); err == nil {
		t.Error("place through symlinked dir must fail")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Error("bytes landed outside the user tree")
	}
}

// TestPlaceNoReplaceBumpLoop exercises the exact loop the upload commit
// and files_share run: concurrent writers for one name must end up with
// distinct files, never an overwrite.
func TestPlaceNoReplaceBumpLoop(t *testing.T) {
	root := newRoot(t)
	placed := 0
	for i := 0; i < 5; i++ {
		tmp := mkTemp(t, root, "content")
		for n := 0; n < 200; n++ {
			rel := "uploads/" + BumpName("report.pdf", n)
			err := PlaceNoReplace(root, testULID, rel, tmp)
			if err == nil {
				placed++
				break
			}
			if !errors.Is(err, ErrExists) {
				t.Fatalf("PlaceNoReplace: %v", err)
			}
		}
	}
	if placed != 5 {
		t.Fatalf("placed %d files, want 5", placed)
	}
	for _, want := range []string{"report.pdf", "report (1).pdf", "report (2).pdf",
		"report (3).pdf", "report (4).pdf"} {
		if _, err := os.Stat(filepath.Join(root, testULID, "uploads", want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
}

func TestBumpName(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want string
	}{
		{"report.pdf", 1, "report (1).pdf"},
		{"archive.tar.gz", 1, "archive.tar (1).gz"},
		{"noext", 2, "noext (2)"},
		{".hidden", 3, ".hidden (3)"}, // leading dot is never an extension
		{"trailing.", 4, "trailing (4)."},
	}
	for _, c := range cases {
		if got := BumpName(c.name, c.n); got != c.want {
			t.Errorf("BumpName(%q, %d) = %q, want %q", c.name, c.n, got, c.want)
		}
	}
	if got := BumpName("x.txt", 0); got != "x.txt" {
		t.Errorf("BumpName n=0 must be identity, got %q", got)
	}
}
