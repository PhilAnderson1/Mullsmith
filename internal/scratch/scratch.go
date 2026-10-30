// Package scratch manages per-user scratchpad directories (spec §2.3, §8).
// M1 covers provisioning; the path-safe operation helper arrives in M5.
package scratch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"mullsmith/internal/id"
)

// errOverLimit ends a DirSizeExceeds walk the instant the sum crosses the
// line — the watchdog must stay cheap even over a pathological directory.
var errOverLimit = errors.New("scratch: over size limit")

// errScanBudget ends a quota scan that examined more entries than it may
// (astra finding 5): empty files count ~zero bytes, so a flood of entries
// makes every scan expensive while the quota looks nearly untouched.
var errScanBudget = errors.New("scratch: entry budget exceeded")

// scanEntryBudget caps how many filesystem entries one quota scan visits.
// Going over it is an ERROR, not a partial total — incomplete accounting
// is unknown accounting, and the fail-closed callers already act on that
// (run watchdog kills the run, fs_write refuses). Only a pathological tree
// gets near this; a var so tests can run small.
var scanEntryBudget = 50_000

// DirSizeExceeds reports whether the regular-file bytes under the user
// root exceed limit, short-circuiting the walk as soon as the running
// total crosses it (audit finding 5: the code_run watchdog polls once per
// second while a hostile script floods new files into the tree).
//
// A non-nil error means usage is UNKNOWN — the result is not "false"
// (fail closed, audit2 finding 3): callers must refuse the operation they
// were about to authorize. A missing user root is the one incomplete tree
// that legitimately reports zero usage: a fresh user holds no bytes.
// Walking more than scanEntryBudget entries is itself such an error
// (astra finding 5), so a flooded directory cannot burn CPU forever — and
// the scan streams through pinned descriptors in batches, so even the
// worst single directory cannot force a large allocation first (astra2
// finding 2).
func DirSizeExceeds(root, userID string, limit int64) (bool, error) {
	var total int64
	err := scanUserTree(root, userID, ".", scanEntryBudget, func(e scanEntry) error {
		if e.st.Mode&unix.S_IFMT == unix.S_IFREG {
			total += e.st.Size
			if total > limit {
				return errOverLimit
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errOverLimit):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// Subdirectories created for every user.
var subdirs = []string{"uploads", "artifacts", "work"}

// UserRoot returns the user's scratchpad root; rejects non-ULID ids so this
// function can never be pointed outside the tree (spec §9.7).
func UserRoot(root, userID string) (string, error) {
	if !id.Valid(userID) {
		return "", fmt.Errorf("invalid user id")
	}
	return filepath.Join(root, userID), nil
}

// EnsureUserDirs creates the user root and uploads/, artifacts/ and work/
// under it — fd-pinned (astra finding 4). The configured scratch root is
// opened by name once, the documented trust boundary; from there every step
// is a mkdirat against the pinned fd, re-opened no-follow to prove the
// created-or-existing element is a real directory. Provisioning therefore
// refuses to build beneath a planted symlink instead of shrugging it
// through, and a symlinked path aborts with ErrPathUnsafe.
func EnsureUserDirs(root, userID string) error {
	if _, err := UserRoot(root, userID); err != nil {
		return err
	}
	rootFd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open scratchpad root: %w", err)
	}
	defer unix.Close(rootFd)
	userFd, err := mkdiratDescend(rootFd, userID)
	if err != nil {
		return fmt.Errorf("create scratchpad: %w", err)
	}
	defer unix.Close(userFd)
	for _, sub := range subdirs {
		fd, err := mkdiratDescend(userFd, sub)
		if err != nil {
			return fmt.Errorf("create scratchpad %s: %w", sub, err)
		}
		_ = unix.Close(fd) // verification fd; nothing was read through it
	}
	return nil
}

// AdoptOwnership hands a freshly provisioned user tree to the owner a
// daemon run would have used. It exists for `mullsmith admin create-user`
// run as root: plain MkdirAll leaves the tree root-owned and the daemon,
// running unprivileged, cannot write it. A non-root caller is a no-op —
// its umask already produces the right owner.
//
// The daemon's identity is derived from the configured scratchpad root,
// never from a hardcoded path: serviceOwnerID takes the deepest existing
// directory at or above it that is not root-owned (for the packaged
// default, the installer-owned data directory). A root-owned scratchpad
// root itself — created moments ago, or by an earlier root CLI run — is
// repaired too, since it would block every later provisioning. The repair
// is non-recursive: it covers exactly the directories EnsureUserDirs
// makes. When nothing outside root's grip exists, an error is returned so
// the caller can warn instead of silently repeating the bug; the
// directories themselves were still created.
//
// Every chown goes through a pinned descriptor and refuses symlinked layout
// entries (astra finding 4); the ancestor walk that merely DISCOVERS the
// service identity stays name-based, because its subject — the trust
// boundary and above — is operator territory by definition.
func AdoptOwnership(root, userID string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	dir, err := UserRoot(root, userID)
	if err != nil {
		return err
	}
	uid, gid, ok := serviceOwnerID(root)
	if !ok {
		return fmt.Errorf("cannot tell which user runs the daemon: every directory at or above %s is owned by root", root)
	}

	// Collect the tree through the same pinned walk provisioning does, then
	// chown by descriptor (astra finding 4): with no name left to resolve,
	// a planted symlink cannot steer ownership onto an external tree.
	userF, err := openUserRoot(root, userID)
	if err != nil {
		return fmt.Errorf("adopt %s: %w", dir, err)
	}
	defer userF.Close()
	holds := []*os.File{userF}
	for _, sub := range subdirs {
		fd, err := unix.Openat(int(userF.Fd()), sub,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("adopt %s/%s: %w", dir, sub, mapOpenErr(err))
		}
		holds = append(holds, os.NewFile(uintptr(fd), sub))
	}
	defer func() {
		for _, f := range holds[1:] {
			_ = f.Close()
		}
	}()

	if ownedByRoot(root) {
		if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to chown %s: it is a symlink; repair its ownership manually", root)
		}
		rootF, err := openScratchRootDir(root)
		if err != nil {
			return fmt.Errorf("adopt %s: %w", root, err)
		}
		defer rootF.Close()
		if err := rootF.Chown(uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", root, err)
		}
	}
	for _, f := range holds {
		if err := f.Chown(uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", f.Name(), err)
		}
	}
	return nil
}

// openScratchRootDir opens the configured scratch root itself for the
// root-run ownership repair: no-follow on the final component (a symlinked
// scratch root is refused by the caller above), ordinary resolution of the
// parent path, consistent with the trust-boundary rule.
func openScratchRootDir(root string) (*os.File, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	return os.NewFile(uintptr(fd), root), nil
}

// RemoveUserDirs deletes a user's scratchpad entirely.
func RemoveUserDirs(root, userID string) error {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// DirSize sums bytes under the user root (drives the /me quota figure and
// every quota decision). A non-nil error means usage is UNKNOWN, not zero
// (fail closed, audit2 finding 3): an unreadable directory — a script's
// chmod 000 trick — or unreadable metadata aborts the walk instead of
// silently contributing nothing. A missing user root legitimately sums to
// zero. Like DirSizeExceeds, exceeding scanEntryBudget entries (or the
// depth cap) fails the scan as unknown, and the scan itself streams in
// batches through pinned descriptors (astra2 finding 2).
func DirSize(root, userID string) (int64, error) {
	var total int64
	err := scanUserTree(root, userID, ".", scanEntryBudget, func(e scanEntry) error {
		if e.st.Mode&unix.S_IFMT == unix.S_IFREG {
			total += e.st.Size
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
