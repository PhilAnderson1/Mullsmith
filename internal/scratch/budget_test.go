// Astra finding 5 regressions: entry-count budgets. Empty files meter as
// ~zero bytes yet make every host walk expensive, so each host-side
// enumeration carries a finite entry budget — truncating (fs_list), erroring
// as "unknown" (quota scans, which the fail-closed callers already act on),
// capping snapshots (sweep), or refusing to plan (RemoveTree). Budgets are
// overridden to tiny values here; production defaults are in the source.
package scratch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// flood creates n empty files in dir (deterministic zero-padded names).
func flood(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%06d", i))
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadDirLimitTruncates(t *testing.T) {
	root := newRoot(t)
	work := filepath.Join(root, testULID, "work")
	flood(t, work, 7)

	entries, truncated, err := ReadDirLimit(root, testULID, "work", 5)
	if err != nil {
		t.Fatal(err)
	}
	// (*File).ReadDir returns directory order, not sorted — assert the
	// cap and the flag, and that the five names are distinct entries.
	if !truncated || len(entries) != 5 {
		t.Fatalf("capped listing = %d entries (truncated=%v), want 5/true", len(entries), truncated)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Name()] {
			t.Errorf("duplicate entry %s", e.Name())
		}
		seen[e.Name()] = true
	}
	entries, truncated, err = ReadDirLimit(root, testULID, "work", 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(entries) != 7 {
		t.Errorf("uncapped listing = %d entries (truncated=%v), want 7/none", len(entries), truncated)
	}
	if _, _, err := ReadDirLimit(root, testULID, "work", 0); err == nil {
		t.Error("limit 0 accepted")
	}
	// An empty directory is an empty listing, not io.EOF (the fs_list
	// path hit exactly this).
	entries, truncated, err = ReadDirLimit(root, testULID, "artifacts", 5)
	if err != nil || len(entries) != 0 || truncated {
		t.Errorf("empty dir = (%d, truncated=%v, %v), want (0, false, nil)", len(entries), truncated, err)
	}
}

func TestQuotaScanEntryBudget(t *testing.T) {
	old := scanEntryBudget
	scanEntryBudget = 8
	t.Cleanup(func() { scanEntryBudget = old })

	root := newRoot(t)
	flood(t, filepath.Join(root, testULID, "work"), 10) // + root & 3 subdirs > 8

	if _, err := DirSize(root, testULID); !errors.Is(err, errScanBudget) {
		t.Errorf("DirSize = %v, want errScanBudget", err)
	}
	// Unknown is NOT "within quota": the watchdog/write paths fail closed
	// on exactly this error.
	if over, err := DirSizeExceeds(root, testULID, 1<<30); err == nil || over {
		t.Errorf("DirSizeExceeds = (%v, %v), want (false, errScanBudget)", over, err)
	}

	// Budgets only bound the walk: raising it back reads the same tree
	// accurately, nothing was mutated.
	scanEntryBudget = 1000
	size, err := DirSize(root, testULID)
	if err != nil {
		t.Fatalf("after raising budget: %v", err)
	}
	if size != 0 { // flood files are empty; quota exposure is entry count
		t.Errorf("size = %d, want 0", size)
	}
}

func TestSweepReadBudgetCapsSnapshot(t *testing.T) {
	oldRead := sweepReadBudget
	sweepReadBudget = 5
	t.Cleanup(func() { sweepReadBudget = oldRead })

	root := newRoot(t)
	work := filepath.Join(root, testULID, "work")
	flood(t, work, 12)
	ents, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		sweepAged(t, filepath.Join(work, e.Name()))
	}

	var warnings []string
	now, maxAge := sweepNow(t)
	spec := SweepSpec{Now: now, FileMaxAge: maxAge, MaxFiles: 100,
		Warn: func(rel, note string) { warnings = append(warnings, rel+" "+note) }}

	st, err := SweepTree(root, testULID, "work", spec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 5 {
		t.Errorf("first pass deleted %d files, want exactly the read budget (5)", st.Files)
	}
	var sawTrunc bool
	for _, w := range warnings {
		if strings.Contains(w, "truncated") {
			sawTrunc = true
		}
	}
	if !sawTrunc {
		t.Errorf("no truncation warning in %v", warnings)
	}
	// The following passes finish the job — nothing is lost, only delayed.
	for i := 0; i < 5 && st.Files > 0; i++ {
		st, err = SweepTree(root, testULID, "work", spec)
		if err != nil {
			t.Fatal(err)
		}
	}
	ents, err = os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("%d files survived the bounded passes", len(ents))
	}
}

func TestRemoveTreeEntryBudget(t *testing.T) {
	old := removeTreeEntryBudget
	removeTreeEntryBudget = 5
	t.Cleanup(func() { removeTreeEntryBudget = old })

	root := newRoot(t)
	tree := filepath.Join(root, testULID, "work", "tree")
	if err := os.MkdirAll(filepath.Join(tree, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	flood(t, tree, 4)
	flood(t, filepath.Join(tree, "inner"), 4) // 11 entries > 5

	if err := RemoveTree(root, testULID, "work/tree"); err == nil ||
		!strings.Contains(err.Error(), "refusing") {
		t.Fatalf("RemoveTree = %v, want a refusal", err)
	}
	// Fail-closed contract: a refused plan deletes NOTHING.
	for _, rel := range []string{"f000000", "f000003", "inner", "inner/f000003"} {
		mustExist(t, filepath.Join(tree, rel))
	}

	// Under a workable budget the same tree deletes normally.
	removeTreeEntryBudget = 1000
	if err := RemoveTree(root, testULID, "work/tree"); err != nil {
		t.Fatalf("RemoveTree after raising budget: %v", err)
	}
	mustGone(t, tree)
}
