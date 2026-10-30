// Astra finding 2 regressions: the old string-based sweeper Lstat'd and
// Remove'd by absolute path, so renaming a checked parent aside and planting
// a symlink made the host delete THROUGH it. SweepTree is fd-pinned: swaps
// can neither redirect the walk nor a deletion. The interleaving is made
// deterministic by the Warn callback — entries arrive in sorted order, so a
// "0-trigger" symlink fires the swap before any victim entry is inspected.
package scratch

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func sweepNow(t *testing.T) (time.Time, time.Duration) {
	t.Helper()
	return time.Now(), 24 * time.Hour
}

func sweepAged(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s must exist: %v", path, err)
	}
}

func mustGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s must be gone (err=%v)", path, err)
	}
}

// swapUnder renames dirPath aside and points it at outside — the exact move
// a script races against the host inside its writable work/ bind.
func swapUnder(t *testing.T, dirPath, outside string) {
	t.Helper()
	if err := os.Rename(dirPath, dirPath+".keep"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dirPath); err != nil {
		t.Fatal(err)
	}
}

// File deletion must land in the REAL directory the walk pinned, never in
// the directory the swapped-in symlink now points to.
func TestSweepFileDeleteThroughParentSwap(t *testing.T) {
	root := newRoot(t)
	userDir := filepath.Join(root, testULID)
	outside := t.TempDir()

	agedFile := filepath.Join(outside, "victim.txt")
	os.WriteFile(agedFile, []byte("external"), 0o600)
	sweepAged(t, agedFile)

	if err := os.Mkdir(filepath.Join(userDir, "work", "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	realVictim := filepath.Join(userDir, "work", "a", "victim.txt")
	os.WriteFile(realVictim, []byte("in-tree"), 0o600)
	sweepAged(t, realVictim)
	os.Symlink(outside, filepath.Join(userDir, "work", "a", "0-trigger"))

	swapped := false
	now, age := sweepNow(t)
	_, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age,
		Warn: func(rel, note string) {
			if rel == "work/a/0-trigger" && !swapped {
				swapped = true
				swapUnder(t, filepath.Join(userDir, "work", "a"), outside)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Fatal("trigger never fired")
	}
	mustExist(t, agedFile)                                              // the host never deleted through the link
	mustGone(t, filepath.Join(userDir, "work", "a.keep", "victim.txt")) // pinned fd: real dir swept
}

// Empty-directory removal obeys the same rule: the rmdir attempts against
// the pinned parent, and neither the swapped parent nor a symlink standing
// in for a child may consume an external target.
func TestSweepEmptyDirDeleteThroughParentSwap(t *testing.T) {
	root := newRoot(t)
	userDir := filepath.Join(root, testULID)
	outside := t.TempDir()

	extDir := filepath.Join(outside, "emptydir")
	if err := os.Mkdir(extDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sweepAged(t, extDir)

	workA := filepath.Join(userDir, "work", "a")
	if err := os.Mkdir(workA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workA, "emptydir"), 0o700); err != nil {
		t.Fatal(err)
	}
	sweepAged(t, filepath.Join(workA, "emptydir"))
	os.Symlink(outside, filepath.Join(workA, "0-trigger"))

	swapped := false
	now, age := sweepNow(t)
	st, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, DirMaxAge: age,
		Warn: func(rel, note string) {
			if rel == "work/a/0-trigger" && !swapped {
				swapped = true
				swapUnder(t, workA, outside)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Fatal("trigger never fired")
	}
	mustExist(t, extDir) // external empty dir untouched…
	// …and the removal went to the real (renamed) directory's child.
	if _, err := os.Lstat(filepath.Join(userDir, "work", "a.keep", "emptydir")); !os.IsNotExist(err) {
		t.Error("real empty dir should have been rmdir'd through the pinned fd")
	}
	if st.Dirs != 1 {
		t.Errorf("Dirs = %d, want 1", st.Dirs)
	}
}

// A pre-existing symlink — planted before the pass — is reported and never
// traversed; an aged external tree behind it loses nothing.
func TestSweepDoesNotFollowPlantedSymlinks(t *testing.T) {
	root := newRoot(t)
	userDir := filepath.Join(root, testULID)
	outside := t.TempDir()

	victim := filepath.Join(outside, "precious.bin")
	os.WriteFile(victim, []byte("x"), 0o600)
	sweepAged(t, victim)
	os.Symlink(outside, filepath.Join(userDir, "work", "away"))

	now, age := sweepNow(t)
	var notes []string
	_, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, DirMaxAge: age,
		Warn: func(rel, note string) { notes = append(notes, rel+": "+note) },
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExist(t, victim)
	if len(notes) != 1 || !strings.Contains(notes[0], "symlink") {
		t.Errorf("expected one symlink warning, got %v", notes)
	}
}

// The pass must not even START through a symlinked user root.
func TestSweepRefusesSymlinkedUserRoot(t *testing.T) {
	root := newRoot(t)
	userDir := filepath.Join(root, testULID)
	outside := t.TempDir()

	victim := filepath.Join(outside, "precious.bin")
	os.WriteFile(victim, []byte("x"), 0o600)
	sweepAged(t, victim)

	if err := os.Rename(userDir, userDir+".keep"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, userDir); err != nil {
		t.Fatal(err)
	}
	now, age := sweepNow(t)
	if _, err := SweepTree(root, testULID, ".", SweepSpec{Now: now, FileMaxAge: age}); err == nil {
		t.Error("sweep must refuse a symlinked user root")
	}
	mustExist(t, victim)
}

// Budget semantics: the cap counts file deletions only, marks Capped, and
// leaves the rest standing; directories age untouched once the budget spent
// the traversal.
func TestSweepFileBudget(t *testing.T) {
	root := newRoot(t)
	userDir := filepath.Join(root, testULID)
	for i := 0; i < 5; i++ {
		p := filepath.Join(userDir, "work", string(rune('a'+i))+".txt")
		os.WriteFile(p, []byte("x"), 0o600)
		sweepAged(t, p)
	}
	now, age := sweepNow(t)
	st, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, DirMaxAge: age, MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 || !st.Capped {
		t.Errorf("stats = %+v, want 3 files, capped", st)
	}
	left := 0
	ents, _ := os.ReadDir(filepath.Join(userDir, "work"))
	for _, e := range ents {
		if !e.IsDir() {
			left++
		}
	}
	if left != 2 {
		t.Errorf("%d files left, want 2", left)
	}
}

// Astra2 finding 3: aged symlinks and special files age out like regular
// files — an entry the sweep could never delete would wall off everything
// ordered behind the first read batch, forever. Deletion must still touch
// only the link/special itself, never the target.
func TestSweepDeletesAgedSymlinksAndSpecials(t *testing.T) {
	root := newRoot(t)
	work := filepath.Join(root, testULID, "work")
	outside := t.TempDir()
	target := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	lf := filepath.Join(work, "link-file")
	ld := filepath.Join(work, "link-dir")
	os.Symlink(target, lf)
	os.Symlink(outside, ld)
	old := time.Now().Add(-48 * time.Hour)
	tv := []unix.Timeval{{Sec: int64(old.Unix())}, {Sec: int64(old.Unix())}}
	if err := unix.Lutimes(lf, tv); err != nil { // Chtimes would touch the TARGET
		t.Fatal(err)
	}
	if err := unix.Lutimes(ld, tv); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(work, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	sweepAged(t, pipe)
	fresh := filepath.Join(work, "fresh-link")
	os.Symlink(target, fresh) // young: left alone, as ever

	var warns []string
	now, age := sweepNow(t)
	st, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, MaxFiles: 100,
		Warn: func(rel, note string) { warns = append(warns, rel+": "+note) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 {
		t.Fatalf("stats = %+v, want 3 removals (2 links + fifo)", st)
	}
	mustGone(t, lf)
	mustGone(t, ld)
	mustGone(t, pipe)
	mustExist(t, fresh)
	mustExist(t, target) // the link's target survives its link
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Errorf("outside tree changed: %v", ents)
	}
	removed := 0
	for _, w := range warns {
		if strings.Contains(w, "aged symlink removed") {
			removed++
		}
	}
	if removed != 2 {
		t.Errorf("%d symlink-removal warnings, want 2 (audit trail): %v", removed, warns)
	}
}

// Astra2 finding 3: MaxEntries bounds the whole traversal, not one
// directory — a tree with nothing eligible for deletion can no longer
// consume an hour of walk work, and the capped remainder is swept once
// the budget lifts.
func TestSweepGlobalInspectionBudget(t *testing.T) {
	root := newRoot(t)
	work := filepath.Join(root, testULID, "work")
	if err := os.Mkdir(filepath.Join(work, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	p1 := filepath.Join(work, "old-a.bin")
	os.WriteFile(p1, []byte("x"), 0o600)
	sweepAged(t, p1)
	p2 := filepath.Join(work, "inner", "old-b.bin")
	os.WriteFile(p2, []byte("x"), 0o600)
	sweepAged(t, p2)
	for i := 0; i < 20; i++ { // young chaff: inspected, never deletable
		os.WriteFile(filepath.Join(work, string(rune('A'+i))+".tmp"), []byte("x"), 0o600)
	}

	now, age := sweepNow(t)
	st, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, MaxFiles: 100, MaxEntries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !st.EntriesCapped || st.Entries != 1 {
		t.Errorf("stats = %+v, want exactly 1 entry inspected then entries-capped", st)
	}
	if st.Files > 1 { // at most one deletion is reachable on one inspection
		t.Errorf("%d files deleted under a 1-entry budget", st.Files)
	}
	// The capped remainder waits, it is not lost: an uncapped pass clears
	// everything.
	if _, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, MaxFiles: 100,
	}); err != nil {
		t.Fatal(err)
	}
	mustGone(t, p1)
	mustGone(t, p2)
}

// Astra2 finding 3: descent is capped too — each level pins a descriptor,
// so a pathological depth cannot hold the pass open forever. Over-cap
// subtrees are reported and skipped; a workable cap sweeps them.
func TestSweepDepthCapSkipsDeepSubtree(t *testing.T) {
	oldD := sweepMaxDepth
	sweepMaxDepth = 2
	t.Cleanup(func() { sweepMaxDepth = oldD })

	root := newRoot(t)
	deep := filepath.Join(root, testULID, "work")
	for i := 0; i < 3; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(deep, "old.bin")
	os.WriteFile(p, []byte("x"), 0o600)
	sweepAged(t, p)

	var warns []string
	now, age := sweepNow(t)
	if _, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, MaxFiles: 100,
		Warn: func(rel, note string) { warns = append(warns, rel+": "+note) },
	}); err != nil {
		t.Fatal(err)
	}
	mustExist(t, p) // beyond the cap: skipped whole
	sawDepth := false
	for _, w := range warns {
		if strings.Contains(w, "deeper than the sweep depth cap") {
			sawDepth = true
		}
	}
	if !sawDepth {
		t.Errorf("no depth-cap warning in %v", warns)
	}
	sweepMaxDepth = 8
	if _, err := SweepTree(root, testULID, "work", SweepSpec{
		Now: now, FileMaxAge: age, MaxFiles: 100,
	}); err != nil {
		t.Fatal(err)
	}
	mustGone(t, p)
}
