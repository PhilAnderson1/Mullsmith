// Fd-pinned tree sweeping (astra finding 2). TTL cleanup must never act on
// a path string: a script racing inside its own writable bind can rename a
// checked directory aside and plant a symlink in its place, and the host's
// Lstat-then-Remove by name would then delete THROUGH the link, outside the
// user's tree.
//
// SweepTree holds a pinned fd for every directory it descends (no-follow
// opens relative to the parent fd), inspects each entry with
// fstatat(AT_SYMLINK_NOFOLLOW) against that fd, and unlinks with unlinkat
// against the same fd. A swap during the pass can therefore neither redirect
// the walk nor steer a deletion outside the tree; unlinkat resolves exactly
// the named entry inside the pinned directory — a swapped-in symlink is
// unlinked as a link, never followed to its target. Age semantics stay
// best-effort as they were: between the age check and the unlink the entry
// at that name can be replaced, and the pass may then delete whatever stands
// there. The security property is WHERE deletion happens (inside this user's
// tree, at this one name), not which inode wins the race.
package scratch

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// SweepSpec configures one SweepTree pass.
type SweepSpec struct {
	Now        time.Time              // clock for the age arithmetic
	FileMaxAge time.Duration          // unlink regular files (and symlinks/specials) at least this old
	DirMaxAge  time.Duration          // rmdir empty directories at least this old; ≤0 leaves directories alone
	MaxFiles   int                    // file-deletion budget per pass; ≤0 is unlimited
	MaxEntries int                    // entry-inspection budget per pass; ≤0 is unlimited (astra2 finding 3)
	Warn       func(rel, note string) // operator-visible: symlinks, unreadables; may be nil
}

// SweepStats reports what one pass did.
type SweepStats struct {
	Files         int
	Dirs          int
	Entries       int // entries inspected before the pass ended
	Capped        bool // the file budget was exhausted; untouched bytes wait for the next pass
	EntriesCapped bool // the inspection budget was exhausted; the rest of the tree waits too
}

// sweepMaxDepth caps descent: every pinned parent stays open while its
// children are walked, so depth is also an fd budget. Beyond it a subtree is
// reported and skipped (fail-closed), matching how unreadable corners are
// handled. A var so tests can run small.
var sweepMaxDepth = 512

// SweepTree prunes rel (a directory inside the user's scratchpad; "." is the
// user root) depth-first. For every regular file past FileMaxAge the file is
// unlinked; with DirMaxAge set, directories that are empty by the time the
// post-order visit reaches them and whose own mtime is past DirMaxAge are
// rmdir'd — a directory emptied during this pass has had its mtime bumped by
// the removals and therefore ages for the next one. Symlinks and special
// files age out like regular files (astra2 finding 3: an entry the sweep
// could never delete would wall off, forever, everything ordered behind the
// first read batch). Safety is unchanged by that widening: unlinkat against
// a pinned parent removes a link as a link and a FIFO as a FIFO — the
// target is never opened, never followed. A YOUNG symlink is still reported
// and left alone, exactly as before. An unreadable or vanished subtree is
// reported and skipped whole (fail-closed: sweeping less is always safe,
// sweeping more is not). Removing a file can shift further getdents
// positions, so a pass may miss entries — the next pass catches them, which
// is the same bounded-sweep doctrine the caller already applies.
//
// Progress is BEST-EFFORT, not a guarantee (astra2 review): every pass
// restarts each directory listing from the beginning and no cursor or
// fairness state is kept — by design, the disk stays the single source of
// truth (spec §8). Entries ordered behind a permanently undeletable prefix
// (files a workload keeps young, an unreadable flood) can therefore starve
// until that prefix ages out or is removed; the spec accepts this as a
// self-scoped availability quirk under the code-execution trust model (§9.9).
//
// Two further bounds keep one pass cheap whatever the tree looks like
// (astra2 finding 3): the deletion budget MaxFiles counts unlinks, so an
// all-young tree never spends it — MaxEntries caps ENTRIES INSPECTED per
// pass, and sweepMaxDepth caps descent (one pinned fd per level). All
// leftovers are expected to be picked up by a later pass (best-effort —
// see the progress note above; nothing is ever deleted wrongly, and
// normally only timing is lost).
func SweepTree(root, userID, rel string, spec SweepSpec) (SweepStats, error) {
	segs, err := ParseRel(rel)
	if err != nil {
		return SweepStats{}, err
	}
	cur, err := openUserRoot(root, userID)
	if err != nil {
		return SweepStats{}, err
	}
	for _, s := range segs {
		nfd, err := unix.Openat(int(cur.Fd()), s,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = cur.Close()
			return SweepStats{}, mapOpenErr(err)
		}
		next := os.NewFile(uintptr(nfd), s)
		_ = cur.Close() // old fd is superseded; a close error leaks at most the fd
		cur = next
	}
	st := SweepStats{}
	sweepDir(cur, rel, 0, spec, &st)
	_ = cur.Close() // traversal fd; nothing was written through it
	return st, nil
}

// sweepBatch is how many directory entries one batched read delivers
// (astra2 review follow-up). Only the CURRENT batch stays alive while
// children recurse, so a deep tree costs O(sweepBatch × depth) retained
// entries — the previous whole-directory snapshot (20k) pinned a listing
// per level and could multiply into millions. Work remains bounded by the
// caller's MaxEntries, which is counted before each entry is touched, and
// a batch is tiny enough that read-ahead beyond the budget is trivial.
// A var so tests can prove correctness across batch boundaries.
var sweepBatch = 512

