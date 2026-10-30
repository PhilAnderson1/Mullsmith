// Streaming, fd-pinned tree enumeration (astra2 finding 2). Quota scans and
// RemoveTree's delete plan used filepath.WalkDir, which loads AND SORTS a
// whole directory before the first callback — so a flooded directory forced
// one huge allocation no matter where the entry budget tripped. This walker
// instead reads directories in batches through held descriptors (os.File
// ReadDir(n): no sort, nothing beyond the batch alive at once) and descends
// with openat(O_NOFOLLOW) from the pinned user root, so enumeration is both
// bounded in memory and immune to the parent-swap redirection the string
// walks allowed.
//
// Accounting doctrine is unchanged (audit2 finding 3): a vanished entry
// contributes zero, anything else that cannot be inspected — unreadable
// metadata, unreadable directory, excessive depth — ABORTS with an error.
// Incomplete means unknown, and unknown is fail-closed upstream.
package scratch

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// scanBatch is how many directory entries one batched read delivers. Peak
// memory of a scan is O(scanBatch × depth) — the current batch stays
// alive while children recurse — never O(directory size); a var so
// tests can prove the walk does not depend on whole-directory reads.
var scanBatch = 512

// scanMaxDepth caps recursion (one pinned fd per level). Deeper than the
// cap is reported as unknown rather than trusted — same fail-closed answer
// as an unreadable corner. Generous: any legitimate tree stays far above
// everyday use and far below PATH_MAX/2. A var so tests can run small.
var scanMaxDepth = 512

// scanEntry is one enumerated filesystem entry, inspected by
// fstatat(AT_SYMLINK_NOFOLLOW) against the pinned parent directory.
type scanEntry struct {
	rel string // path relative to the user root, e.g. "work/a/b.bin"
	st  unix.Stat_t
}

// scanUserTree streams every entry under rel ("." is the user root),
// pre-order: a directory is visited before its contents. visit errors
// (including the caller's own short-circuit sentinels) end the scan and are
// returned unchanged. More than budget entries aborts with errScanBudget;
// exceeding scanMaxDepth, unreadable metadata or unreadable directories
// abort with their own error. A root (or rel) that does not exist yields a
// clean zero-visit scan — a fresh user holds no bytes.
func scanUserTree(root, userID, rel string, budget int, visit func(scanEntry) error) error {
	cur, err := openUserRoot(root, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	defer func() {
		if cur != nil {
			_ = cur.Close()
		}
	}()

	segs, perr := ParseRel(rel)
	if perr != nil {
		return perr
	}
	for _, s := range segs {
		nfd, err := unix.Openat(int(cur.Fd()), s,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = cur.Close()
		if err != nil {
			cur = nil // the target never opens; nothing further to close
			if errors.Is(err, unix.ENOENT) {
				return nil // target absent ⇒ nothing to count, nothing to plan
			}
			return mapOpenErr(err)
		}
		cur = os.NewFile(uintptr(nfd), s)
	}

	visited := 0
	var walk func(dirf *os.File, dirRel string, depth int) error
	walk = func(dirf *os.File, dirRel string, depth int) error {
		fd := int(dirf.Fd())
		for {
			entries, err := dirf.ReadDir(scanBatch)
			for _, e := range entries {
				if visited++; visited > budget {
					return fmt.Errorf("%w: over %d entries", errScanBudget, budget)
				}
				var st unix.Stat_t
				if err := unix.Fstatat(fd, e.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					if errors.Is(err, unix.ENOENT) {
						continue // vanished between listing and inspection — bytes are gone
					}
					return fmt.Errorf("scratch: scan cannot inspect entry: %w", err)
				}
				sub := joinRel(dirRel, e.Name())
				if err := visit(scanEntry{rel: sub, st: st}); err != nil {
					return err
				}
				if st.Mode&unix.S_IFMT != unix.S_IFDIR {
					continue // regular, symlink, special: leaves, as WalkDir treated them
				}
				if depth+1 > scanMaxDepth {
					return fmt.Errorf("scratch: tree deeper than %d directories, usage unknown", scanMaxDepth)
				}
				cfd, err := unix.Openat(fd, e.Name(),
					unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				if err != nil {
					if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) ||
						errors.Is(err, unix.ELOOP) {
						continue // raced into nothing/a link: contributes zero
					}
					return fmt.Errorf("scratch: scan cannot read directory: %w", err) // e.g. EACCES ⇒ unknown
				}
				child := os.NewFile(uintptr(cfd), e.Name())
				err = walk(child, sub, depth+1)
				_ = child.Close()
				if err != nil {
					return err
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil // drained this directory
				}
				return fmt.Errorf("scratch: scan cannot read directory: %w", err)
			}
		}
	}
	return walk(cur, rel, 0)
}
