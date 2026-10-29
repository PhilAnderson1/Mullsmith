package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mullsmith/internal/id"
)

// Background scratchpad lifecycle (spec §8). The disk is the single source
// of truth, so the sweep is pure mtime arithmetic — there is no registry
// left to consult, and the old "bytes without a row" orphan definition
// cannot even be expressed anymore. Per hourly pass (once at startup):
//
//  1. abandoned temps in the scratch ROOT: ".upload-*" (upload crash
//     between write and place) and ".share-*" (files_share staging) older
//     than orphanMinAge;
//  2. ULID user dirs with NO user behind them (deleted accounts): every
//     regular file older than orphanMinAge. This is exactly what the old
//     "rows cascaded away, bytes swept" mechanism achieved, now stated
//     directly;
//  3. per live user: work/ files older than work_ttl (the model's desk —
//     formalizes the age the orphan sweep already applied to unregistered
//     work files) and artifacts/ files older than artifact_ttl; uploads/
//     NEVER expires (user originals outlive every TTL);
//  4. empty subdirectories of work/ / artifacts/ whose own mtime is past
//     that directory's TTL, removed deepest-first — files vanish on their
//     own mtime, so without this the desk and the deliverables box fill
//     with empty folders. A folder holding one fresh file survives whole
//     (its rmdir fails on ENOTEMPTY and is retried next pass); sweeping
//     never reorganizes anything.
//
// Fail-closed stance (§9.7): anything unreadable is logged and left
// untouched — sweeping less is always safe, sweeping more is not. Each
// pass is bounded (spec §9.11): at most sweepPass deletions per directory.

const (
	maintenanceInterval = time.Hour
	orphanMinAge        = 24 * time.Hour // never race an in-flight upload/scan
	sweepPass           = 1000           // max deletions per directory per pass

	// pendingSignupTTL ages out never-confirmed sign-ups (disabled + NULL
	// email) from created_at; resends do NOT extend it. Equals the
	// confirmation token TTL so links and rows die together (plan §3).
	pendingSignupTTL = 24 * time.Hour
)

// RunMaintenance performs the file and auth-table sweeps immediately, then
// hourly until ctx ends. Safe to run as a goroutine. Signature unchanged
// from the registry era on purpose.
func (s *Server) RunMaintenance(ctx context.Context) {
	s.sweepScratchpad(ctx)
	s.sweepAuthDB(ctx)
	t := time.NewTicker(maintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepScratchpad(ctx)
			s.sweepAuthDB(ctx)
		}
	}
}

// sweepAuthDB prunes expired one-shot tokens and deletes pending sign-ups
// past their 24 h window (plan §3, §11.2 — dead data must not accumulate;
// the disabled+NULL-email predicate matches nothing else). No mail on
// expiry: the address was never proven deliverable.
func (s *Server) sweepAuthDB(ctx context.Context) {
	now := time.Now()
	if n, err := s.store.DeleteExpiredTokens(ctx, now); err != nil {
		s.logger.Warn("auth token sweep failed", "error", err)
	} else if n > 0 {
		s.logger.Info("purged expired auth tokens", "count", n)
	}
	if n, err := s.store.DeleteExpiredPendingUsers(ctx, now.Add(-pendingSignupTTL)); err != nil {
		s.logger.Warn("pending sign-up sweep failed", "error", err)
	} else if n > 0 {
		s.logger.Info("deleted expired unconfirmed sign-ups", "count", n)
	}
}

