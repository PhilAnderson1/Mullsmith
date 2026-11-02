package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mullsmith/internal/id"
	"mullsmith/internal/scratch"
	"mullsmith/internal/store"
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
//     that directory's TTL, removed depth-first after their contents —
//     files vanish on their own mtime, so without this the desk and the
//     deliverables box fill with empty folders. A folder holding one fresh
//     file survives whole (its rmdir fails on ENOTEMPTY and is retried next
//     pass); sweeping never reorganizes anything.
//
// Inside user trees every deletion goes through scratch.SweepTree, which is
// fd-pinned end to end (astra finding 2): entries are inspected with
// no-follow fstatat and unlinked with unlinkat against the pinned parent,
// so a directory swapped for a symlink by a racing script can neither
// redirect the walk nor make the host delete outside the user's tree. Only
// the scratch-ROOT temps still use name-based removal, and the root's only
// writer is the daemon itself (§9.7).
//
// Fail-closed stance (§9.7): anything unreadable is logged and left
// untouched — sweeping less is always safe, sweeping more is not. Each
// pass is bounded (spec §9.11): at most sweepPass deletions and at most
// sweepInspections entry inspections per sweep.

const (
	maintenanceInterval = time.Hour
	orphanMinAge        = 24 * time.Hour // never race an in-flight upload/scan
	sweepPass           = 1000           // max deletions per directory per pass
	sweepInspections    = 200_000        // max entries inspected per sweep pass (astra2 finding 3)

)

// RunMaintenance performs the file and auth-table sweeps immediately, then
// hourly until ctx ends. Safe to run as a goroutine. Signature unchanged
// from the registry era on purpose.
func (s *Server) RunMaintenance(ctx context.Context) {
	t := time.NewTicker(maintenanceInterval)
	defer t.Stop()
	s.runMaintenance(ctx, t.C)
}

// runMaintenance owns the observable loop contract while RunMaintenance owns
// only the real clock. Keeping the tick source injectable makes the startup
// pass, repeated passes and cancellation testable without sleeping for an
// hour or weakening the production interval.
func (s *Server) runMaintenance(ctx context.Context, ticks <-chan time.Time) {
	s.sweepScratchpad(ctx)
	s.sweepAuthDB(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			s.sweepScratchpad(ctx)
			s.sweepAuthDB(ctx)
		}
	}
}

// sweepAuthDB clears expired email-change reservations, prunes expired tokens
// and deletes pending sign-ups
// past their 24 h window (plan §3, §11.2 — dead data must not accumulate;
// the unconfirmed+NULL-email predicate matches nothing else). No mail on
// expiry: the address was never proven deliverable.
func (s *Server) sweepAuthDB(ctx context.Context) {
	now := time.Now()
	if n, err := s.store.DeleteExpiredEmailChanges(ctx, now); err != nil {
		s.logger.Warn("pending email-change sweep failed", "error", err)
	} else if n > 0 {
		s.logger.Info("cleared expired email changes", "count", n)
	}
	if n, err := s.store.DeleteExpiredTokens(ctx, now); err != nil {
		s.logger.Warn("auth token sweep failed", "error", err)
	} else if n > 0 {
		s.logger.Info("purged expired auth tokens", "count", n)
	}
	if n, err := s.store.DeleteExpiredPendingUsers(ctx, now.Add(-store.ConfirmationTTL)); err != nil {
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
			orphanFiles += s.sweepUnknownUser(root, name, now)
			continue
		}
		// name passed id.Valid, so the join cannot escape the root.
		// uploads/ is deliberately absent: user originals never expire.
		s.sweepTTL(root, name, "work", s.workTTL(), now)
		s.sweepTTL(root, name, "artifacts", s.artifactTTL(), now)
	}
	if temps > 0 || orphanFiles > 0 {
		s.logger.Info("scratchpad sweep", "temps_removed", temps, "orphan_files_removed", orphanFiles)
	}
}

// sweepWarn renders a SweepTree note with the absolute path the operator
// saw before the fd-pinned rewrite.
func (s *Server) sweepWarn(root, userID string) func(rel, note string) {
	return func(rel, note string) {
		s.logger.Warn("sweep: "+note, "path", filepath.Join(root, userID, filepath.FromSlash(rel)))
	}
}

// sweepUnknownUser removes every regular file, symlink and special file
// older than orphanMinAge in a scratchpad directory with no user behind
// it. Empty directories are left alone: they cost nothing, and a
// re-registered ULID never happens anyway — they are simply too small to
// be worth a deletion rule.
func (s *Server) sweepUnknownUser(root, userID string, now time.Time) int {
	res, err := scratch.SweepTree(root, userID, ".", scratch.SweepSpec{
		Now:        now,
		FileMaxAge: orphanMinAge,
		MaxEntries: sweepInspections, // a flooded orphan cannot burn the hour either
		Warn:       s.sweepWarn(root, userID),
	})
	if err != nil {
		s.logger.Warn("sweep: user dir not swept", "path", filepath.Join(root, userID), "error", err)
	}
	return res.Files
}

// sweepTTL ages one layout directory of one user: regular files with an
// mtime past ttl are unlinked (at any depth — each file ages on its own,
// sweeping never reorganizes), then empty subdirectories whose own mtime
// is past ttl are rmdir'd post-order. Removing a file bumps its parent
// directory's mtime, so a directory emptied this pass becomes rmdir-able
// once IT has aged — one TTL later, at the next pass that finds it still
// empty. Aged symlinks and special files go too (unlinked as themselves,
// never followed — scratch.SweepTree); a young symlink is reported and
// left alone. An unreadable directory is skipped, never partially swept.
// Every operation is fd-pinned (scratch.SweepTree, astra finding 2) and
// the pass is bounded (astra2 finding 3).
func (s *Server) sweepTTL(root, userID, dir string, ttl time.Duration, now time.Time) {
	res, err := scratch.SweepTree(root, userID, dir, scratch.SweepSpec{
		Now:        now,
		FileMaxAge: ttl,
		DirMaxAge:  ttl,
		MaxFiles:   sweepPass,
		MaxEntries: sweepInspections,
		Warn:       s.sweepWarn(root, userID),
	})
	if err != nil {
		s.logger.Warn("sweep: dir not swept", "path", filepath.Join(root, userID, dir), "error", err)
		return
	}
	if res.Files > 0 || res.Dirs > 0 || res.Capped || res.EntriesCapped {
		s.logger.Info("ttl sweep", "dir", dir, "removed", res.Files, "dirs", res.Dirs,
			"capped", res.Capped, "entries_capped", res.EntriesCapped)
	}
}

// removeIfOld deletes p when its mtime is past maxAge; reports whether it
// deleted. Scope note (astra finding 2): this name-based helper is now used
// ONLY for the ".upload-*"/".share-*" temps in the scratch root, whose
// parent directory is written by the daemon alone. Anything inside a user
// tree is swept by scratch.SweepTree, which never resolves attacker-raced
// path strings.
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
