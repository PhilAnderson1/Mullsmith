// Package managed owns application-managed scratchpad writes and their
// quota critical section. Sandbox primitives remain in internal/scratch.
package managed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"mullsmith/internal/id"
	"mullsmith/internal/scratch"
)

var (
	ErrQuota      = errors.New("storage quota exceeded")
	ErrUnverified = errors.New("storage usage could not be verified; check scratchpad permissions")
	ErrDiskLow    = errors.New("server storage is nearly full")
	ErrConflict   = errors.New("too many files already share this name")
)

type Service struct {
	gate  chan struct{}
	quota int64
}

func New(quota int64) *Service { return &Service{quota: quota, gate: make(chan struct{}, 1)} }

type File struct {
	Path, Name string
	Bytes      int64
}

func (s *Service) Check(ctx context.Context, root, userID string) error {
	return checkQuota(ctx, root, userID, s.quota)
}
func (s *Service) Exceeds(ctx context.Context, root, userID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.quota <= 0 {
		return false, nil
	}
	return scratch.DirSizeExceeds(ctx, root, userID, s.quota)
}
func (s *Service) CommitUpload(ctx context.Context, root, userID, temp, display string, n int64) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()
	used, err := scratch.DirSize(ctx, root, userID)
	if err != nil {
		return "", storageError(err)
	}
	if s.quota > 0 && used+n > s.quota {
		return "", ErrQuota
	}
	return placeNamed(ctx, root, userID, "uploads", display, temp)
}

