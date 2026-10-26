package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/id"
	"gabkeep/internal/scratch"
	"gabkeep/internal/store"
)

// seedWork writes a file straight into the user's work/ dir (the way a
// jailed script would: bytes on disk, no files-table row).
func seedWork(t *testing.T, cfg *config.Config, userID, rel string, content []byte) {
	t.Helper()
	f, err := scratch.CreateFile(cfg.Scratchpad.Root, userID, rel, 0o600)
	if err != nil {
		t.Fatalf("seed %s: %v", rel, err)
	}
	f.Write(content)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func workItems(t *testing.T, a *authTest) []any {
	t.Helper()
	resp := a.do(http.MethodGet, "/api/v1/files/work", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("work list: %d %v", resp.StatusCode, decodedBody(t, resp))
	}
	j := decodedBody(t, resp)
	items, _ := j["items"].([]any)
	if items == nil {
		t.Fatalf("no items in %v", j)
	}
	return items
}

func TestWorkList(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "w1@test.io", "gk-code-1")
	uid := meID(a)

	seedWork(t, cfg, uid, "work/old.txt", []byte("older"))
	seedWork(t, cfg, uid, "work/sub/deep.txt", []byte("nested"))
	time.Sleep(20 * time.Millisecond) // guarantee distinct mtimes
	seedWork(t, cfg, uid, "work/newer.csv", []byte("a,b\n"))
	seedWork(t, cfg, uid, "work/.hidden", []byte("secret-ish"))
	// Symlink and FIFO must never appear in the list.
	userRoot := filepath.Join(cfg.Scratchpad.Root, uid)
	if err := os.Symlink("newer.csv", filepath.Join(userRoot, "work", "link.csv")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(userRoot, "work", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	items := workItems(t, a)
	// 3 regular non-hidden files + the work/sub FOLDER row (empty or not,
	// directories are listed so the UI can clean them up).
	if len(items) != 4 {
		t.Fatalf("items = %v, want 3 files + 1 folder", items)
	}
	first, _ := items[0].(map[string]any)
	if first["name"] != "newer.csv" || first["kind"] != "work" ||
		first["path"] != "work/newer.csv" || first["size"].(float64) != 4 {
		t.Errorf("newest entry = %v", first)
	}
	if dl, _ := first["download_url"].(string); dl != "/api/v1/files/work/download?path=work%2Fnewer.csv" {
		t.Errorf("download_url = %v", dl)
	}
	// Nested path visible with its slash-joined rel path.
	var found bool
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["path"] == "work/sub/deep.txt" {
			found = true
		}
		for _, banned := range []string{".hidden", "link.csv", "pipe"} {
			if strings.Contains(m["path"].(string), banned) {
				t.Errorf("hidden/symlink/fifo leaked into list: %v", m)
			}
		}
	}
	if !found {
		t.Errorf("nested work file missing from list: %v", items)
	}
	if q, _ := decodedBody(t, a.do(http.MethodGet, "/api/v1/files/work", nil))["quota"].(map[string]any); q == nil {
		t.Error("quota block missing")
	}
}

func TestWorkListEmptyDir(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "w2@test.io", "gk-code-1")
	items := workItems(t, a) // user exists but never wrote anything
	if len(items) != 0 {
		t.Errorf("fresh user work list = %v, want empty", items)
	}
}

