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

func TestSweepFloodDirectoryDefersViaBudget(t *testing.T) {
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

	now, maxAge := sweepNow(t)
	spec := SweepSpec{Now: now, FileMaxAge: maxAge, MaxFiles: 100, MaxEntries: 5}

	st, err := SweepTree(root, testULID, "work", spec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 5 || !st.EntriesCapped || st.Entries != 5 {
		t.Errorf("first pass = %+v, want 5 files/5 entries/entries-capped", st)
	}
	// The following passes finish the job — nothing is lost, only delayed.
	total := st.Files
	for i := 0; i < 10 && st.Files > 0; i++ {
		st, err = SweepTree(root, testULID, "work", spec)
		if err != nil {
			t.Fatal(err)
		}
		total += st.Files
	}
	ents, err = os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 || total != 12 {
		t.Errorf("%d files survived (deleted %d total), want 0 survived / 12 deleted", len(ents), total)
	}
}

// Tiny batches across several depths still sweep everything. Deletions may
// shift unread getdents entries between batches, so completion is asserted
// across repeated passes (the documented doctrine), never a single pass.
func TestSweepTinyBatchesSweepDeepTrees(t *testing.T) {
	old := sweepBatch
	sweepBatch = 3
	t.Cleanup(func() { sweepBatch = old })

	root := newRoot(t)
	base := filepath.Join(root, testULID, "work")
	const nAged = 12
	for _, dir := range []string{base, filepath.Join(base, "sub1"), filepath.Join(base, "sub2", "deeper")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < nAged/3; i++ {
			p := filepath.Join(dir, string(rune('a'+i))+".bin")
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			sweepAged(t, p)
		}
	}
	// One young file must survive every pass.
	young := filepath.Join(base, "sub1", "fresh.bin")
	if err := os.WriteFile(young, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	now, maxAge := sweepNow(t)
	total := 0
	for i := 0; i < 10; i++ {
		st, err := SweepTree(root, testULID, "work", SweepSpec{
			Now: now, FileMaxAge: maxAge, DirMaxAge: maxAge, MaxFiles: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		total += st.Files
		if st.Files == 0 {
			break
		}
	}
	if total != nAged {
		t.Errorf("deleted %d aged files across passes, want %d", total, nAged)
	}
	mustExist(t, young)
	left, _ := os.ReadDir(filepath.Join(base, "sub2"))
	for _, e := range left {
		mustExist(t, filepath.Join(base, "sub2", e.Name()))
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
