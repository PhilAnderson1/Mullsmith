package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gabkeep/internal/id"
	"gabkeep/internal/store"
)

// Files-page folder support: script-made directories are listed (incl.
// empty), deletable through the same work-path protections, and deleting a
// tree takes its files_share rows with it — no download cards pointing
// into removed bytes.

func regArtifact(t *testing.T, a *authTest, uid, rel string) {
	t.Helper()
	if err := a.db.CreateFile(context.Background(), &store.File{
		ID: id.New(), UserID: uid, Kind: store.FileKindArtifact, RelPath: rel,
		MediaType: "text/plain", Name: filepath.Base(rel), Size: 1,
		SHA256: []byte("x"), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register %s: %v", rel, err)
	}
}

func folderRows(t *testing.T, a *authTest) map[string]map[string]any {
	t.Helper()
	byPath := map[string]map[string]any{}
	for _, it := range workItems(t, a) {
		m, _ := it.(map[string]any)
		if m["kind"] == "work-dir" {
			byPath[m["path"].(string)] = m
		}
	}
	return byPath
}

func TestWorkListShowsFolders(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "wd1@test.io", "gk-code-1")
	uid := meID(a)

	seedWork(t, cfg, uid, "work/proj/a.txt", []byte("a"))
	seedWork(t, cfg, uid, "work/proj/sub/b.txt", []byte("b"))
	uidRoot := filepath.Join(cfg.Scratchpad.Root, uid)
	if err := os.Mkdir(filepath.Join(uidRoot, "work", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(uidRoot, "work", ".hidden-dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	rows := folderRows(t, a)
	if _, ok := rows["work/.hidden-dir"]; ok {
		t.Errorf("hidden directory leaked into list: %v", rows)
	}
	empty, ok := rows["work/empty"]
	if !ok {
		t.Fatalf("empty folder missing from list: %v", rows)
	}
	if empty["dir"] != true || empty["name"] != "empty" {
		t.Errorf("empty folder row = %v", empty)
	}
	if n, _ := empty["files"].(float64); n != 0 {
		t.Errorf("empty folder files = %v, want 0", empty["files"])
	}
	proj, ok := rows["work/proj"]
	if !ok {
		t.Fatalf("work/proj folder row missing: %v", rows)
	}
	// RECURSIVE count — the confirmation copy promises everything inside.
	if n, _ := proj["files"].(float64); n != 2 {
		t.Errorf("work/proj files = %v, want 2 (recursive)", proj["files"])
	}
	if sub, ok := rows["work/proj/sub"]; !ok || sub["files"].(float64) != 1 {
		t.Errorf("nested folder row = %v", sub)
	}
}

func TestWorkDeleteTreeRemovesBytesAndRows(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "wd2@test.io", "gk-code-1")
	uid := meID(a)
	uidRoot := filepath.Join(cfg.Scratchpad.Root, uid)

	seedWork(t, cfg, uid, "work/del/report.txt", []byte("r"))
	seedWork(t, cfg, uid, "work/del/nested/deep.txt", []byte("d"))
	seedWork(t, cfg, uid, "work/keep.txt", []byte("k"))
	// Prefix collision: starts with "work/del" but is NOT inside it.
	seedWork(t, cfg, uid, "work/del-report.txt", []byte("kk"))
	regArtifact(t, a, uid, "work/del/report.txt")
	regArtifact(t, a, uid, "work/del/nested/deep.txt")
	regArtifact(t, a, uid, "work/keep.txt")
	regArtifact(t, a, uid, "work/del-report.txt")

	resp := a.do(http.MethodDelete, "/api/v1/files/work?path=work%2Fdel", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("tree delete: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(uidRoot, "work", "del")); !os.IsNotExist(err) {
		t.Errorf("tree survived delete: %v", err)
	}
	ctx := context.Background()
	for _, gone := range []string{"work/del/report.txt", "work/del/nested/deep.txt"} {
		if _, err := a.db.FindArtifactByPath(ctx, uid, gone); err != store.ErrFileNotFound {
			t.Errorf("row %s survived tree delete: err = %v", gone, err)
		}
	}
	for _, kept := range []string{"work/keep.txt", "work/del-report.txt"} {
		if _, err := a.db.FindArtifactByPath(ctx, uid, kept); err != nil {
			t.Errorf("row %s wrongly deleted: %v", kept, err)
		}
		if _, err := os.Stat(filepath.Join(uidRoot, kept)); err != nil {
			t.Errorf("bytes %s wrongly deleted: %v", kept, err)
		}
	}
	// The folder rows are gone from the list too.
	if rows := folderRows(t, a); len(rows) != 0 {
		t.Errorf("deleted folders still listed: %v", rows)
	}
}

// A directory literally named "100%" must not LIKE-wildcard its way into
// deleting "1005/…" rows — the prefix is escaped before the match.
func TestWorkDeleteTreePercentNameEscaped(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "wd3@test.io", "gk-code-1")
	uid := meID(a)
	uidRoot := filepath.Join(cfg.Scratchpad.Root, uid)

	seedWork(t, cfg, uid, "work/100%/f.txt", []byte("in"))
	seedWork(t, cfg, uid, "work/1005/x.txt", []byte("out"))
	regArtifact(t, a, uid, "work/100%/f.txt")
	regArtifact(t, a, uid, "work/1005/x.txt")

	resp := a.do(http.MethodDelete, "/api/v1/files/work?path=work%2F100%25", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete '100%%': %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(uidRoot, "work", "100%")); !os.IsNotExist(err) {
		t.Errorf("'100%%' dir survived: %v", err)
	}
	ctx := context.Background()
	if _, err := a.db.FindArtifactByPath(ctx, uid, "work/100%/f.txt"); err != store.ErrFileNotFound {
		t.Errorf("row under '100%%' survived: %v", err)
	}
	if _, err := a.db.FindArtifactByPath(ctx, uid, "work/1005/x.txt"); err != nil {
		t.Errorf("LIKE wildcard over-matched: work/1005/x.txt row deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(uidRoot, "work", "1005", "x.txt")); err != nil {
		t.Errorf("over-match removed bytes too: %v", err)
	}
}

// The directory delete inherits every workPathFrom protection: the work
// root itself, traversal, and anything outside work/ stay 404 — whether
// or not such a directory physically exists.
func TestWorkDeleteTreePathProtections(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "wd4@test.io", "gk-code-1")
	uid := meID(a)
	uidRoot := filepath.Join(cfg.Scratchpad.Root, uid)
	if err := os.Mkdir(filepath.Join(uidRoot, "uploads", "adir"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		"work",                       // work root is unaddressable
		"work%2F..%2Fuploads",        // traversal
		"work%2F..%2Fuploads%2Fadir", // traversal into a real directory
		"uploads%2Fadir",             // non-work directory
		"work%2Fghost",               // missing inside work/
	} {
		resp := a.do(http.MethodDelete, "/api/v1/files/work?path="+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("delete %q: %d, want 404", p, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(uidRoot, "uploads", "adir")); err != nil {
		t.Errorf("refused delete touched uploads/adir: %v", err)
	}
}

// The merged "All" view carries folder rows too, and registered rows now
// expose their relative path so same-named files are tellable apart.
func TestAllViewFoldersAndPaths(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "wd5@test.io", "gk-code-1")
	uid := meID(a)
	uidRoot := filepath.Join(cfg.Scratchpad.Root, uid)

	seedWork(t, cfg, uid, "work/proj2/out.txt", []byte("o"))
	if err := os.Mkdir(filepath.Join(uidRoot, "work", "loose-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	up := a.upload("report.csv", []byte("a,b\n"))
	up.Body.Close()

	resp := a.do(http.MethodGet, "/api/v1/files", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("all list: %d", resp.StatusCode)
	}
	j := decodedBody(t, resp)
	items, _ := j["items"].([]any)
	var sawDir bool
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["kind"] == "work-dir" && m["path"] == "work/loose-dir" {
			sawDir = true
		}
		if m["kind"] == "upload" && m["name"] == "report.csv" {
			p, _ := m["path"].(string)
			if !strings.HasPrefix(p, "uploads/") {
				t.Errorf("upload row path = %q, want uploads/…", p)
			}
		}
	}
	if !sawDir {
		t.Errorf("folder row missing from All view: %v", items)
	}
}