func (s *Server) sweepScratchpad(ctx context.Context) {
	now := time.Now()
	root := s.cfg.Scratchpad.Root

	live, err := s.store.UserIDs(ctx)
	if err != nil {
		// Fail closed: without the liveness set, every user dir could look
		// like a deleted account's. Nothing moves this pass.
		s.logger.Warn("sweep: user set lookup failed", "error", err)
		return
	}
	liveSet := make(map[string]bool, len(live))
	for _, u := range live {
		liveSet[u] = true
	}

	userDirs, err := os.ReadDir(root)
	if err != nil {
		s.logger.Warn("sweep: read scratch root", "error", err)
		return
	}

	var temps, orphanFiles int
	for _, de := range userDirs {
		name := de.Name()
		if strings.HasPrefix(name, ".upload-") || strings.HasPrefix(name, ".share-") {
			if s.removeIfOld(filepath.Join(root, name), now, orphanMinAge) {
				temps++
			}
			continue
		}
		// Only ULID directories are mullsmith's; anything else is left alone.
		if !de.IsDir() || !id.Valid(name) {
			continue
		}
		if !liveSet[name] {
			orphanFiles += s.sweepUnknownUser(filepath.Join(root, name), now)
			continue
		}
		// name passed id.Valid, so the join cannot escape the root.
		userDir := filepath.Join(root, name)
		// uploads/ is deliberately absent: user originals never expire.
		s.sweepTTL(userDir, "work", s.workTTL(), now)
		s.sweepTTL(userDir, "artifacts", s.artifactTTL(), now)
	}
	if temps > 0 || orphanFiles > 0 {
		s.logger.Info("scratchpad sweep", "temps_removed", temps, "orphan_files_removed", orphanFiles)
	}
}

// sweepUnknownUser removes every regular file older than orphanMinAge in
// a scratchpad directory with no user behind it. Empty directories are
// left alone: they cost nothing, and a re-registered ULID never happens
// anyway — they are simply too small to be worth a deletion rule.
func (s *Server) sweepUnknownUser(userDir string, now time.Time) int {
	removed := 0
	_ = filepath.WalkDir(userDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries never abort the walk
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			s.logger.Warn("symlink inside scratchpad tree (left alone)", "path", p)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if s.removeIfOld(p, now, orphanMinAge) {
			removed++
		}
		return nil
	})
	return removed
}

// sweepTTL ages one layout directory of one user: regular files with an
// mtime past ttl are unlinked (at any depth — each file ages on its own,
// sweeping never reorganizes), then empty subdirectories whose own mtime
// is past ttl are rmdir'd deepest-first. Removing a file bumps its parent
// directory's mtime, so a directory emptied this pass becomes rmdir-able
// once IT has aged — one TTL later, at the next pass that finds it still
// empty. Symlinks and special files are left alone with a warning; an
// unreadable directory is skipped, never partially swept.
func (s *Server) sweepTTL(userDir, dir string, ttl time.Duration, now time.Time) {
	base := filepath.Join(userDir, dir)
	cutoff := now.Add(-ttl)
	var dirs []string
	deleted := 0
	full := false
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			s.logger.Warn("sweep: unreadable entry left untouched", "path", p, "error", err)
			return nil
		}
		if d.IsDir() {
			if p != base {
				dirs = append(dirs, p)
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			s.logger.Warn("symlink inside scratchpad tree (left alone)", "path", p)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if deleted >= sweepPass {
			full = true
			return filepath.SkipAll
		}
		if s.removeIfOld(p, now, ttl) {
			deleted++
		}
		return nil
	})

	// Empty directories, deepest first (deepest paths sort last by length
	// when the separator is the only structural character — but do it by
	// component count to be exact).
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(filepath.Separator)) >
			strings.Count(dirs[j], string(filepath.Separator))
	})
	for _, d := range dirs {
		fi, err := os.Lstat(d) // never follow a symlink planted here
		if err != nil || !fi.IsDir() {
			continue
		}
		if fi.ModTime().After(cutoff) {
			continue // the folder itself has not aged out yet
		}
		if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) &&
			!errors.Is(err, syscall.ENOTEMPTY) {
			s.logger.Warn("sweep: rmdir failed", "path", d, "error", err)
		}
	}
	if deleted > 0 || full {
		s.logger.Info("ttl sweep", "dir", dir, "removed", deleted, "capped", full)
	}
}

// removeIfOld deletes p when its mtime is past maxAge; reports whether it
// deleted.
func (s *Server) removeIfOld(p string, now time.Time, maxAge time.Duration) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if now.Sub(fi.ModTime()) < maxAge {
		return false
	}
	if err := os.Remove(p); err != nil {
		s.logger.Warn("sweep remove failed", "path", p, "error", err)
		return false
	}
	return true
}
