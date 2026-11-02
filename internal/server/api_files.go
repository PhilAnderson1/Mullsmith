package server

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mullsmith/internal/managed"
	"mullsmith/internal/scratch"
)

// --- The physical file API (§5.3): upload, list, download, delete ----------
//
// The disk is the single source of truth: a file exists iff its bytes
// exist, its identity is its path (kind = top-level directory), and the
// Files page lists exactly what a walk finds. There is no registry, no
// ids: listings use modification-time/path cursors over bounded scans. Ownership is structural: ?path= resolves only inside the SESSION
// user's tree through the fd-pinned sandbox (§9.7), so every rejection
// answers 404 — "no such thing" and "not yours" are the same answer.

// The three layout directories and their singular item kinds (badges).
var fileDirs = []string{"uploads", "artifacts", "work"}

func fileKindOf(dir string) string {
	switch dir {
	case "uploads":
		return "upload"
	case "artifacts":
		return "artifact"
	default:
		return "work"
	}
}

func isFileDir(seg string) bool {
	return seg == "uploads" || seg == "artifacts" || seg == "work"
}

const (
	filePageDefault = 50
	filePageMax     = 200
	fileListScanMax = 200_000 // inspected entries per layout directory
	fileListDeep    = 4       // directory depth explored below a layout dir
)

// fileDownloadURL is the one download shape: a canonical path, URL-encoded
// (no whitespace/quotes survive), always admitted by the frontend's
// fileApiUrl guard.
func fileDownloadURL(rel string) string {
	return "/api/v1/files/download?path=" + url.QueryEscape(rel)
}

// mediaTypeForUpload classifies bytes: content sniffing first, then an
// extension hint for common text types sniffing labels octet-stream.
func mediaTypeForUpload(sniff []byte, name string) string {
	mt := http.DetectContentType(sniff)
	ext := strings.ToLower(path.Ext(name))
	// CSV bytes sniff as plain text; the extension disambiguates. The type
	// rides on the download so the saved file opens in the right app
	// (everything is an attachment; nosniff pins the type to what we say).
	if strings.HasPrefix(mt, "text/plain") && ext == ".csv" {
		return "text/csv"
	}
	if strings.HasPrefix(mt, "application/octet-stream") {
		switch ext {
		case ".json", ".jsonl", ".ndjson":
			return "application/json"
		case ".md", ".markdown", ".txt", ".log", ".tsv":
			return "text/plain"
		}
	}
	return mt
}

