// Astra finding 4 (runner side): jail layout sources are validated through
// pinned no-follow opens, so a symlinked scratch root or work path fails
// closed before any jail exists.
package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyJailLayout(t *testing.T) {
	sp := t.TempDir()
	if err := os.Mkdir(filepath.Join(sp, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyJailLayout(sp, "work"); err != nil {
		t.Errorf("clean layout refused: %v", err)
	}
	if err := verifyJailLayout(sp, "missing"); err == nil {
		t.Error("missing work dir accepted")
	}
	if err := verifyJailLayout(sp, "work/missing/deep"); err == nil {
		t.Error("missing deep work path accepted")
	}

	// Symlink standing in for the scratch root itself.
	elsewhere := t.TempDir()
	link := filepath.Join(elsewhere, "sp-link")
	if err := os.Symlink(sp, link); err != nil {
		t.Fatal(err)
	}
	if err := verifyJailLayout(link, "work"); err == nil {
		t.Error("symlinked scratch dir accepted")
	}

	// Symlinked intermediate component on the work path.
	if err := os.Symlink("/etc", filepath.Join(sp, "away")); err != nil {
		t.Fatal(err)
	}
	if err := verifyJailLayout(sp, "away/sub"); err == nil {
		t.Error("symlinked work path component accepted")
	}
	if err := verifyJailLayout(sp, "away"); err == nil {
		t.Error("symlink as the work dir accepted")
	}
}

// The refusal must reach callers through Run, not just the helper: a
// symlinked scratch dir never gets a jail built around it.
func TestRunRefusesSymlinkedScratchDir(t *testing.T) {
	if err := ProbeBwrap(false); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skipf("bwrap not found: %v", err)
	}
	real := t.TempDir()
	for _, sub := range []string{"uploads", "artifacts", "work"} {
		if err := os.Mkdir(filepath.Join(real, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "sp")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	b, err := NewBwrap(Options{BwrapBin: bin, Timeout: 5 * time.Second, GlobalCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Run(context.Background(), "01ARZ3NDEKTSV4RRFFQ69G5FAV", Request{
		Binary: "/bin/sh", ScratchDir: link, WorkDir: filepath.Join(link, "work"),
		Script: "true",
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Run with symlinked scratch dir = %v, want a symlink refusal", err)
	}
}
