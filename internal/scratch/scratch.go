// Package scratch manages per-user scratchpad directories (spec §2.3, §8).
// M1 covers provisioning; the path-safe operation helper arrives in M5.
package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gabkeep/internal/id"
)

// errOverLimit ends a DirSizeExceeds walk the instant the sum crosses the
// line — the watchdog must stay cheap even over a pathological directory.
var errOverLimit = errors.New("scratch: over size limit")

// DirSizeExceeds reports whether the regular-file bytes under the user root
// exceed limit, short-circuiting the walk as soon as the running total
// crosses it (audit finding 5: the code_run watchdog polls once per second
// while a hostile script floods new files into the tree).
func DirSizeExceeds(root, userID string, limit int64) (bool, error) {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return false, err
	}
	var total int64
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // same fail-soft policy as DirSize
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
				if total > limit {
					return errOverLimit
				}
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errOverLimit):
		return true, nil
	case err != nil && !os.IsNotExist(err):
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

// RemoveUserDirs deletes a user's scratchpad entirely.
func RemoveUserDirs(root, userID string) error {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// DirSize sums bytes under the user root (drives the /me quota figure).
func DirSize(root, userID string) (int64, error) {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return 0, err
	}
	var total int64
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries do not fail the quota estimate
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return total, nil
}