func TestWorkDownload(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "w3@test.io", "gk-code-1")
	uid := meID(a)
	body := []byte("print('hi')\n")
	seedWork(t, cfg, uid, "work/tool.py", body)
	seedWork(t, cfg, uid, "work/page.html", []byte("<script>alert(1)</script>"))
	if err := syscall.Mkfifo(filepath.Join(cfg.Scratchpad.Root, uid, "work", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	resp := a.do(http.MethodGet, "/api/v1/files/work/download?path=work%2Ftool.py", nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("download: %d %q", resp.StatusCode, got)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}

	// Inline allow still applies...
	resp = a.do(http.MethodGet, "/api/v1/files/work/download?path=work%2Ftool.py&inline=1", nil)
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("txt inline: Content-Disposition = %q", cd)
	}
	// ...and active content is NEVER inline (§9.4), row or no row.
	resp = a.do(http.MethodGet, "/api/v1/files/work/download?path=work%2Fpage.html&inline=1", nil)
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("html must never be inline: %q", cd)
	}

	// A FIFO would block an open-for-read forever; it must be refused.
	resp = a.do(http.MethodGet, "/api/v1/files/work/download?path=work%2Fpipe", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("fifo download: %d, want 404", resp.StatusCode)
	}

	// Everything outside work/ — other dirs, traversal, absolute paths, the
	// work dir itself — is a uniform 404.
	for _, p := range []string{
		"uploads/x.csv", "work", "../uploads/x.csv", "work/../uploads/x.csv",
		"/etc/passwd", "work//", "artifacts/a.txt",
	} {
		resp = a.do(http.MethodGet, "/api/v1/files/work/download?path="+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("download path %q: %d, want 404", p, resp.StatusCode)
		}
	}
	// Missing file inside work/ ⇒ 404 too.
	resp = a.do(http.MethodGet, "/api/v1/files/work/download?path=work%2Fgone.txt", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing file: %d, want 404", resp.StatusCode)
	}
}

