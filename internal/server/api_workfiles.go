package server

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gabkeep/internal/scratch"
)

// --- GET /api/v1/files/work | .../work/download | DELETE .../work (§5.3) ----
//
// A PHYSICAL view of the user's work/ scratch dir. The files table registers
// curated deliverables (uploads, shared artifacts); scripts and fs_write
// leave most work/ output without a row, so nothing in the registry can show
// it. These endpoints list, serve and delete work files as they exist on
// disk, reusing the fd-pinned sandbox (§9.7) and the inline-safety allowlist
// (§9.4) unchanged. No rows are created: viewing is not sharing.

const (
	workListMax  = 500 // entries per response
	workListDeep = 4   // directory depth explored below work/
)

// workPathFrom validates the ?path= parameter: a scratchpad-relative path at
// least two segments deep whose first segment is exactly "work" (so the work
// dir itself can never be addressed for download or removal). Every
// rejection answers 404, matching the row endpoints' stance of never
// distinguishing "no such thing" from "not yours".
func workPathFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	segs, err := scratch.ParseRel(r.URL.Query().Get("path"))
	if err != nil || len(segs) < 2 || segs[0] != "work" {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return "", false
	}
	return strings.Join(segs, "/"), true
}

func (s *Server) handleWorkList(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if err := scratch.EnsureUserDirs(s.cfg.Scratchpad.Root, user.ID); err != nil {
		s.logger.Error("provision scratchpad", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	dir, err := scratch.Resolve(s.cfg.Scratchpad.Root, user.ID, "work")
	if err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	type entry struct {
		mtime time.Time
		json  map[string]any
	}
	var entries []entry
	truncated := false
	// WalkDir never follows symlinks; entries are metadata only (§9.7 keeps
	// Resolve for display/walking safe, while download/delete re-open
	// fd-pinned). Hidden files (dot-prefixed, e.g. tmp leftovers) are noise
	// and stay out of the list; so do symlinks and anything non-regular.
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries do not fail the listing
		}
		sub, rerr := filepath.Rel(dir, p)
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
			if strings.Count(sub, string(filepath.Separator)) >= workListDeep {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(entries) >= workListMax {
			truncated = true
			return filepath.SkipAll
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel := "work/" + filepath.ToSlash(sub)
		entries = append(entries, entry{mtime: info.ModTime(), json: map[string]any{
			"kind":         "work",
			"path":         rel,
			"name":         d.Name(),
			"size":         info.Size(),
			"modified_at":  info.ModTime().UTC().Format(timeRFC3339),
			"download_url": "/api/v1/files/work/download?path=" + url.QueryEscape(rel),
		}})
		return nil
	})
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].mtime.After(entries[j].mtime) })

	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, e.json)
	}
	used, _ := scratch.DirSize(s.cfg.Scratchpad.Root, user.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"truncated":  truncated,
		"quota":      map[string]any{"used_bytes": used, "total_bytes": int64(s.cfg.Scratchpad.UserQuota)},
	})
}

func (s *Server) handleWorkDownload(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	rel, ok := workPathFrom(w, r)
	if !ok {
		return
	}
	fi, err := scratch.Stat(s.cfg.Scratchpad.Root, user.ID, rel)
	if err != nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	}
	// Regular files only: a script could plant a FIFO at this path, and
	// opening one for read would block serving until a writer appears.
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

	// Classify like an upload (sniff + extension hint); rows do not exist to
	// carry a media type here.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	name := filepath.Base(rel)
	mt := mediaTypeForUpload(head[:n], name)
	// The sniffed type may carry parameters ("text/plain; charset=utf-8");
	// the §9.4 allowlist is keyed on the bare type.
	base := strings.TrimSpace(strings.Split(mt, ";")[0])
	inline := r.URL.Query().Get("inline") == "1" && inlineSafeTypes[base] &&
		!neverInlineExts[strings.ToLower(filepath.Ext(rel))]
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Content-Disposition", mineDisposition(disp, name))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, io.MultiReader(bytes.NewReader(head[:n]), f))
}

func (s *Server) handleWorkDelete(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	rel, ok := workPathFrom(w, r)
	if !ok {
		return
	}
	if fi, err := scratch.Stat(s.cfg.Scratchpad.Root, user.ID, rel); err != nil {
		writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
		return
	} else if fi.IsDir() {
		writeAPIError(w, r, http.StatusBadRequest, "not_a_file", "path is a directory")
		return
	}
	if err := scratch.Remove(s.cfg.Scratchpad.Root, user.ID, rel); err != nil {
		if errors.Is(err, scratch.ErrNotFound) || errors.Is(err, scratch.ErrPathUnsafe) {
			writeAPIError(w, r, http.StatusNotFound, "not_found", "file not found")
			return
		}
		s.logger.Error("work delete", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// files_share can register an artifact row pointing INTO work/; deleting
	// the bytes must delete the row too, or the card dangles forever.
	if art, err := s.store.FindArtifactByPath(r.Context(), user.ID, rel); err == nil {
		if err := s.store.DeleteFilesByID(r.Context(), []string{art.ID}); err != nil {
			s.logger.Warn("artifact row left behind by work delete", "error", err,
				"file", art.ID, "rid", RequestIDFrom(r.Context()))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