// sweepDir processes one pinned directory in batches. Deletions this pass
// can shift not-yet-read getdents entries between batches, so some entries
// may be missed until the next pass — the same bounded-sweep doctrine a
// half-read listing always implied: deletions happen by name against a
// pinned fd, never at a stale position, and duplicates are harmless
// (fstatat/unlinkat answer ENOENT). Children recurse in place: post-order
// means a directory's rmdir attempt happens after its last child was
// handled.
func sweepDir(dirf *os.File, dirRel string, depth int, spec SweepSpec, st *SweepStats) {
	if st.Capped || st.EntriesCapped {
		return
	}
	fd := int(dirf.Fd())
	for {
		entries, err := dirf.ReadDir(sweepBatch)
		for _, e := range entries {
			if spec.MaxFiles > 0 && st.Files >= spec.MaxFiles {
				st.Capped = true
				return
			}
			if spec.MaxEntries > 0 && st.Entries >= spec.MaxEntries {
				st.EntriesCapped = true
				return // inspection budget spent (astra2 finding 3): a later pass picks up (best-effort)
			}
			st.Entries++
			sweepEntry(fd, e.Name(), dirRel, depth, spec, st)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return // drained; an empty directory reads as io.EOF, not failure
			}
			warnf(spec, dirRel, "unreadable directory skipped: "+err.Error())
			return
		}
	}
}

// sweepEntry inspects and dispositions one entry of a pinned directory.
// Every deletion names the entry within the pinned parent — a link is
// unlinked as a link, never followed.
func sweepEntry(fd int, name, dirRel string, depth int, spec SweepSpec, st *SweepStats) {
	sub := joinRel(dirRel, name)
	var fst unix.Stat_t
	if err := unix.Fstatat(fd, name, &fst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return // vanished between listing and inspection — the bytes are gone
	}
	switch fst.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		if !agedPast(fst, spec.Now, spec.FileMaxAge) {
			warnf(spec, sub, "symlink inside scratchpad tree (left alone)")
			return
		}
		// Aged: unlink as a link — the target is never opened, never
		// followed, so this is no wider a power than file deletion
		// and removes the permanent blockage the sweep could never
		// clear before (astra2 finding 3).
		if err := unix.Unlinkat(fd, name, 0); err != nil {
			if !errors.Is(err, unix.ENOENT) {
				warnf(spec, sub, "remove symlink failed: "+err.Error())
			}
			return
		}
		warnf(spec, sub, "aged symlink removed")
		st.Files++
	case unix.S_IFDIR:
		if st.Capped || st.EntriesCapped {
			return // budget spent: no further traversal work this pass
		}
		if depth+1 > sweepMaxDepth {
			warnf(spec, sub, "subtree deeper than the sweep depth cap (skipped)")
			return
		}
		cfd, err := unix.Openat(fd, name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			// Unreadable, vanished, or swapped for a symlink since the
			// fstatat — the fail-closed answer is the same for all:
			// skip this subtree whole.
			warnf(spec, sub, "unreadable directory skipped: "+err.Error())
			return
		}
		child := os.NewFile(uintptr(cfd), name)
		sweepDir(child, sub, depth+1, spec, st)
		_ = child.Close()
		if spec.DirMaxAge <= 0 {
			return
		}
		// Re-inspect by name, then unlink relative to the SAME pinned
		// parent: whatever stands here at rmdir time is what goes —
		// never anything through a link.
		if err := unix.Fstatat(fd, name, &fst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return
		}
		if fst.Mode&unix.S_IFMT != unix.S_IFDIR || !agedPast(fst, spec.Now, spec.DirMaxAge) {
			return
		}
		if err := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err != nil {
			// ENOTEMPTY: refilled (or never empty) — retried next pass.
			// ENOENT/ENOTDIR: vanished or swapped — nothing to do.
			if !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.ENOENT) &&
				!errors.Is(err, unix.ENOTDIR) {
				warnf(spec, sub, "rmdir failed: "+err.Error())
			}
			return
		}
		st.Dirs++
	case unix.S_IFREG:
		if !agedPast(fst, spec.Now, spec.FileMaxAge) {
			return
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil {
			if !errors.Is(err, unix.ENOENT) {
				warnf(spec, sub, "remove failed: "+err.Error())
			}
			return
		}
		st.Files++
	default:
		// FIFO/socket/device: aged ones go too (astra2 finding 3 —
		// same permanent-blockage reasoning as symlinks). unlinkat
		// never opens the special file, so a FIFO cannot block the
		// sweep the way an open() would.
		if !agedPast(fst, spec.Now, spec.FileMaxAge) {
			return
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil {
			if !errors.Is(err, unix.ENOENT) {
				warnf(spec, sub, "remove special file failed: "+err.Error())
			}
			return
		}
		st.Files++
	}
}

func agedPast(fst unix.Stat_t, now time.Time, maxAge time.Duration) bool {
	mod := time.Unix(int64(fst.Mtim.Sec), int64(fst.Mtim.Nsec))
	return now.Sub(mod) >= maxAge
}

func joinRel(dirRel, name string) string {
	if dirRel == "" || dirRel == "." {
		return name
	}
	return dirRel + "/" + name
}

func warnf(spec SweepSpec, rel, note string) {
	if spec.Warn != nil {
		spec.Warn(rel, note)
	}
}
