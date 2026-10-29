//go:build unix

package scratch

import (
	"os"
	"path/filepath"
	"syscall"
)

// serviceOwnerID returns the uid/gid of the deepest existing directory at
// or above root that is NOT owned by root. That owner is the identity the
// daemon writes as: the packaged installer creates the data directory
// (default /var/lib/mullsmith) as the service user, and the daemon creates
// everything below it itself. Skipping root-owned entries means directories
// a root-run CLI has just created do not poison the answer, and moving the
// scratchpad or changing the service user needs no config knowledge here —
// both change what the walk finds.
func serviceOwnerID(root string) (uid, gid int, ok bool) {
	for dir := filepath.Clean(root); ; {
		if info, err := os.Stat(dir); err == nil {
			if st, isStat := info.Sys().(*syscall.Stat_t); isStat && st.Uid != 0 {
				return int(st.Uid), int(st.Gid), true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, 0, false
		}
		dir = parent
	}
}

// ownedByRoot reports whether p exists and is root-owned — the state this
// package's root-run callers must repair rather than inherit.
func ownedByRoot(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}
