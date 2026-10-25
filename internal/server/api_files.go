package server

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"gabkeep/internal/id"
	"gabkeep/internal/scratch"
	"gabkeep/internal/store"
)

// --- GET /api/v1/files | POST /api/v1/files | download | DELETE (§5.3) ------

// mediaTypeForUpload classifies a part: content sniffing first, then an
// extension hint for common text types sniffing labels octet-stream.
func mediaTypeForUpload(sniff []byte, name string) string {
	mt := http.DetectContentType(sniff)
	ext := strings.ToLower(path.Ext(name))
	// CSV bytes sniff as plain text; the extension disambiguates. Safe to
	// trust for inline display because the media type comes from us and
	// nosniff prevents browser reinterpretation.
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

// sanitizeDisplayName keeps a display-only filename safe for JSON + UI:
// basename, no control chars, bounded length (§9.4: never a path component).
func sanitizeDisplayName(raw string) string {
	name := filepath.Base(strings.ReplaceAll(raw, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
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

// diskFloorOK reports whether free space above root is above the 5% floor
// (§9.7); stat failures fail open here — quota still bounds growth.
func diskFloorOK(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return true
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	return total == 0 || free*20 >= total // free >= 5 %
}

func fileJSON(f *store.File) map[string]any {
	j := map[string]any{
		"id":              f.ID,
		"kind":            f.Kind,
		"name":            f.Name,
		"media_type":      f.MediaType,
		"size":            f.Size,
		"created_at":      f.CreatedAt.UTC().Format(timeRFC3339),
		"download_url":    "/api/v1/files/" + f.ID + "/download",
		"conversation_id": f.ConversationID,
	}
	if !f.ExpiresAt.IsZero() {
		j["expires_at"] = f.ExpiresAt.UTC().Format(timeRFC3339)
	}
	return j
}

// handleFileUpload streams a single multipart "file" part to the scratchpad
// (§9.11: never buffered whole; the bodyLimit middleware exempts this route
// because the applicable cap is max_file_size, not max_body_bytes).
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if !diskFloorOK(s.cfg.Scratchpad.Root) {
		writeAPIError(w, r, http.StatusServiceUnavailable, "storage_low", "server storage is nearly full")
		return
	}
	used, err := scratch.DirSize(s.cfg.Scratchpad.Root, user.ID)
	if err != nil {
		s.logger.Error("quota read failed", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if used >= int64(s.cfg.Scratchpad.UserQuota) {
		writeAPIError(w, r, http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded")
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "bad_multipart", "multipart/form-data with a \"file\" part required")
		return
	}
	var part *multipartPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeAPIError(w, r, http.StatusBadRequest, "bad_multipart", "malformed multipart body")
			return
		}
		if p.FormName() == "file" && p.FileName() != "" {
			part = &multipartPart{display: sanitizeDisplayName(p.FileName()), reader: p}
			break
		}
		p.Close() // form fields we do not use
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
	defer os.Remove(tempName) // no-op after a successful rename

	// Sniff, hash and size in one streaming pass; cap at max_file_size + 1
	// so "exactly too big" is distinguishable from merely oversized.
	max := int64(s.cfg.Scratchpad.MaxFileSize)
	head := make([]byte, 512)
	nHead, _ := io.ReadFull(part.reader, head)
	hash := sha256.New()
	tee := io.MultiWriter(tmp, hash)
	n, err := io.Copy(tee, io.LimitReader(io.MultiReader(
		bytes.NewReader(head[:nHead]), part.reader), max+1))
	tmp.Close()
	switch {
	case n > max:
		writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds the size limit")
		return
	case err != nil:
		writeAPIError(w, r, http.StatusBadRequest, "upload_failed", "upload could not be read")
		return
	}
	if used+n > int64(s.cfg.Scratchpad.UserQuota) {
		writeAPIError(w, r, http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded")
		return
	}

	fileID := id.New()
	rel := "uploads/" + fileID + ext
	if err := os.Rename(tempName, filepath.Join(s.cfg.Scratchpad.Root, user.ID, rel)); err != nil {
		s.logger.Error("upload rename", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Re-check the floor after landing bytes: refuse service for the next
	// upload rather than corrupt state mid-write (§9.7).
	if !diskFloorOK(s.cfg.Scratchpad.Root) {
		s.logger.Warn("disk floor breached right after upload", "rid", RequestIDFrom(r.Context()))
	}

	rec := &store.File{
		ID: fileID, UserID: user.ID, Kind: store.FileKindUpload, RelPath: rel,
		MediaType: mediaTypeForUpload(head[:nHead], part.display),
		Name:      part.display, Size: n, SHA256: hash.Sum(nil), CreatedAt: time.Now(),
	}
	if err := s.store.CreateFile(r.Context(), rec); err != nil {
		// Roll the physical file back so no orphan can exist on DB failure.
		os.Remove(filepath.Join(s.cfg.Scratchpad.Root, user.ID, rel))
		s.logger.Error("file row insert", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.logger.Info("upload ok", "file", fileID, "bytes", n, "user", user.ID, "rid", RequestIDFrom(r.Context()))
	writeJSON(w, http.StatusCreated, fileJSON(rec))
}

type multipartPart struct {
	display string
	reader  io.ReadCloser
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != store.FileKindUpload && kind != store.FileKindArtifact {
		writeAPIError(w, r, http.StatusBadRequest, "bad_kind", "kind must be upload or artifact")
		return
	}
	items, next, err := s.store.ListFiles(r.Context(), user.ID, kind, r.URL.Query().Get("cursor"), 50)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		out = append(out, fileJSON(&items[i]))
	}
	used, _ := scratch.DirSize(s.cfg.Scratchpad.Root, user.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       out,
		"next_cursor": next,
		"quota":       map[string]any{"used_bytes": used, "total_bytes": int64(s.cfg.Scratchpad.UserQuota)},
	})
}

// inlineSafeTypes is the ONLY media types ever served inline (§9.4); html,
// svg, xhtml and xml are never inline regardless of what is stored — a
// shared "document" must never execute in the app's origin.
var inlineSafeTypes = map[string]bool{
	"text/plain": true, "text/csv": true,
	"image/png": true, "image/jpeg": true, "image/webp": true,
	"application/pdf": true,
}

// neverInlineExts: belt-and-braces beside the media-type allowlist — these
// extensions are NEVER inline no matter what the row claims (§9.4).
var neverInlineExts = map[string]bool{
	".html": true, ".htm": true, ".svg": true, ".xhtml": true, ".xml": true,
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	f, err := s.store.GetFileForUser(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	}
	file, err := scratch.OpenFile(s.cfg.Scratchpad.Root, user.ID, f.RelPath)
	if err != nil {
		// Row without a file (crash between delete phases) ⇒ treat as gone.
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	}
	defer file.Close()

	inline := r.URL.Query().Get("inline") == "1" && inlineSafeTypes[f.MediaType] &&
		!neverInlineExts[strings.ToLower(path.Ext(f.RelPath))]
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("Content-Disposition",
		mineDisposition(disp, f.Name))
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, file)
}

// mineDisposition builds Content-Disposition with an RFC 5987 UTF-8 name so
// any display name is injection-safe (quotes, newlines impossible).
func mineDisposition(kind, name string) string {
	return mime.FormatMediaType(kind, map[string]string{"filename": name})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	f, err := s.store.DeleteFileForUser(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrFileNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
			return
		}
		s.logger.Error("file delete", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := scratch.Remove(s.cfg.Scratchpad.Root, user.ID, f.RelPath); err != nil &&
		!errors.Is(err, scratch.ErrNotFound) {
		s.logger.Warn("unlink after row delete failed", "error", err, "rid", RequestIDFrom(r.Context()))
	}
	w.WriteHeader(http.StatusNoContent)
}
