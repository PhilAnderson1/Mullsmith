// Package scratch manages per-user scratchpad directories (spec §2.3, §8).
// M1 covers provisioning; the path-safe operation helper arrives in M5.
package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"mullsmith/internal/id"
)

// errOverLimit ends a DirSizeExceeds walk the instant the sum crosses the
// line — the watchdog must stay cheap even over a pathological directory.
var errOverLimit = errors.New("scratch: over size limit")

// scanAbort decides how a WalkDir callback error affects a quota scan
// (audit2 finding 3, accounting bypass): an entry that vanished mid-walk
// (the user deleted it, a maintenance sweep collected it) contributes zero
// bytes and must NOT fail the scan — the race is routine and the bytes are
// genuinely gone. Every other error (unreadable directory after a hostile
// chmod 000, I/O failure, metadata we cannot read) means the total would be
// a guess, so it aborts: quota decisions must never trust an incomplete
// scan. Callers surface the failure; hiding bytes makes usage
// unverifiable, not free.
//
// d may be nil when err != nil; callers must check err before touching d.
func scanAbort(err error) error {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// DirSizeExceeds reports whether the regular-file bytes under the user
// root exceed limit, short-circuiting the walk as soon as the running
// total crosses it (audit finding 5: the code_run watchdog polls once per
// second while a hostile script floods new files into the tree).
//
// A non-nil error means usage is UNKNOWN — the result is not "false"
// (fail closed, audit2 finding 3): callers must refuse the operation they
// were about to authorize. A missing user root is the one incomplete tree
// that legitimately reports zero usage: a fresh user holds no bytes.
func DirSizeExceeds(root, userID string, limit int64) (bool, error) {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return false, err
	}
	var total int64
	var rootMissing bool
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				rootMissing = true
				return nil
			}
			return scanAbort(err)
		}
		if d.Type().IsRegular() {
			info, ierr := d.Info()
			if ierr != nil {
				return scanAbort(ierr) // metadata unreadable ⇒ total incomplete
			}
			total += info.Size()
			if total > limit {
				return errOverLimit
			}
		}
		return nil
	})
	if rootMissing {
		return false, nil
	}
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

// EnsureUserDirs creates uploads/, artifacts/ and work/ under the user root.
func EnsureUserDirs(root, userID string) error {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return err
	}
	for _, sub := range subdirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return fmt.Errorf("create scratchpad %s: %w", sub, err)
		}
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
	targets := []string{dir}
	for _, sub := range subdirs {
		targets = append(targets, filepath.Join(dir, sub))
	}
	if ownedByRoot(root) {
		targets = append([]string{root}, targets...)
	}
	for _, p := range targets {
		if err := os.Chown(p, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	return nil
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
// zero.
func DirSize(root, userID string) (int64, error) {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return 0, err
	}
	var total int64
	var rootMissing bool
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				rootMissing = true
				return nil
			}
			return scanAbort(err)
		}
		if d.Type().IsRegular() {
			info, ierr := d.Info()
			if ierr != nil {
				return scanAbort(ierr)
			}
			total += info.Size()
		}
		return nil
	})
	if rootMissing {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return total, nil
}
