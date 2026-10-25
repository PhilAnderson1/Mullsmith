package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testULID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := EnsureUserDirs(root, testULID); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestParseRelRejects(t *testing.T) {
	bad := []string{
		"",
		"/etc/passwd",
		"//etc/passwd",
		"..",
		"../x",
		"x/..",
		"a/../../etc/passwd",
		"a/..",
		"./..",
		"a/./../../etc",
		"..\\..\\windows\\win.ini",
		"foo\\bar",
		"C:/windows",
		"C:\\windows",
		"a\x00b",
		"a\nb",
		"a\tb",
		strings.Repeat("a", 256),       // segment too long
		strings.Repeat("a/", 40) + "b", // > 32 segments
		strings.Repeat("a", 5000),      // rel too long
		"valid/" + strings.Repeat("x", 300) + "/y",
	}
	for _, rel := range bad {
		if _, err := ParseRel(rel); !errors.Is(err, ErrPathUnsafe) {
			t.Errorf("ParseRel(%q) = %v, want ErrPathUnsafe", trunc(rel), err)
		}
	}
}

func TestParseRelAccepts(t *testing.T) {
	ok := map[string][]string{
		"a.txt":      {"a.txt"},
		"./a.txt":    {"a.txt"},
		"a//b":       {"a", "b"},
		"a/b/":       {"a", "b"},
		".":          {},
		"uploads/x":  {"uploads", "x"},
		"..stake":    {"..stake"}, // legal filename, not traversal
		"a...":       {"a..."},
		"dots/....":  {"dots", "...."},
		"spa ce/ü":   {"spa ce", "ü"},
		"sem;col$n":  {"sem;col$n"},
		"back`tick":  {"back`tick"},
		"%2e%2e/x":   {"%2e%2e", "x"}, // URL-encoding is literal here
		"..%2f..%2f": {"..%2f..%2f"},
	}
	for rel, want := range ok {
		got, err := ParseRel(rel)
		if err != nil {
			t.Errorf("ParseRel(%q) error: %v", rel, err)
			continue
		}
		if strings.Join(got, "/") != strings.Join(want, "/") {
			t.Errorf("ParseRel(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestResolveContainment(t *testing.T) {
	root := newRoot(t)
	for _, rel := range []string{".", "a", "a/b", "uploads/f.csv", "x/y/z"} {
		got, err := Resolve(root, testULID, rel)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", rel, err)
		}
		if !Within(filepath.Join(root, testULID), got) {
			t.Errorf("Resolve(%q) = %q escapes the user root", rel, got)
		}
	}
	// A non-ULID user id can never even address a directory.
	if _, err := Resolve(root, "../../etc", "passwd"); err == nil {
		t.Error("Resolve with malformed user id should fail")
	}
}

func TestFileOpsRoundTrip(t *testing.T) {
	root := newRoot(t)

	f, err := CreateFile(root, testULID, "uploads/reports/2026/q3.csv", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("a,b\n1,2\n")
	f.Close()

	got, err := OpenFile(root, testULID, "uploads/reports/2026/q3.csv")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, _ := got.Read(buf)
	got.Close()
	if string(buf[:n]) != "a,b\n1,2\n" {
		t.Errorf("read %q", buf[:n])
	}

	fi, err := Stat(root, testULID, "uploads/reports/2026/q3.csv")
	if err != nil || fi.Size() != 8 || fi.IsDir() {
		t.Errorf("Stat = %v, %v", fi, err)
	}

	entries, err := ReadDir(root, testULID, "uploads/reports/2026")
	if err != nil || len(entries) != 1 || entries[0].Name() != "q3.csv" {
		t.Errorf("ReadDir = %v, %v", entries, err)
	}

	if err := Remove(root, testULID, "uploads/reports/2026/q3.csv"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if _, err := OpenFile(root, testULID, "uploads/reports/2026/q3.csv"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after remove: %v, want ErrNotFound", err)
	}
}

func TestUserRootItself(t *testing.T) {
	root := newRoot(t)
	f, err := OpenDir(root, testULID, ".")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil || len(entries) != 3 { // uploads artifacts work
		t.Errorf("root ReadDir = %v, %v", entries, err)
	}
	if err := Remove(root, testULID, "."); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("Remove(.) = %v, want ErrPathUnsafe", err)
	}
}

func TestSymlinkEscapes(t *testing.T) {
	root := newRoot(t)
	userDir := filepath.Join(root, testULID)

	secret := filepath.Join(t.TempDir(), "victim.txt")
	os.WriteFile(secret, []byte("top secret"), 0o600)

	// 1. Final component is a symlink to a file outside the tree.
	os.Symlink(secret, filepath.Join(userDir, "uploads", "innocent.csv"))
	if _, err := OpenFile(root, testULID, "uploads/innocent.csv"); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("open symlinked file = %v, want ErrPathUnsafe", err)
	}
	if err := Remove(root, testULID, "uploads/innocent.csv"); err != nil {
		t.Errorf("unlink symlink: %v", err)
	}
	if _, err := os.Lstat(secret); err != nil {
		t.Error("Remove must unlink the link, not the target")
	}

	// 2. Intermediate directory is a symlink (final file exists in the
	//    target tree — the openat+O_NOFOLLOW chain must refuse to traverse).
	os.Symlink(filepath.Dir(secret), filepath.Join(userDir, "uploads", "loose"))
	if _, err := OpenFile(root, testULID, "uploads/loose/victim.txt"); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("open via symlinked dir = %v, want ErrPathUnsafe", err)
	}
	if _, err := CreateFile(root, testULID, "uploads/loose/planted.txt", 0o600); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("create via symlinked dir = %v, want ErrPathUnsafe", err)
	}

	// 3. MkdirAll must not walk through a symlink.
	os.Symlink(t.TempDir(), filepath.Join(userDir, "work", "away"))
	if err := MkdirAll(root, testULID, "work/away/deeper"); !errors.Is(err, ErrPathUnsafe) {
		t.Errorf("MkdirAll via symlink = %v, want ErrPathUnsafe", err)
	}

	// 4. Stat reports the link itself, never the target.
	os.Symlink(secret, filepath.Join(userDir, "link2"))
	fi, err := Stat(root, testULID, "link2")
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Stat on symlink = %v, %v", fi, err)
	}
}

