package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gabkeep/internal/id"
	"gabkeep/internal/scratch"
)

// Background scratchpad lifecycle jobs (spec §8, §9.7):
//   - expired artifacts: rows past artifact_ttl are deleted, bytes with them;
//   - orphan files: bytes on disk without a files row (crash between upload
//     phases, or user rows cascaded away) older than a safety window;
//   - abandoned upload temps (crash between temp write and rename).
//
// Each sweep pass is bounded (§9.11); the loop runs once at startup and then
// hourly.

const (
	maintenanceInterval = time.Hour
	orphanMinAge        = 24 * time.Hour // never race an in-flight upload
	sweepPass           = 1000           // max rows per expiry pass
)

// RunMaintenance performs the file sweeps immediately, then hourly until ctx
// ends. Safe to run as a goroutine.
func (s *Server) RunMaintenance(ctx context.Context) {
	s.sweepScratchpad(ctx)
	t := time.NewTicker(maintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepScratchpad(ctx)
		}
	}
}

func (s *Server) sweepScratchpad(ctx context.Context) {
	now := time.Now()

	// 1) Expired artifacts — drain backlog a bounded pass at a time.
	for {
		exp, err := s.store.ExpiredArtifacts(ctx, now, sweepPass)
		if err != nil {
			s.logger.Warn("artifact expiry query failed", "error", err)
			break
		}
		if len(exp) == 0 {
			break
		}
		ids := make([]string, 0, len(exp))
		for i := range exp {
			if err := scratch.Remove(s.cfg.Scratchpad.Root, exp[i].UserID, exp[i].RelPath); err != nil &&
				!isGone(err) {
				s.logger.Warn("unlink expired artifact", "error", err, "file", exp[i].ID)
			}
			ids = append(ids, exp[i].ID)
		}
		if err := s.store.DeleteFilesByID(ctx, ids); err != nil {
			s.logger.Warn("delete expired artifact rows", "error", err)
			break
		}
		s.logger.Info("swept expired artifacts", "count", len(ids))
		if len(exp) < sweepPass {
			break
		}
	}

	// 2) Orphans: on-disk files with no row (or no live user behind it).
	known, err := s.store.ExistingFileSet(ctx)
	if err != nil {
		s.logger.Warn("orphan sweep: known set failed", "error", err)
		return
	}
	root := s.cfg.Scratchpad.Root
	userDirs, err := os.ReadDir(root)
	if err != nil {
		s.logger.Warn("orphan sweep: read scratch root", "error", err)
		return
	}
	var removed, kept int
	for _, de := range userDirs {
		name := de.Name()
		if strings.HasPrefix(name, ".upload-") { // abandoned temp (post-crash)
			if s.removeIfOld(filepath.Join(root, name), now) {
				removed++
			}
			continue
		}
		// Only ULID directories are gabkeep's; anything else is left alone.
		if !de.IsDir() || !id.Valid(name) {
			continue
		}
		userID := name
		filepath.WalkDir(filepath.Join(root, userID), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // unreadable entries never abort the walk
			}
			if d.Type()&fs.ModeSymlink != 0 {
				s.logger.Warn("symlink inside scratchpad tree (left alone)",
					"user", userID, "path", p)
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(filepath.Join(root, userID), p)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if known[userID+"/"+rel] {
				return nil
			}
			if s.removeIfOld(p, now) { // age gate: in-flight uploads stay untouched
				removed++
			} else {
				kept++
			}
			return nil
		})
	}
	if removed > 0 || kept > 0 {
		s.logger.Info("orphan sweep", "removed", removed, "pending_age_gate", kept)
	}
}

// removeIfOld deletes p when its mtime is past the safety window; reports
// whether it deleted.
func (s *Server) removeIfOld(p string, now time.Time) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if now.Sub(fi.ModTime()) < orphanMinAge {
		return false
	}
	if err := os.Remove(p); err != nil {
		s.logger.Warn("orphan remove failed", "path", p, "error", err)
		return false
	}
	return true
}

func isGone(err error) bool {
	return errors.Is(err, scratch.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}
