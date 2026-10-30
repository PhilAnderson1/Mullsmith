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
	FileMaxAge time.Duration          // unlink regular files at least this old
	DirMaxAge  time.Duration          // rmdir empty directories at least this old; ≤0 leaves directories alone
	MaxFiles   int                    // file-deletion budget per pass; ≤0 is unlimited
	Warn       func(rel, note string) // operator-visible: symlinks, unreadables; may be nil
}

// SweepStats reports what one pass did.
type SweepStats struct {
	Files  int
	Dirs   int
	Capped bool // the file budget was exhausted; untouched bytes wait for the next pass
}

// SweepTree prunes rel (a directory inside the user's scratchpad; "." is the
// user root) depth-first. For every regular file past FileMaxAge the file is
// unlinked; with DirMaxAge set, directories that are empty by the time the
// post-order visit reaches them and whose own mtime is past DirMaxAge are
// rmdir'd — a directory emptied during this pass has had its mtime bumped by
// the removals and therefore ages for the next one. Symlinks are reported
// and left alone; special files are left alone silently; an unreadable or
// vanished subtree is reported and skipped whole (fail-closed: sweeping less
// is always safe, sweeping more is not). Removing a file can shift further
// getdents positions, so a pass may miss entries — the next pass catches
// them, which is the same bounded-sweep doctrine the caller already applies.
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
	sweepDir(cur, rel, spec, &st)
	_ = cur.Close() // traversal fd; nothing was written through it
	return st, nil
}

// sweepReadBudget caps how many entries one sweep pass snapshots per
// directory (astra finding 5): a flooded directory must not force the
// hourly pass to allocate its whole listing. Deletions per directory per
// pass are capped at sweepPass anyway, so enumerating far beyond this buys
// nothing this hour — the next pass re-lists and continues. A var so tests
// can run small.
var sweepReadBudget = 20_000

// sweepDir processes one pinned directory. The listing is snapshotted before
// anything is deleted, so removals cannot shift getdents offsets under a
// half-read iteration. Children recurse in place: post-order means a
// directory's rmdir attempt happens after its last child was handled.
func sweepDir(dirf *os.File, dirRel string, spec SweepSpec, st *SweepStats) {
	if st.Capped {
		return
	}
	fd := int(dirf.Fd())
	// One extra read decides whether the listing was truncated, rather
	// than loading an unbounded directory to find out. ReadDir(n) reports
	// an empty directory as io.EOF — that is an empty listing, not an
	// unreadable one.
	entries, err := dirf.ReadDir(sweepReadBudget + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		warnf(spec, dirRel, "unreadable directory skipped: "+err.Error())
		return
	}
	if len(entries) > sweepReadBudget {
		entries = entries[:sweepReadBudget]
		warnf(spec, dirRel, "directory listing truncated this pass (more entries exist; next pass continues)")
	}
	for _, e := range entries {
		if spec.MaxFiles > 0 && st.Files >= spec.MaxFiles {
			st.Capped = true
			return
		}
		name := e.Name()
		sub := joinRel(dirRel, name)
		var fst unix.Stat_t
		if err := unix.Fstatat(fd, name, &fst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue // vanished between listing and inspection — the bytes are gone
		}
		switch fst.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			warnf(spec, sub, "symlink inside scratchpad tree (left alone)")
		case unix.S_IFDIR:
			if st.Capped {
				continue // budget spent: no further traversal work this pass
			}
			cfd, err := unix.Openat(fd, name,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				// Unreadable, vanished, or swapped for a symlink since the
				// fstatat — the fail-closed answer is the same for all:
				// skip this subtree whole.
				warnf(spec, sub, "unreadable directory skipped: "+err.Error())
				continue
			}
			child := os.NewFile(uintptr(cfd), name)
			sweepDir(child, sub, spec, st)
			_ = child.Close()
			if spec.DirMaxAge <= 0 {
				continue
			}
			// Re-inspect by name, then unlink relative to the SAME pinned
			// parent: whatever stands here at rmdir time is what goes —
			// never anything through a link.
			if err := unix.Fstatat(fd, name, &fst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				continue
			}
			if fst.Mode&unix.S_IFMT != unix.S_IFDIR || !agedPast(fst, spec.Now, spec.DirMaxAge) {
				continue
			}
			if err := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err != nil {
				// ENOTEMPTY: refilled (or never empty) — retried next pass.
				// ENOENT/ENOTDIR: vanished or swapped — nothing to do.
				if !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.ENOENT) &&
					!errors.Is(err, unix.ENOTDIR) {
					warnf(spec, sub, "rmdir failed: "+err.Error())
				}
				continue
			}
			st.Dirs++
		case unix.S_IFREG:
			if !agedPast(fst, spec.Now, spec.FileMaxAge) {
				continue
			}
			if err := unix.Unlinkat(fd, name, 0); err != nil {
				if !errors.Is(err, unix.ENOENT) {
					warnf(spec, sub, "remove failed: "+err.Error())
				}
				continue
			}
			st.Files++
		default:
			// FIFO/socket/device: left alone, as the string-based sweep did.
		}
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