func TestMkdirAllAgainstFile(t *testing.T) {
	root := newRoot(t)
	os.WriteFile(filepath.Join(root, testULID, "uploads", "plain"), []byte("x"), 0o600)
	if err := MkdirAll(root, testULID, "uploads/plain/inner"); !errors.Is(err, ErrNotDir) {
		t.Errorf("got %v, want ErrNotDir", err)
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// FuzzParseRel: whatever the input, success implies strict containment of
// Resolve's result — the property the whole sandbox rests on.
func FuzzPathContainment(f *testing.F) {
	seeds := []string{
		"a/b/c", "../x", "/etc", "..", ".", "uploads/f.csv",
		"a/../../b", "%2e%2e/", "\x00", "a\\..\\b", "//", "....//",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	root := "/srv/gabkeep/scratchpad"
	f.Fuzz(func(t *testing.T, rel string) {
		segs, err := ParseRel(rel)
		if err != nil {
			return
		}
		for _, s := range segs {
			if s == ".." || s == "." || s == "" || strings.ContainsAny(s, "/\x00") {
				t.Fatalf("ParseRel accepted unsafe segment %q from %q", s, rel)
			}
		}
		got, err := Resolve(root, testULID, rel)
		if err != nil {
			return
		}
		userRoot := filepath.Join(root, testULID)
		if !Within(userRoot, got) {
			t.Fatalf("Resolve(%q) = %q escapes %q", rel, got, userRoot)
		}
		// And the resolved path must survive re-parsing (idempotence).
		rel2, err := filepath.Rel(userRoot, got)
		if err != nil {
			return
		}
		if _, err := ParseRel(rel2); rel2 != "." && err != nil {
			t.Fatalf("re-parse of %q (from %q) failed: %v", rel2, rel, err)
		}
	})
}