// sanitizeUploadName turns a client filename into the ONE disk segment the
// bytes will live under (§9.4): basename, control chars → "_", trimmed,
// length-capped — plus the dot rule: LEADING dots become a single "_",
// because a dotfile would vanish from the Files listing and turn into an
// unmanageable ghost (.env → _env, "..x" → _x). The result is always a
// single non-empty segment; scratch.ParseRel re-checks that at every use.
// Collision names (" (1)") are added at placement time by commitUpload.
func sanitizeUploadName(raw string) string {
	name := filepath.Base(strings.ReplaceAll(raw, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, ".") {
		name = "_" + strings.TrimLeft(name, ".")
	}
	if name == "" {
		name = "upload"
	}
	if len(name) > 200 {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

// uploadExtension returns the blocked-or-okay final extension of a display
// name, lowercased and dot-prefixed ("" when absent/too long).
func uploadExtension(name string, blocked []string) (string, bool) {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" || len(ext) > 11 {
		return "", false
	}
	for _, b := range blocked {
		if ext == b {
			return ext, true
		}
	}
	return ext, false
}

// uploadMultipartAllowance is the framing headroom over max_file_size for
// a whole upload request: boundary lines, part headers and any ignored
// form fields — the UI sends none, so 64 KiB is generous by design (audit
// finding 6). uploadMaxParts bounds part-iteration waste; a legitimate
// upload carries exactly one part.
const (
	uploadMultipartAllowance = 64 << 10
	uploadMaxParts           = 64
)

// uploadTooLarge answers the 413 shared by the budget-exhausted paths.
func uploadTooLarge(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, http.StatusRequestEntityTooLarge, "upload_too_large",
		"request exceeds the upload size limit")
}

// handleFileUpload streams a single multipart "file" part to the scratchpad
// (§9.11: never buffered whole; the bodyLimit middleware exempts this route
// because the applicable cap is max_file_size, not max_body_bytes). The
// bytes land under the user's OWN (sanitized) filename — with the registry
// gone the name lives on disk, and a collision bumps to "name (1).ext"
// instead of silently overwriting.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	// Global upload concurrency gate (audit finding 5): bounds in-flight
	// temp bytes and quota-walk stampedes independently of the request-rate
	// bucket. Refused outright — a saturated upload lane is a flood, and
	// honest clients retry naturally.
	if s.uploadSem != nil {
		select {
		case s.uploadSem <- struct{}{}:
			defer func() { <-s.uploadSem }()
		default:
			w.Header().Set("Retry-After", "2")
			writeAPIError(w, r, http.StatusTooManyRequests, "server_busy",
				"too many concurrent uploads, try again shortly")
			return
		}
	}
	if !scratch.DiskFloorOK(s.cfg.Scratchpad.Root) {
		writeAPIError(w, r, http.StatusServiceUnavailable, "storage_low", "server storage is nearly full")
		return
	}
	// Advisory fail-fast: an already-full user never pays for streaming.
	// The authoritative decision is commitUpload, against fresh usage.
	// An unverifiable scan REFUSES the upload (audit2 finding 3): unknown
	// usage never authorizes a write; raw scan errors stay in the log.
	if err := s.files.Check(s.cfg.Scratchpad.Root, user.ID); err != nil {
		code, message := "quota_exceeded", "storage quota exceeded"
		if errors.Is(err, managed.ErrUnverified) {
			code, message = "storage_unverified", "Storage usage could not be verified; check scratchpad permissions"
		}
		writeAPIError(w, r, http.StatusInsufficientStorage, code, message)
		return
	}

	// Whole-request cap (audit finding 6): this route skips the generic
	// max_body_bytes because the file part streams to disk under
	// max_file_size — but unwrapped, everything ELSE in the request was
	// free: ignored fields drained by p.Close(), or the entire body to EOF
	// when no file part appears. One budget now covers the whole stream:
	// the file cap plus fixed framing allowance. Every read path — part
	// scan, Close-drain, streaming copy — fails with MaxBytesError once
	// it is spent, answered as 413 below.
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.Scratchpad.MaxFileSize)+uploadMultipartAllowance)
	mr, err := r.MultipartReader()
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "bad_multipart", "multipart/form-data with a \"file\" part required")
		return
	}
	var part *multipartPart
	for parts := 0; ; {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if IsBodyTooLarge(err) {
				uploadTooLarge(w, r)
				return
			}
			writeAPIError(w, r, http.StatusBadRequest, "bad_multipart", "malformed multipart body")
			return
		}
		parts++
		if parts > uploadMaxParts { // iteration bound; the byte budget alone
			_ = p.Close() // would still admit ~10^6 tiny parts (audit 6)
			writeAPIError(w, r, http.StatusBadRequest, "too_many_parts", "upload carries too many form fields")
			return
		}
		if p.FormName() == "file" && p.FileName() != "" {
			part = &multipartPart{display: sanitizeUploadName(p.FileName()), reader: p}
			break
		}
		_ = p.Close() // form fields we do not use
	}
	if part == nil {
		writeAPIError(w, r, http.StatusBadRequest, "no_file", "a \"file\" part is required")
		return
	}
	defer part.reader.Close()

	ext, blocked := uploadExtension(part.display, s.cfg.Scratchpad.BlockExtensions)
	if blocked {
		writeAPIError(w, r, http.StatusUnprocessableEntity, "file_type_blocked", "this file type cannot be uploaded")
		return
	}
	_ = ext // the disk name carries the extension; this call checks it

	if err := scratch.EnsureUserDirs(s.cfg.Scratchpad.Root, user.ID); err != nil {
		s.logger.Error("provision scratchpad", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	tmp, err := os.CreateTemp(s.cfg.Scratchpad.Root, ".upload-*")
	if err != nil {
		s.logger.Error("upload temp", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	tempName := tmp.Name()
	defer os.Remove(tempName) // no-op after a successful placement

	// Sniff and size in one streaming pass; cap at max_file_size + 1 so
	// "exactly too big" is distinguishable from merely oversized.
	max := int64(s.cfg.Scratchpad.MaxFileSize)
	head := make([]byte, 512)
	nHead, _ := io.ReadFull(part.reader, head)
	n, err := io.Copy(tmp, io.LimitReader(io.MultiReader(
		bytes.NewReader(head[:nHead]), part.reader), max+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr // a failed close can silently lose bytes pre-rename
	}
	switch {
	case n > max:
		writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds the size limit")
		return
	case err != nil:
		if IsBodyTooLarge(err) { // budget spent mid-copy (oversized part)
			uploadTooLarge(w, r)
			return
		}
		writeAPIError(w, r, http.StatusBadRequest, "upload_failed", "upload could not be read")
		return
	}
	rel, ok := s.commitUpload(w, r, user.ID, tempName, part.display, n)
	if !ok {
		return
	}
	// Re-check the floor after landing bytes: refuse service for the next
	// upload rather than corrupt state mid-write (§9.7).
	if !scratch.DiskFloorOK(s.cfg.Scratchpad.Root) {
		s.logger.Warn("disk floor breached right after upload", "rid", RequestIDFrom(r.Context()))
	}

	s.logger.Info("upload ok", "path", rel, "bytes", n, "user", user.ID, "rid", RequestIDFrom(r.Context()))
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":         path.Base(rel), // what survived collision bumping
		"path":         rel,
		"size":         n,
		"created_at":   time.Now().UTC().Format(timeRFC3339),
		"media_type":   mediaTypeForUpload(head[:nHead], part.display),
		"download_url": fileDownloadURL(rel),
	})
}

// commitUpload translates managed-storage outcomes into HTTP responses.
func (s *Server) commitUpload(w http.ResponseWriter, r *http.Request,
	userID, tempName, display string, n int64,
) (rel string, ok bool) {
	rel, err := s.files.CommitUpload(s.cfg.Scratchpad.Root, userID, tempName, display, n)
	if err == nil {
		return rel, true
	}
	switch {
	case errors.Is(err, managed.ErrUnverified):
		writeAPIError(w, r, http.StatusInsufficientStorage, "storage_unverified", "Storage usage could not be verified; check scratchpad permissions")
	case errors.Is(err, managed.ErrQuota):
		writeAPIError(w, r, http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded")
	case errors.Is(err, managed.ErrConflict):
		writeAPIError(w, r, http.StatusConflict, "name_conflict", "too many files already share this name; rename the file and try again")
	default:
		s.logger.Error("upload place", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
	}
	return "", false
}

type multipartPart struct {
	display string
	reader  io.ReadCloser
}

// --- GET /api/v1/files/download?path= | DELETE /api/v1/files?path= ---------

// fileRelFrom validates the ?path= parameter shared by download and
// delete: a scratchpad-relative path at least two segments deep whose
// first segment is one of the three layout directories — so the layout
// dirs themselves can never be addressed (you delete things INSIDE
// uploads/, never "uploads"). Every rejection answers 404, the stance of
// never distinguishing "no such thing" from "not yours" (§9.2).
func fileRelFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	segs, err := scratch.ParseRel(r.URL.Query().Get("path"))
	if err != nil || len(segs) < 2 || !isFileDir(segs[0]) {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return "", false
	}
	return strings.Join(segs, "/"), true
}

// handleFileDownload serves any regular file in the three layout dirs.
// ALWAYS as Content-Disposition: attachment (§9.4): a download is a
// download, never a page rendered in the app's origin. This is the
// security anchor under the (now optional) upload extension gate —
// even a deliberately uploaded .html only ever reaches the browser as a
// file the user chose to save. The media type is sniffed on the fly —
// nothing caches it, the bytes are the truth — so the saved file opens
// with the right application. Regular files only: a script could plant
// a FIFO at a path, and opening one for read would block serving until
// a writer appears.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	rel, ok := fileRelFrom(w, r)
	if !ok {
		return
	}
	fi, err := scratch.Stat(s.cfg.Scratchpad.Root, user.ID, rel)
	if err != nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if !fi.Mode().IsRegular() {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	}
	f, err := scratch.OpenFile(s.cfg.Scratchpad.Root, user.ID, rel)
	if err != nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	}
	defer f.Close()

	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	name := path.Base(rel)
	mt := mediaTypeForUpload(head[:n], name)
	w.Header().Set("Content-Type", mt)
	// Attachment always (§9.4): no query parameter can ask for inline.
	w.Header().Set("Content-Disposition", mineDisposition("attachment", name))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if r.Method == http.MethodHead {
		return
	}
	// Rolling per-chunk write deadline (astra2 finding 1): the server-wide
	// write cap budgets a WHOLE response — Go arms it once per request —
	// which no honest multi-megabyte download can promise. What must be
	// bounded is PROGRESS: a client that stops reading dies after one
	// stalled chunk, a slow-but-live client never runs out of budget. Same
	// doctrine as sse.go's per-frame deadline.
	rc := http.NewResponseController(w)
	_, _ = io.Copy(deadlineWriter{rc: rc, w: w}, io.MultiReader(bytes.NewReader(head[:n]), f))
	_ = rc.SetWriteDeadline(time.Time{})
}

// downloadWriteTimeout bounds ONE chunk handoff of a download. 30 s without
// handing over 32 KB means the client is gone. Var so tests can shrink it.
var downloadWriteTimeout = 30 * time.Second

// deadlineWriter arms a fresh write deadline before every Write, so the cap
// is per chunk, rolling — never an absolute ceiling on transfer time.
type deadlineWriter struct {
	rc *http.ResponseController
	w  io.Writer
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	if err := d.rc.SetWriteDeadline(time.Now().Add(downloadWriteTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) { // unwrappable wrappers must not fail every download
		return 0, err
	}
	return d.w.Write(p)
}

// mineDisposition builds Content-Disposition with an RFC 5987 UTF-8 name so
// any display name is injection-safe (quotes, newlines impossible).
func mineDisposition(kind, name string) string {
	return mime.FormatMediaType(kind, map[string]string{"filename": name})
}

// handleFileDelete unlinks a file, or a directory and its whole tree
// (§9.7: fd-pinned, symlinks unlinked never followed). NO table cleanup
// happens because no tables are involved: old download cards and chat
// chips for deleted bytes 404, which is this app's designed "gone" state.
//
// With ?older_than= it becomes the Files page's bulk "Delete older than…"
// command instead (handleFilePurge); ?path= and ?older_than= are
// alternatives, never combined.
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("older_than") != "" {
		s.handleFilePurge(w, r)
		return
	}
	user := UserFrom(r.Context())
	rel, ok := fileRelFrom(w, r)
	if !ok {
		return
	}
	fi, err := scratch.Stat(s.cfg.Scratchpad.Root, user.ID, rel)
	if err != nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if fi.IsDir() {
		s.deleteTree(w, r, user.ID, rel)
		return
	}
	if err := scratch.Remove(s.cfg.Scratchpad.Root, user.ID, rel); err != nil {
		if errors.Is(err, scratch.ErrNotFound) || errors.Is(err, scratch.ErrPathUnsafe) {
			writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
			return
		}
		s.logger.Error("file delete", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteTree removes a layout directory and everything under it with the
// same protections as file deletion: fileRelFrom already restricted the
// path to a layout dir at ≥2 segments (the dirs themselves are
// unaddressable), and scratch.RemoveTree deletes every element through
// the fd-pinned sandbox. A tree with any part that cannot be enumerated
// (chmod 000) is refused rather than half-deleted.
func (s *Server) deleteTree(w http.ResponseWriter, r *http.Request, userID, rel string) {
	if err := scratch.RemoveTree(s.cfg.Scratchpad.Root, userID, rel); err != nil {
		if errors.Is(err, scratch.ErrNotFound) || errors.Is(err, scratch.ErrPathUnsafe) {
			writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
			return
		}
		s.logger.Error("dir delete", "error", err, "path", rel, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- DELETE /api/v1/files?older_than=&kind= ----------------------------------

// A single purge request is bounded the way the hourly sweep is, so one
// click cannot pin the server to a walk of arbitrary size. A capped purge
// deletes everything it inspected and says so; the user re-runs it —
// unlike the sweep, the caller here is present and retrying is cheap.
const (
	purgeMaxFiles   = 50_000  // unlinks per purge request
	purgeMaxEntries = 200_000 // entry inspections per purge request
)

// handleFilePurge is the Files page's bulk housekeeping: it deletes aged
// content from the CURRENT view — one layout directory via ?kind=, or all
// three when kind is empty — using the same age tokens as the sidebar's
// conversation prune (pruneAges: 1d/1w/1mo/1y/all, mtime-based). The work
// is the fd-pinned sweep itself (scratch.SweepTree, §9.7): regular files
// past the cutoff are unlinked at any depth, aged empty directories are
// rmdir'd, symlinks and special files go as themselves and are never
// followed, unreadable corners are skipped whole. The layout directories
// themselves survive, as always. A directory emptied by this purge has
// its mtime bumped by the removals, so it ages for a later one — the
// documented sweep doctrine, not a bug to paper over. A user root or
// layout directory that does not exist yet simply has nothing to delete.
func (s *Server) handleFilePurge(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if r.URL.Query().Get("path") != "" {
		writeAPIError(w, r, http.StatusBadRequest, "bad_request",
			"path and older_than are alternatives")
		return
	}
	age, ok := pruneAges[r.URL.Query().Get("older_than")]
	if !ok {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_older_than",
			"older_than must be one of 1d, 1w, 1mo, 1y, all")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && !isFileDir(kind) {
		writeAPIError(w, r, http.StatusBadRequest, "bad_kind", "kind must be uploads, artifacts or work")
		return
	}
	dirs := fileDirs
	if kind != "" {
		dirs = []string{kind}
	}
	// SweepSpec.DirMaxAge ≤ 0 means "leave directories alone"; the purge
	// never means that — "all" deletes every empty directory too. One
	// nanosecond keeps the empty-dir rule live without ageing anything in:
	// a directory emptied during THIS purge has its mtime bumped to now,
	// so it still ages for a later pass like any other.
	dirAge := age
	if dirAge <= 0 {
		dirAge = time.Nanosecond
	}
	now := time.Now()
	var files, dirsDeleted int
	capped := false
	for _, d := range dirs {
		res, err := scratch.SweepTree(s.cfg.Scratchpad.Root, user.ID, d, scratch.SweepSpec{
			Now:        now,
			FileMaxAge: age,
			DirMaxAge:  dirAge,
			MaxFiles:   purgeMaxFiles,
			MaxEntries: purgeMaxEntries,
			Warn:       s.sweepWarn(s.cfg.Scratchpad.Root, user.ID),
		})
		if errors.Is(err, scratch.ErrNotFound) {
			continue // no such directory yet — nothing to delete, not an error
		}
		if err != nil {
			s.logger.Error("file purge", "error", err, "kind", d, "rid", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusInternalServerError, "internal", "purge failed")
			return
		}
		files += res.Files
		dirsDeleted += res.Dirs
		capped = capped || res.Capped || res.EntriesCapped
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files_deleted": files,
		"dirs_deleted":  dirsDeleted,
		"capped":        capped,
	})
}