// CommitDownload atomically places a completed download in an existing work/
// or artifacts/ directory. It never replaces existing files. The caller owns
// the temporary file and removes it on every failure.
func (s *Service) CommitDownload(ctx context.Context, root, userID, temp, rel string, n int64) error {
	if err := WriteTarget(rel); err != nil {
		return safeError("web_download", err)
	}
	if !scratch.DiskFloorOK(root) {
		return ErrDiskLow
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	used, err := scratch.DirSize(ctx, root, userID)
	if err != nil {
		return storageError(err)
	}
	if n < 0 || (s.quota > 0 && (used > s.quota || n > s.quota-used)) {
		return ErrQuota
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scratch.PlaceNoReplace(root, userID, rel, temp); err != nil {
		if errors.Is(err, scratch.ErrExists) {
			return errors.New("web_download: destination already exists; choose another path")
		}
		return safeError("web_download", err)
	}
	return nil
}

func placeNamed(ctx context.Context, root, userID, dir, name, temp string) (string, error) {
	for i := 0; i < 200; i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		rel := path.Join(dir, scratch.BumpName(name, i))
		err := scratch.PlaceNoReplace(root, userID, rel, temp)
		if err == nil {
			return rel, nil
		}
		if !errors.Is(err, scratch.ErrExists) {
			return "", safeError("file placement", err)
		}
	}
	return "", ErrConflict
}
func (s *Service) Stage(ctx context.Context, root, userID string, rels []string) ([]string, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	if err := s.Check(ctx, root, userID); err != nil {
		return nil, err
	}
	if err := scratch.EnsureUserDirs(root, userID); err != nil {
		return nil, safeError("code_run stage", err)
	}
	if err := precheckStagedSizes(ctx, root, userID, rels, s.quota); err != nil {
		return nil, err
	}
	return stageFiles(ctx, root, userID, rels)
}
func (s *Service) WriteText(ctx context.Context, root, userID, rel, content string) (int, error) {
	if err := WriteTarget(rel); err != nil {
		return 0, err
	}
	if !scratch.DiskFloorOK(root) {
		return 0, errors.New("server storage is nearly full")
	}
	// Fresh-check → write → re-check is ONE quota-critical section, shared
	// with upload commits and code_run staging: separately-atomic checks let
	// joint writers each pass while overrunning together (audit2 finding 3).
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()

	// Inspecting an existing target does DOUBLE duty (finding 3): its size
	// keeps the quota maths honest while its bytes must survive a failed
	// replacement — which staged-and-renamed writes guarantee, and a
	// non-regular target keeps its explicit refusal for, instead of being
	// silently swapped or destroyed.
	var oldSize int64
	if fi, serr := scratch.Stat(root, userID, rel); serr == nil {
		if !fi.Mode().IsRegular() {
			if fi.IsDir() {
				return 0, safeError("fs_write", scratch.ErrIsDir)
			}
			return 0, fmt.Errorf("fs_write: %s is a special file; refusing to replace it", quoteShort(rel))
		}
		oldSize = fi.Size()
	} else if !errors.Is(serr, scratch.ErrNotFound) {
		// An unreadable path means the replacement's quota maths (oldSize)
		// cannot be done honestly either — same refusal the quota scan uses,
		// not a raw permission string (fail closed, §9.7).
		if errors.Is(serr, os.ErrPermission) {
			return 0, errors.New("storage usage could not be verified; check scratchpad permissions")
		}
		return 0, safeError("fs_write", serr)
	}
	// The old file's bytes free up exactly when the new ones land, so they
	// come off the used total BEFORE either verdict — otherwise replacing a
	// file at quota would always look over-quota (the two copies are
	// counted jointly only for the moment of the swap).
	if err := checkQuotaAllowing(ctx, root, userID, s.quota, oldSize); err != nil {
		return 0, err
	}

	// Stage beside the destination (same directory → rename is atomic),
	// then commit with a rename that never opens or follows the target: a
	// failure at ANY step leaves the original intact. The previous version
	// truncated in place and, on a failed quota re-check, deleted the path
	// outright — destroying a file the user still owned (finding 3).
	tmpRel := stagingPath(rel)
	f, err := scratch.CreateExclusive(root, userID, tmpRel, 0o600)
	if err != nil {
		return 0, safeError("fs_write", err)
	}
	defer scratch.Remove(root, userID, tmpRel)
	n64, err := copyContext(ctx, f, strings.NewReader(content))
	n := int(n64)
	if cerr := f.Close(); err == nil {
		err = cerr // surface a flush failure, not just the WriteString one
	}
	if err != nil {
		_ = scratch.Remove(root, userID, tmpRel)
		return 0, safeError("write", err)
	}
	// Quota again after close — bytes may have crossed the line (§9.7). The
	// re-scan counts BOTH copies, so subtract oldSize again; crossing the
	// line discards only the staged copy. A failed re-scan keeps the
	// original (no auto-delete on an unknown total) but never reports
	// success: nothing landed UNVERIFIED (audit2 finding 3, fail closed).
	if err := checkQuotaAllowing(ctx, root, userID, s.quota, oldSize); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		_ = scratch.Remove(root, userID, tmpRel)
		if s.quota > 0 {
			if _, derr := scratch.DirSize(ctx, root, userID); derr != nil {
				return 0, errors.New("wrote " + quoteShort(rel) +
					", but storage usage could not be verified; check scratchpad permissions")
			}
		}
		return 0, errors.New("write would exceed the storage quota; replacement discarded")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := scratch.RenameOver(root, userID, tmpRel, rel); err != nil {
		_ = scratch.Remove(root, userID, tmpRel)
		return 0, safeError("fs_write", err)
	}
	return n, nil
}
func (s *Service) Share(ctx context.Context, root, userID, source string) (File, error) {
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	rel := strings.TrimSpace(source)
	if rel == "" {
		return File{}, errors.New("path is required")
	}
	// The uploads namespace is off-limits AFTER normalization (same lesson
	// as audit finding 7): uploads must never gain a TTL'd artifact copy —
	// the sweep or a card-delete must never reach the user's originals.
	segs, err := scratch.ParseRel(rel)
	if err != nil {
		return File{}, safeError("files_share", err)
	}
	if len(segs) == 0 {
		return File{}, errors.New("path is required")
	}
	if segs[0] == "uploads" {
		return File{}, errors.New("uploads are the user's data and already downloadable " +
			"from the Files page; share files under work/ or artifacts/ instead")
	}
	rel = path.Join(segs...) // canonical spelling all the way through (§8)
	fi, err := scratch.Stat(root, userID, rel)
	if err != nil {
		return File{}, safeError("files_share", err)
	}
	if fi.IsDir() {
		return File{}, errors.New("path is a directory, not a file")
	}
	if !fi.Mode().IsRegular() {
		// FIFOs/sockets would block the copy open indefinitely.
		return File{}, errors.New("path is not a regular file")
	}

	if segs[0] == "artifacts" {
		// Already a deliverable: deliver as-is. No copy, no quota spend;
		// the card points at the existing bytes and their own mtime
		// governs expiry (re-sharing refreshes nothing — that is what
		// snapshots mean).
		return File{Path: rel, Name: path.Base(rel), Bytes: fi.Size()}, nil
	}

	// A copy is a WRITE: same disk floor, lock and checks as fs_write.
	if !scratch.DiskFloorOK(root) {
		return File{}, ErrDiskLow
	}
	if err := s.acquire(ctx); err != nil {
		return File{}, err
	}
	defer s.release()
	if err := checkQuota(ctx, root, userID, s.quota); err != nil {
		return File{}, err
	}
	if err := scratch.MkdirAll(root, userID, "artifacts"); err != nil {
		return File{}, safeError("files_share", err)
	}

	src, err := scratch.OpenFile(root, userID, rel)
	if err != nil {
		return File{}, safeError("files_share", err)
	}
	defer src.Close()

	// Stream to a temp in the scratch ROOT (same filesystem as the
	// destination, so the landing rename is atomic), then collide-free
	// NOREPLACE placement with name bumping. The COPY is what the card
	// points at, so later edits to the source never rewrite history.
	tmp, err := os.CreateTemp(root, ".share-*")
	if err != nil {
		return File{}, errors.New("share staging failed")
	}
	tempPath := tmp.Name()
	defer os.Remove(tempPath) // no-op after a successful placement
	n, err := copyContext(ctx, tmp, src)
	if cerr := tmp.Close(); err == nil {
		err = cerr // a failed close can silently truncate the copy
	}
	if err != nil {
		return File{}, safeError("share copy", err)
	}

	dstRel, err := placeNamed(ctx, root, userID, "artifacts", path.Base(rel), tempPath)
	if err != nil {
		return File{}, err
	}

	// Fail-closed re-check exactly like fs_write: the bytes landed, so the
	// total must be VERIFIED; a real overrun is collected again, an
	// unverifiable one is reported instead of a false success (§9.7).
	if s.quota > 0 {
		used, derr := scratch.DirSize(ctx, root, userID)
		if derr != nil {
			if ctx.Err() != nil {
				return File{}, ctx.Err()
			}
			return File{}, errors.New("shared " + quoteShort(dstRel) +
				", but storage usage could not be verified; check scratchpad permissions")
		}
		if used > s.quota {
			_ = scratch.Remove(root, userID, dstRel)
			return File{}, errors.New("share would exceed the storage quota; copy removed")
		}
	}
	return File{Path: dstRel, Name: path.Base(dstRel), Bytes: n}, nil
}
func checkQuota(ctx context.Context, root, userID string, quota int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if quota <= 0 {
		return nil
	}
	used, err := scratch.DirSize(ctx, root, userID)
	if err != nil {
		return storageError(err)
	}
	if used >= quota {
		return ErrQuota
	}
	return nil
}
func checkQuotaAllowing(ctx context.Context, root, userID string, quota, deduct int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if quota <= 0 {
		return nil
	}
	used, err := scratch.DirSize(ctx, root, userID)
	if err != nil {
		return storageError(err)
	}
	if used-deduct >= quota {
		return ErrQuota
	}
	return nil
}
func WriteTarget(rel string) error {
	segs, err := scratch.ParseRel(rel)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return errors.New("path must be inside work/ or artifacts/")
	}
	switch segs[0] {
	case "uploads":
		return errors.New("uploads/ is read-only for the model; write under work/ or artifacts/")
	case "work", "artifacts":
		if len(segs) == 1 {
			return errors.New("path must be a file inside work/ or artifacts/, not the directory itself")
		}
		return nil
	default:
		return errors.New("the scratchpad root is not writable; write under work/ or artifacts/")
	}
}
func stagingPath(rel string) string {
	dir, _ := path.Split(rel)
	return dir + ".tmp-" + strings.ToLower(id.New())
}
func safeError(op string, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, scratch.ErrPathUnsafe):
		return fmt.Errorf("%s: that path is outside the scratchpad sandbox", op)
	case errors.Is(err, scratch.ErrNotFound):
		return fmt.Errorf("%s: not found", op)
	case errors.Is(err, scratch.ErrIsDir):
		return fmt.Errorf("%s: path is a directory", op)
	case errors.Is(err, scratch.ErrNotDir):
		return fmt.Errorf("%s: path component is not a directory", op)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%s: permission denied", op)
	default:
		return fmt.Errorf("%s failed", op)
	}
}
func humanBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
func quoteShort(s string) string {
	r := []rune(s)
	if len(r) > 60 {
		s = string(r[:60]) + "…"
	}
	return fmt.Sprintf("%q", s)
}
func precheckStagedSizes(ctx context.Context, root, userID string, rels []string, quota int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if quota <= 0 {
		return nil
	}
	var add int64
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		f, err := scratch.OpenFile(root, userID, rel) // same sandbox as the copy
		if err != nil {
			return safeError("code_run stage", err)
		}
		fi, serr := f.Stat()
		_ = f.Close()
		if serr != nil {
			return safeError("code_run stage", serr)
		}
		if target, err := scratch.Stat(root, userID, path.Join("work", path.Base(rel))); err == nil && os.SameFile(fi, target) {
			continue // already staged; no quota spend
		}
		add += fi.Size()
	}
	if add == 0 {
		return nil
	}
	used, err := scratch.DirSize(ctx, root, userID)
	if err != nil {
		return storageError(err)
	}
	if used+add > quota {
		return fmt.Errorf("code_run: staging %s would exceed the storage quota (%s used)",
			humanBytes(add), humanBytes(used))
	}
	return nil
}
func stageFiles(ctx context.Context, root, userID string, rels []string) ([]string, error) {
	var done []string
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		base := path.Base(path.Clean("/" + rel)) // last component only, always safe
		if base == "." || base == "/" {
			return done, fmt.Errorf("code_run: cannot stage %q", rel)
		}
		src, err := scratch.OpenFile(root, userID, rel)
		if err != nil {
			return done, safeError("code_run stage", err)
		}
		dest := path.Join("work", base)
		fi, err := src.Stat()
		if err != nil {
			_ = src.Close()
			return done, safeError("code_run stage", err)
		}
		if target, err := scratch.Stat(root, userID, dest); err == nil && os.SameFile(fi, target) {
			_ = src.Close()
			done = append(done, base)
			continue
		}
		temp := stagingPath(dest)
		dst, err := scratch.CreateExclusive(root, userID, temp, 0o600)
		if err != nil {
			_ = src.Close()
			return done, safeError("code_run stage", err)
		}
		_, copyErr := copyContext(ctx, dst, src)
		_ = src.Close()
		if closeErr := dst.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr == nil {
			copyErr = ctx.Err()
		}
		if copyErr == nil {
			copyErr = scratch.RenameOver(root, userID, temp, dest)
		}
		_ = scratch.Remove(root, userID, temp)
		if copyErr != nil {
			return done, safeError("code_run staging "+quoteShort(base), copyErr)
		}
		done = append(done, base)
	}
	return done, nil
}

// acquire rejects cancellation both while waiting and immediately after entry.
func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) release() { <-s.gate }

func storageError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrUnverified
}

// Wrapping the reader prevents file-copy fast paths from bypassing checks.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, contextReader{ctx: ctx, reader: src})
}
