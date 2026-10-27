package server

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gabkeep/internal/id"
	"gabkeep/internal/scratch"
)

// --- The physical file API (§5.3): upload, list, download, delete ----------
//
// The disk is the single source of truth: a file exists iff its bytes
// exist, its identity is its path (kind = top-level directory), and the
// Files page lists exactly what a walk finds. There is no registry, no
// ids, no cursor — each directory listing is capped like the old Work
// view. Ownership is structural: ?path= resolves only inside the SESSION
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
	fileListMax  = 500 // entries per directory per response (no pagination)
	fileListDeep = 4   // directory depth explored below a layout dir
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
	nameCollisionTries       = 200 // bump attempts before admitting defeat
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
	used, err := scratch.DirSize(s.cfg.Scratchpad.Root, user.ID)
	if err != nil {
		s.logger.Error("quota read failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInsufficientStorage, "storage_unverified",
			"Storage usage could not be verified; check scratchpad permissions")
		return
	}
	if used >= int64(s.cfg.Scratchpad.UserQuota) {
		writeAPIError(w, r, http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded")
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

// commitUpload is the atomic quota decision for an upload (audit finding
// 5): usage is re-read FRESH and the placement executed while holding
// commitMu, shared by every upload commit — so N concurrent requests can
// no longer all spend the same stale "used" figure (the audit stored
// 1,400 bytes under a 1,024-byte quota). The name is chosen by colliding
// onto the disk: PlaceNoReplace lands the temp under
// sanitize→BumpName(display) atomically or reports ErrExists, so two
// same-named uploads become "report.pdf" and "report (1).pdf" with no
// overwrite window. Returns the FINAL rel path.
func (s *Server) commitUpload(w http.ResponseWriter, r *http.Request,
	userID, tempName, display string, n int64,
) (rel string, ok bool) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	// Fail closed (audit2 finding 3): if usage cannot be walked, the
	// upload is refused — not treated as zero — until the tree is readable
	// again. No bytes move, so no cleanup is needed beyond temp removal by
	// the caller.
	used, err := scratch.DirSize(s.cfg.Scratchpad.Root, userID)
	if err != nil {
		s.logger.Error("quota read failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInsufficientStorage, "storage_unverified",
			"Storage usage could not be verified; check scratchpad permissions")
		return "", false
	}
	if used+n > int64(s.cfg.Scratchpad.UserQuota) {
		writeAPIError(w, r, http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded")
		return "", false
	}
	for i := 0; i < nameCollisionTries; i++ {
		cand := "uploads/" + scratch.BumpName(display, i)
		perr := scratch.PlaceNoReplace(s.cfg.Scratchpad.Root, userID, cand, tempName)
		if perr == nil {
			return cand, true
		}
		if !errors.Is(perr, scratch.ErrExists) {
			s.logger.Error("upload place", "error", perr, "rid", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
			return "", false
		}
	}
	// 200 same-named uploads is a flood or a pathological tree; asking the
	// user to rename is the only honest answer.
	writeAPIError(w, r, http.StatusConflict, "name_conflict",
		"too many files already share this name; rename the file and try again")
	return "", false
}

type multipartPart struct {
	display string
	reader  io.ReadCloser
}

// --- GET /api/v1/files?kind= — one physical walk (§5.3) --------------------

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	kind := r.URL.Query().Get("kind")
	if kind != "" && !isFileDir(kind) {
		writeAPIError(w, r, http.StatusBadRequest, "bad_kind", "kind must be uploads, artifacts or work")
		return
	}
	dirs := fileDirs
	if kind != "" {
		dirs = []string{kind}
	}
	var items []map[string]any
	truncated := false
	for _, d := range dirs {
		its, trunc := dirEntries(s.cfg.Scratchpad.Root, user.ID, d, s.artifactTTL())
		truncated = truncated || trunc
		items = append(items, its...)
	}
	// All timestamps are RFC3339-UTC from our own formatter, so
	// lexicographic order equals chronological order.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i]["modified_at"].(string) > items[j]["modified_at"].(string)
	})
	used, _ := scratch.DirSize(s.cfg.Scratchpad.Root, user.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     items,
		"truncated": truncated,
		"quota":     map[string]any{"used_bytes": used, "total_bytes": int64(s.cfg.Scratchpad.UserQuota)},
	})
}