func TestWorkDelete(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "w4@test.io", "gk-code-1")
	uid := meID(a)
	userRoot := filepath.Join(cfg.Scratchpad.Root, uid)

	seedWork(t, cfg, uid, "work/temp.txt", []byte("junk"))
	resp := a.do(http.MethodDelete, "/api/v1/files/work?path=work%2Ftemp.txt", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(userRoot, "work", "temp.txt")); !os.IsNotExist(err) {
		t.Errorf("bytes survived delete: %v", err)
	}

	// An artifact row registered IN work/ (files_share does this) must go
	// with the bytes, or its chat card would dangle.
	seedWork(t, cfg, uid, "work/shared.txt", []byte("shared output"))
	if err := a.db.CreateFile(context.Background(), &store.File{
		ID: id.New(), UserID: uid, Kind: store.FileKindArtifact, RelPath: "work/shared.txt",
		MediaType: "text/plain", Name: "shared.txt", Size: 13,
		SHA256: []byte("x"), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	resp = a.do(http.MethodDelete, "/api/v1/files/work?path=work%2Fshared.txt", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete shared: %d", resp.StatusCode)
	}
	if _, err := a.db.FindArtifactByPath(context.Background(), uid, "work/shared.txt"); err != store.ErrFileNotFound {
		t.Errorf("artifact row survived work delete: err = %v", err)
	}

	// Outside work/: refused, bytes untouched.
	uploadResp := a.upload("keep.csv", []byte("a,b\n"))
	uploadResp.Body.Close()
	rowDir := filepath.Join(userRoot, "uploads")
	kept, _ := filepath.Glob(filepath.Join(rowDir, "*"))
	resp = a.do(http.MethodDelete, "/api/v1/files/work?path=uploads%2Fx.csv", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete outside work: %d, want 404", resp.StatusCode)
	}
	after, _ := filepath.Glob(filepath.Join(rowDir, "*"))
	if len(after) != len(kept) || len(kept) == 0 {
		t.Errorf("upload affected by refused delete: %v -> %v", kept, after)
	}
	// Directories ARE deletable now (Files-page cleanup): 204 and gone.
	if err := os.Mkdir(filepath.Join(userRoot, "work", "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	resp = a.do(http.MethodDelete, "/api/v1/files/work?path=work%2Fadir", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete directory: %d, want 204", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(userRoot, "work", "adir")); !os.IsNotExist(err) {
		t.Errorf("directory survived delete: %v", err)
	}
}

func TestAllIncludesWorkFiles(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "w5@test.io", "gk-code-1")
	uid := meID(a)

	up := a.upload("sales.csv", []byte("a,b\n"))
	up.Body.Close()
	time.Sleep(1100 * time.Millisecond) // row created_at is second-granular
	seedWork(t, cfg, uid, "work/script_out.txt", []byte("unregistered"))
	// A work file that IS registered (files_share registers in place): the
	// All view must show the row, not the physical file too.
	seedWork(t, cfg, uid, "work/shared.txt", []byte("shared"))
	if err := a.db.CreateFile(context.Background(), &store.File{
		ID: id.New(), UserID: uid, Kind: store.FileKindArtifact, RelPath: "work/shared.txt",
		MediaType: "text/plain", Name: "shared.txt", Size: 7,
		SHA256: []byte("x"), CreatedAt: time.Now().Add(-time.Hour), // predates the work file
	}); err != nil {
		t.Fatal(err)
	}

	items, _ := decodedBody(t, a.do(http.MethodGet, "/api/v1/files", nil))["items"].([]any)
	if len(items) != 3 { // upload row + artifact row + unregistered work file
		t.Fatalf("All items = %v, want 3", items)
	}
	kinds := map[string]string{} // name → kind
	for _, it := range items {
		m, _ := it.(map[string]any)
		kinds[m["name"].(string)] = m["kind"].(string)
	}
	if kinds["script_out.txt"] != "work" || kinds["shared.txt"] != "artifact" ||
		kinds["sales.csv"] != "upload" {
		t.Errorf("All merge wrong: %v", kinds)
	}
	// Newest-modified first: the work file was written after the upload.
	first := items[0].(map[string]any)
	if first["name"] != "script_out.txt" {
		t.Errorf("first item = %v, want the work file", first)
	}

	// Kind filters stay registry-only.
	for kind, want := range map[string]int{"upload": 1, "artifact": 1} {
		lj := decodedBody(t, a.do(http.MethodGet, "/api/v1/files?kind="+kind, nil))
		got, _ := lj["items"].([]any)
		if len(got) != want {
			t.Errorf("kind=%s items = %v, want %d", kind, got, want)
		}
	}
}

func TestAllPaginationNoWorkDupes(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "w6@test.io", "gk-code-1")
	uid := meID(a)
	// Rows are seeded straight into the DB (identical shape to the upload
	// path) so a 55-item paging test does not fight the HTTP upload limiter.
	for i := 0; i < 55; i++ {
		fid := id.New()
		if err := a.db.CreateFile(context.Background(), &store.File{
			ID: fid, UserID: uid, Kind: store.FileKindUpload,
			RelPath: "uploads/" + fid + ".txt", MediaType: "text/plain",
			Name: fmt.Sprintf("row%02d.txt", i), Size: 1,
			SHA256: []byte("x"), CreatedAt: time.Now().Add(-time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedWork(t, cfg, uid, "work/only_once.txt", []byte("physical"))

	lj := decodedBody(t, a.do(http.MethodGet, "/api/v1/files", nil))
	page1, _ := lj["items"].([]any)
	if len(page1) != 51 { // 50 row cap + the unregistered work file
		t.Fatalf("page1 len = %d, want 51", len(page1))
	}
	cursor, _ := lj["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("expected a next cursor with 55 rows")
	}
	seen := 0
	count := func(items []any) {
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["path"] == "work/only_once.txt" {
				seen++
			}
		}
	}
	count(page1)
	lj = decodedBody(t, a.do(http.MethodGet, "/api/v1/files?cursor="+cursor, nil))
	page2, _ := lj["items"].([]any)
	count(page2)
	if seen != 1 {
		t.Errorf("work file appeared %d times across pages, want exactly 1", seen)
	}
	if len(page2) != 5 {
		t.Errorf("page2 len = %d, want 5", len(page2))
	}
}

func TestWorkEndpointsRequireAuth(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg) // logged out
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/files/work"},
		{http.MethodGet, "/api/v1/files/work/download?path=work%2Fx"},
		{http.MethodDelete, "/api/v1/files/work?path=work%2Fx"},
	} {
		resp := a.raw(call.method, call.path, nil, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", call.method, call.path, resp.StatusCode)
		}
	}
}
