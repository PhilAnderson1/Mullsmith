package server

import (
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"mullsmith/internal/scratch"
)

type fileCursor struct {
	Modified int64  `json:"modified"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
}

type fileEntry struct {
	rel  string
	info fs.FileInfo
}

func fileBefore(a, b fileEntry) bool {
	if !a.info.ModTime().Equal(b.info.ModTime()) {
		return a.info.ModTime().After(b.info.ModTime())
	}
	return a.rel < b.rel
}

// The oldest retained entry is at the root; only one page plus a sentinel
// lives in memory, regardless of how many files the scan visits.
type filePageHeap []fileEntry

func (h filePageHeap) Len() int           { return len(h) }
func (h filePageHeap) Less(i, j int) bool { return fileBefore(h[j], h[i]) }
func (h filePageHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *filePageHeap) Push(v any)        { *h = append(*h, v.(fileEntry)) }
func (h *filePageHeap) Pop() any          { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }

func scanFilePage(ctx context.Context, root, userID, kind string, cursor *fileCursor, limit, budget int) ([]fileEntry, bool, error) {
	dirs := fileDirs
	if kind != "" {
		dirs = []string{kind}
	}
	selected := &filePageHeap{}
	for _, dir := range dirs {
		err := scratch.Walk(ctx, root, userID, dir, budget, func(rel string, info fs.FileInfo) error {
			if strings.HasPrefix(info.Name(), ".") || (info.IsDir() && strings.Count(rel, "/") > fileListDeep) {
				if info.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			if cursor != nil {
				stamp := info.ModTime().UnixNano()
				if stamp > cursor.Modified || (stamp == cursor.Modified && rel <= cursor.Path) {
					return nil
				}
			}
			entry := fileEntry{rel, info}
			if selected.Len() < limit+1 {
				heap.Push(selected, entry)
			} else if fileBefore(entry, (*selected)[0]) {
				(*selected)[0] = entry
				heap.Fix(selected, 0)
			}
			return nil
		})
		if err != nil {
			return nil, false, err
		}
	}
	entries := []fileEntry(*selected)
	sort.Slice(entries, func(i, j int) bool { return fileBefore(entries[i], entries[j]) })
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	return entries, more, nil
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	kind := r.URL.Query().Get("kind")
	if kind != "" && !isFileDir(kind) {
		writeAPIError(w, r, http.StatusBadRequest, "bad_kind", "kind must be uploads, artifacts or work")
		return
	}
	raw, limit := listParams(r)
	if limit < 1 {
		limit = filePageDefault
	}
	if limit > filePageMax {
		limit = filePageMax
	}
	var cursor *fileCursor
	if raw != "" {
		if len(raw) > 4096 {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_cursor", "invalid file cursor")
			return
		}
		var parsed fileCursor
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(decoded, &parsed) != nil || parsed.Path == "" || parsed.Kind != kind {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_cursor", "invalid file cursor")
			return
		}
		cursor = &parsed
	}
	entries, more, err := scanFilePage(r.Context(), s.cfg.Scratchpad.Root, user.ID, kind, cursor, limit, fileListScanMax)
	if err != nil {
		s.logger.Warn("file listing incomplete", "error", err, "rid", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusServiceUnavailable, "listing_unavailable", "File listing could not be completed within its scan limits or permissions; reduce the directory contents or check permissions.")
		return
	}
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		info := entry.info
		item := map[string]any{"kind": fileKindOf(strings.SplitN(entry.rel, "/", 2)[0]), "path": entry.rel, "name": info.Name(), "modified_at": info.ModTime().UTC().Format(time.RFC3339Nano)}
		if info.IsDir() {
			n, capped := countTreeFiles(r.Context(), s.cfg.Scratchpad.Root, user.ID, entry.rel)
			item["kind"], item["dir"], item["files"], item["files_capped"] = "dir", true, n, capped
		} else {
			item["size"], item["download_url"] = info.Size(), fileDownloadURL(entry.rel)
			if strings.HasPrefix(entry.rel, "artifacts/") {
				item["expires_at"] = info.ModTime().Add(s.artifactTTL()).UTC().Format(timeRFC3339)
			}
		}
		items = append(items, item)
	}
	next := ""
	if more {
		last := entries[len(entries)-1]
		b, _ := json.Marshal(fileCursor{Modified: last.info.ModTime().UnixNano(), Path: last.rel, Kind: kind})
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	used, err := scratch.DirSize(r.Context(), s.cfg.Scratchpad.Root, user.ID)
	if err != nil {
		writeAPIError(w, r, http.StatusServiceUnavailable, "storage_unverified", "Storage usage could not be verified; check scratchpad permissions.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next, "quota": map[string]any{"used_bytes": used, "total_bytes": int64(s.cfg.Scratchpad.UserQuota)}})
}

func countTreeFiles(ctx context.Context, root, userID, rel string) (int, bool) {
	n := 0
	err := scratch.Walk(ctx, root, userID, rel, 10_000, func(_ string, info fs.FileInfo) error {
		if info.Mode().IsRegular() {
			n++
			if n >= 999 {
				return fs.SkipAll
			}
		}
		return nil
	})
	return n, n >= 999 || err != nil
}