// dirEntries walks one layout directory (uploads/artifacts/work) and
// returns display entries sorted newest-modified first, plus whether the
// fileListMax cap was hit. Files carry the directory's singular kind
// ("upload"/"artifact"/"work"); DIRECTORIES are listed too — including
// empty ones — as kind "dir" with a bounded file count, so script-made
// folders are visible and deletable instead of becoming invisible litter.
// Artifact rows carry a COMPUTED expires_at (mtime + artifact_ttl): the
// sweep is mtime-based now, and the UI's "expires …" line stays honest.
// Metadata only — §9.7 keeps Resolve safe for display/walking, while
// download/delete re-open fd-pinned. Hidden files (dot-prefixed leftovers),
// symlinks and non-regular files stay out of the list.
func dirEntries(root, userID, dir string, artifactTTL time.Duration) ([]map[string]any, bool) {
	// The walk root is config root + session id + one of the three fixed
	// dir names. Gate the middle component on the ULID alphabet anyway
	// (§9.7): the walk must never be steered by anything that is not a
	// bare id, whoever called.
	if !id.Valid(userID) || !isFileDir(dir) {
		return nil, false
	}
	base, err := scratch.Resolve(root, userID, dir)
	if err != nil {
		return nil, false
	}
	type entry struct {
		mtime time.Time
		json  map[string]any
	}
	var entries []entry
	truncated := false
	// #nosec G104,G703 -- G104: the walk callback swallows per-entry errors
	// by design. G703: walk root fully gated above — config root + id.Valid
	// user id + a whitelisted dir name; the callback only reads walk-issued
	// entries (stat for metadata), and no request-derived path is ever
	// opened here — download/delete take the fd-pinned sandbox separately.
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries do not fail the listing
		}
		sub, rerr := filepath.Rel(base, p)
		if rerr != nil || sub == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.Count(sub, string(filepath.Separator)) >= fileListDeep {
				return fs.SkipDir
			}
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			if len(entries) >= fileListMax {
				truncated = true
				return filepath.SkipAll
			}
			n, capped := countTreeFiles(p)
			entries = append(entries, entry{mtime: info.ModTime(), json: map[string]any{
				"kind":         "dir",
				"path":         dir + "/" + filepath.ToSlash(sub),
				"name":         d.Name(),
				"dir":          true,
				"files":        n,
				"files_capped": capped,
				"modified_at":  info.ModTime().UTC().Format(timeRFC3339),
			}})
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(entries) >= fileListMax {
			truncated = true
			return filepath.SkipAll
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel := dir + "/" + filepath.ToSlash(sub)
		item := map[string]any{
			"kind":         fileKindOf(dir),
			"path":         rel,
			"name":         d.Name(),
			"size":         info.Size(),
			"modified_at":  info.ModTime().UTC().Format(timeRFC3339),
			"download_url": fileDownloadURL(rel),
		}
		if dir == "artifacts" {
			item["expires_at"] = info.ModTime().Add(artifactTTL).UTC().Format(timeRFC3339)
		}
		entries = append(entries, entry{mtime: info.ModTime(), json: item})
		return nil
	})
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].mtime.After(entries[j].mtime) })
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, e.json)
	}
	return items, truncated
}

// countTreeFiles counts regular files below dir for the folder rows'
// "everything inside will be deleted (N files)" confirmation copy. It is
// display metadata, not an enforcement read: walk errors stop the count
// (the delete itself re-walks under §9.7 and refuses anything it cannot
// fully enumerate), and the count is capped so a pathological tree cannot
// make listing expensive.
func countTreeFiles(dir string) (int, bool) {
	const cap = 999
	n, capped := 0, false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		n++
		if n >= cap {
			capped = true
			return filepath.SkipAll
		}
		return nil
	})
	return n, capped
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
	_, _ = io.Copy(w, io.MultiReader(bytes.NewReader(head[:n]), f))
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
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
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
