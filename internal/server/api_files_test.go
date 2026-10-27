package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gabkeep/internal/auth"
	"gabkeep/internal/config"
	"gabkeep/internal/scratch"
)

// scratchCfg is authCfg plus the scratchpad knobs (test configs bypass
// config.Load, so defaults must be set by hand — the server helpers floor
// zero TTLs to 168h/24h, but the sweep tests want the values explicit).
func scratchCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := authCfg(t)
	cfg.Scratchpad.MaxFileSize = 1 << 20
	cfg.Scratchpad.UserQuota = 4 << 20
	cfg.Scratchpad.ArtifactTTL = config.Duration(168 * time.Hour)
	cfg.Scratchpad.WorkTTL = config.Duration(24 * time.Hour)
	cfg.Scratchpad.BlockExtensions = []string{".html", ".svg", ".sh", ".exe"}
	return cfg
}

// upload sends a multipart file through the authTest jar.
func (a *authTest) upload(filename string, content []byte) *http.Response {
	a.t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		a.t.Fatal(err)
	}
	part.Write(content)
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/files", &b)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(auth.CSRFHeaderName, a.csrf)
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := a.ts.Client().Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		a.cookies[c.Name] = c.Value
		if c.Name == auth.CSRFCookieName {
			a.csrf = c.Value
		}
	}
	return resp
}

func decodedBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func signupAs(a *authTest, email, invite string) {
	a.t.Helper()
	a.invite(invite)
	resp := a.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": email, "name": "Test User", "password": "passw0rd-long-enough",
		"invite": invite,
	})
	if resp.StatusCode != http.StatusCreated {
		a.t.Fatalf("register %s: %d %v", email, resp.StatusCode, decodedBody(a.t, resp))
	}
	resp.Body.Close()
}

func meID(a *authTest) string {
	a.t.Helper()
	m := decodedBody(a.t, a.do(http.MethodGet, "/api/v1/auth/me", nil))
	uid, _ := m["id"].(string)
	if uid == "" {
		a.t.Fatal("no user id from /me")
	}
	return uid
}

// seedFile writes a file straight into the user's tree (the way a jailed
// script or files_share copy would: bytes on disk, nothing else).
func seedFile(t *testing.T, cfg *config.Config, userID, rel string, content []byte) {
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

func listItems(t *testing.T, a *authTest, query string) []any {
	t.Helper()
	resp := a.do(http.MethodGet, "/api/v1/files"+query, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list %q: %d %v", query, resp.StatusCode, decodedBody(t, resp))
	}
	j := decodedBody(t, resp)
	items, _ := j["items"].([]any)
	if items == nil {
		items = []any{}
	}
	return items
}

func TestFileUploadDownloadDelete(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "f@test.io", "gk-code-1")
	uid := meID(a)

	body := []byte("name,qty\nwidget,3\ngadget,7\n")
	resp := a.upload("sales.csv", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %v", resp.StatusCode, decodedBody(t, resp))
	}
	j := decodedBody(t, resp)
	// The REAL filename lives on disk (§8): no id-mangles-name anymore.
	if j["name"] != "sales.csv" || j["path"] != "uploads/sales.csv" ||
		j["media_type"] != "text/csv" || j["size"].(float64) != float64(len(body)) {
		t.Fatalf("upload JSON = %v", j)
	}
	dl, _ := j["download_url"].(string)
	if dl != "/api/v1/files/download?path=uploads%2Fsales.csv" {
		t.Errorf("download_url = %v", dl)
	}
	onDisk, err := os.ReadFile(filepath.Join(cfg.Scratchpad.Root, uid, "uploads", "sales.csv"))
	if err != nil || !bytes.Equal(onDisk, body) {
		t.Fatalf("bytes on disk: %q, %v", onDisk, err)
	}

	// Download: forced attachment by default, exact bytes.
	resp = a.do(http.MethodGet, dl, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("download: %d %q", resp.StatusCode, got)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv" {
		t.Errorf("Content-Type = %q", ct)
	}

	// ?inline=1 changes nothing (§9.4): downloads are always attachments,
	// so a hostile page can never be opened in the app's origin.
	resp = a.do(http.MethodGet, dl+"&inline=1", nil)
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("csv with inline=1: Content-Disposition = %q, want attachment", cd)
	}

	// List shows the file + quota.
	lj := decodedBody(t, a.do(http.MethodGet, "/api/v1/files", nil))
	items, _ := lj["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list = %v", lj)
	}
	it := items[0].(map[string]any)
	if it["kind"] != "upload" || it["path"] != "uploads/sales.csv" || it["name"] != "sales.csv" {
		t.Errorf("list item = %v", it)
	}
	quota, _ := lj["quota"].(map[string]any)
	if quota["total_bytes"].(float64) != 4<<20 {
		t.Errorf("quota = %v", quota)
	}

	// Delete: bytes gone; download and list agree immediately — the disk
	// is the truth, there is no row left behind to disagree with it.
	resp = a.do(http.MethodDelete, "/api/v1/files?path=uploads%2Fsales.csv", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = a.do(http.MethodGet, dl, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("download after delete: %d", resp.StatusCode)
	}
	resp.Body.Close()
	matches, _ := filepath.Glob(filepath.Join(cfg.Scratchpad.Root, "*", "uploads", "*"))
	if len(matches) != 0 {
		t.Errorf("physical file survived delete: %v", matches)
	}
	if items := listItems(t, a, ""); len(items) != 0 {
		t.Errorf("deleted file still listed: %v", items)
	}
}

// The collision contract: re-uploading a name bumps to " (N).ext" — two
// uploads of the same name are TWO files, never an overwrite.
func TestUploadCollisionBumps(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "bump@test.io", "gk-code-1")
	uid := meID(a)

	want := []string{"report.txt", "report (1).txt", "report (2).txt"}
	for i, name := range want {
		resp := a.upload("report.txt", []byte("v"+string(rune('0'+i))))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %d: %d", i, resp.StatusCode)
		}
		j := decodedBody(t, resp)
		if j["name"] != name || j["path"] != "uploads/"+name {
			t.Errorf("upload %d = %v, want name %q", i, j, name)
		}
	}
	for i, name := range want {
		b, err := os.ReadFile(filepath.Join(cfg.Scratchpad.Root, uid, "uploads", name))
		if err != nil || string(b) != "v"+string(rune('0'+i)) {
			t.Errorf("%s content = %q, %v", name, b, err)
		}
	}
}

func TestUploadBlocklist(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "b@test.io", "gk-code-1")

	for _, name := range []string{"payload.html", "icon.svg", "run.sh", "tool.exe"} {
		resp := a.upload(name, []byte("harmless bytes"))
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("upload %s: %d, want 422", name, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// The FINAL extension decides: "notes.html.txt" is a text file.
	resp := a.upload("notes.html.txt", []byte("plain text"))
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("notes.html.txt: %d, want 201", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// A rejected upload leaves nothing on disk.
	matches, _ := filepath.Glob(filepath.Join(cfg.Scratchpad.Root, "*", "uploads", "*"))
	if len(matches) != 1 {
		t.Errorf("uploads on disk = %v, want only notes.html.txt", matches)
	}
}

// With no block_extensions configured (the default) every file type may
// be uploaded: the download layer's attachment disposition is what keeps
// active content inert, not this gate.
func TestUploadNoBlocklistIsEmptyByDefault(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.Scratchpad.BlockExtensions = nil
	a := newAuthTest(t, cfg)
	signupAs(a, "nolimit@test.io", "gk-code-1")

	for _, name := range []string{"page.html", "icon.svg", "run.sh"} {
		resp := a.upload(name, []byte("harmless bytes"))
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("upload %s with empty block list: %d, want 201", name, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestUploadSizeAndQuota(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.Scratchpad.MaxFileSize = 100
	cfg.Scratchpad.UserQuota = 150
	a := newAuthTest(t, cfg)
	signupAs(a, "q@test.io", "gk-code-1")

	resp := a.upload("big.bin", bytes.Repeat([]byte("x"), 101))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: %d, want 413", resp.StatusCode)
	}
	resp.Body.Close()

	resp = a.upload("ok.bin", bytes.Repeat([]byte("y"), 100))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first upload: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = a.upload("more.bin", bytes.Repeat([]byte("z"), 60)) // 160 > 150
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("quota: %d, want 507", resp.StatusCode)
	}
	resp.Body.Close()
	matches, _ := filepath.Glob(filepath.Join(cfg.Scratchpad.Root, "*", "uploads", "*"))
	if len(matches) != 1 {
		t.Errorf("uploads on disk = %v, want only ok.bin", matches)
	}
}

func TestFileOwnership(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "owner@test.io", "gk-code-1")

	resp := a.upload("mine.txt", []byte("secret data"))
	defer resp.Body.Close()
	upPath := decodedBody(t, resp)["path"].(string)
	if upPath != "uploads/mine.txt" {
		t.Fatalf("upload path = %v", upPath)
	}

	// Second user on the same server. Paths are per-user BY CONSTRUCTION —
	// the same string resolves inside THEIR tree — so a cross-user probe is
	// a plain 404 of a file that "does not exist", and the file never shows
	// in their list (§9.2).
	b := newAuthTestOn(t, a.ts, a.db)
	signupAs(b, "other@test.io", "gk-code-2")

	resp = b.do(http.MethodGet, "/api/v1/files/download?path=uploads%2Fmine.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user download: %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp = b.do(http.MethodDelete, "/api/v1/files?path=uploads%2Fmine.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user delete: %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	if items := listItems(t, b, ""); len(items) != 0 {
		t.Errorf("other user list = %v", items)
	}
	// The owner's bytes survived every probe.
	resp = a.do(http.MethodGet, "/api/v1/files/download?path=uploads%2Fmine.txt", nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != "secret data" {
		t.Errorf("owner download after probes: %d %q", resp.StatusCode, got)
	}
}

// Every path that is not a file inside one of the three layout dirs gets
// the uniform 404 — including the layout dirs themselves, so nobody can
// delete "uploads" wholesale.
func TestFilePathProtections(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "pp@test.io", "gk-code-1")
	uid := meID(a)
	seedFile(t, cfg, uid, "work/real.txt", []byte("x"))
	if err := os.Mkdir(filepath.Join(cfg.Scratchpad.Root, uid, "uploads", "adir"), 0o700); err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"", "uploads", "artifacts", "work", // the dirs themselves
		"../uploads/x", "work/../../etc/passwd", "/etc/passwd",
		"notes/x.txt",         // not a layout dir
		"work%2F..%2Fuploads", // traversal, wire-encoded
		"uploads%2F..%2Fwork", // traversal that would land inside work/ anyway
		"work//", "..%2Fsecret",
	}
	for _, p := range bad {
		resp := a.do(http.MethodGet, "/api/v1/files/download?path="+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("download %q: %d, want 404", p, resp.StatusCode)
		}
		resp = a.do(http.MethodDelete, "/api/v1/files?path="+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("delete %q: %d, want 404", p, resp.StatusCode)
		}
	}
	// A missing file inside a layout dir is the same 404.
	resp := a.do(http.MethodGet, "/api/v1/files/download?path=work%2Fgone.txt", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing file: %d, want 404", resp.StatusCode)
	}
	// uploads/adir untouched; work/real.txt untouched.
	if _, err := os.Stat(filepath.Join(cfg.Scratchpad.Root, uid, "uploads", "adir")); err != nil {
		t.Errorf("refused delete touched uploads/adir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Scratchpad.Root, uid, "work", "real.txt")); err != nil {
		t.Errorf("refused ops touched work/real.txt: %v", err)
	}
}

// §9.4 in its final form: every download is an attachment, no matter the
// type or any inline=1 plea — including files planted in the store by
// other tools (bypassing the upload gate entirely).
func TestDownloadAlwaysAttachment(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "xss@test.io", "gk-code-1")
	uid := meID(a)

	seedFile(t, cfg, uid, "artifacts/report.html", []byte("<script>alert(1)</script>"))
	seedFile(t, cfg, uid, "artifacts/icon.svg", []byte("<svg onload=alert(1)></svg>"))
	seedFile(t, cfg, uid, "work/tool.py", []byte("print('hi')\n"))

	for _, rel := range []string{"artifacts%2Freport.html", "artifacts%2Ficon.svg", "work%2Ftool.py"} {
		for _, q := range []string{"", "&inline=1"} {
			resp := a.do(http.MethodGet, "/api/v1/files/download?path="+rel+q, nil)
			resp.Body.Close()
			if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
				t.Errorf("%s%s: Content-Disposition = %q, want attachment", rel, q, cd)
			}
		}
	}
}

// One physical walk per directory: kinds come from the directory, hidden
// entries and special files never appear, folders are listed (even empty)
// so they are manageable instead of litter, filters are the plural
// directory names, and artifacts carry a computed expires_at.
func TestListKindsFiltersAndFolders(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "lk@test.io", "gk-code-1")
	uid := meID(a)
	userRoot := filepath.Join(cfg.Scratchpad.Root, uid)

	seedFile(t, cfg, uid, "work/old.txt", []byte("older"))
	seedFile(t, cfg, uid, "work/proj/deep.txt", []byte("nested"))
	time.Sleep(20 * time.Millisecond) // guarantee distinct mtimes
	seedFile(t, cfg, uid, "work/newer.csv", []byte("a,b\n"))
	seedFile(t, cfg, uid, "work/.hidden", []byte("secret-ish"))
	seedFile(t, cfg, uid, "artifacts/deliver.md", []byte("# done\n"))
	if err := os.Mkdir(filepath.Join(userRoot, "work", "empty-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(userRoot, "work", ".hidden-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("newer.csv", filepath.Join(userRoot, "work", "link.csv")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(userRoot, "work", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := a.upload("sales.csv", []byte("a,b\n"))
	up.Body.Close()

	all := listItems(t, a, "")
	// 1 upload + 1 artifact + 3 work files + 2 work dirs = 7
	if len(all) != 7 {
		t.Fatalf("all items = %v, want 7", all)
	}
	byPath := map[string]map[string]any{}
	for _, it := range all {
		m, _ := it.(map[string]any)
		byPath[m["path"].(string)] = m
	}
	for _, banned := range []string{"work/.hidden", "work/link.csv", "work/pipe", "work/.hidden-dir"} {
		if _, ok := byPath[banned]; ok {
			t.Errorf("hidden/symlink/fifo leaked into list: %v", banned)
		}
	}
	if got := byPath["work/old.txt"]; got == nil || got["kind"] != "work" {
		t.Errorf("work item = %v", got)
	}
	if got := byPath["artifacts/deliver.md"]; got == nil || got["kind"] != "artifact" ||
		got["expires_at"] == nil || got["expires_at"] == "" {
		t.Errorf("artifact item must carry expires_at: %v", got)
	}
	if got := byPath["uploads/sales.csv"]; got == nil || got["kind"] != "upload" {
		t.Errorf("upload item = %v", got)
	}
	if _, ok := byPath["uploads/sales.csv"]["expires_at"]; ok {
		t.Error("uploads must never carry an expiry")
	}
	// Folder rows: kind "dir", recursive file count, empty included.
	proj := byPath["work/proj"]
	if proj == nil || proj["kind"] != "dir" || proj["dir"] != true {
		t.Fatalf("folder row missing or wrong: %v", proj)
	}
	if n, _ := proj["files"].(float64); n != 1 {
		t.Errorf("work/proj files = %v, want 1", n)
	}
	if empty := byPath["work/empty-dir"]; empty == nil || empty["files"].(float64) != 0 {
		t.Errorf("empty folder row = %v", empty)
	}
	// Newest-modified first: the upload landed last.
	if first := all[0].(map[string]any); first["path"] != "uploads/sales.csv" {
		t.Errorf("first item = %v, want the fresh upload", first)
	}

	// Filters are the plural directory names; bogus kinds 400.
	for kind, want := range map[string]int{"uploads": 1, "artifacts": 1, "work": 5} {
		items := listItems(t, a, "?kind="+kind)
		if len(items) != want {
			t.Errorf("kind=%s items = %v, want %d", kind, items, want)
		}
		for _, it := range items {
			m := it.(map[string]any)
			if !strings.HasPrefix(m["path"].(string), kind+"/") {
				t.Errorf("kind=%s leaked %v", kind, m)
			}
		}
	}
	resp := a.do(http.MethodGet, "/api/v1/files?kind=upload", nil) // singular is not a dir
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("singular kind filter: %d, want 400", resp.StatusCode)
	}

	// A FIFO would block an open-for-read forever; download refuses it.
	resp = a.do(http.MethodGet, "/api/v1/files/download?path=work%2Fpipe", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("fifo download: %d, want 404", resp.StatusCode)
	}
}

// Directory deletion takes the whole tree with it; siblings and
// prefix-colliding names are untouched. No rows to clean anywhere — the
// bytes are the whole story.
func TestDeleteTree(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "dt@test.io", "gk-code-1")
	uid := meID(a)
	userRoot := filepath.Join(cfg.Scratchpad.Root, uid)

	seedFile(t, cfg, uid, "work/del/report.txt", []byte("r"))
	seedFile(t, cfg, uid, "work/del/nested/deep.txt", []byte("d"))
	seedFile(t, cfg, uid, "work/keep.txt", []byte("k"))
	// Prefix collision: starts with "work/del" but is NOT inside it.
	seedFile(t, cfg, uid, "work/del-report.txt", []byte("kk"))
	// A %-in-name used to need LIKE-escaping for the row cascade; with no
	// rows there is nothing to over-match either — but the bytes still
	// must not leak across directories.
	seedFile(t, cfg, uid, "work/100%/f.txt", []byte("in"))
	seedFile(t, cfg, uid, "work/1005/x.txt", []byte("out"))

	for _, rel := range []string{"work%2Fdel", "work%2F100%25"} {
		resp := a.do(http.MethodDelete, "/api/v1/files?path="+rel, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("tree delete %q: %d", rel, resp.StatusCode)
		}
	}
	for _, gone := range []string{"work/del", "work/100%"} {
		if _, err := os.Stat(filepath.Join(userRoot, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived tree delete: %v", gone, err)
		}
	}
	for _, kept := range []string{"work/keep.txt", "work/del-report.txt", "work/1005/x.txt"} {
		if _, err := os.Stat(filepath.Join(userRoot, kept)); err != nil {
			t.Errorf("%s wrongly deleted: %v", kept, err)
		}
	}
}

func TestFileAPIRequiresAuth(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg) // logged out
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/files"},
		{http.MethodGet, "/api/v1/files/download?path=work%2Fx"},
		{http.MethodDelete, "/api/v1/files?path=work%2Fx"},
		{http.MethodPost, "/api/v1/files"},
	} {
		resp := a.raw(call.method, call.path, nil, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", call.method, call.path, resp.StatusCode)
		}
	}
}

// The disk-name sanitizer: one safe segment, always visible in listings
// (leading dots are folded away — a dotfile would be an unmanageable
// ghost), control characters never survive.
func TestSanitizeUploadName(t *testing.T) {
	cases := map[string]string{
		"":                                "_", // Base("") is "." → dot rule
		"..":                              "_",
		".":                               "_",
		"...x":                            "_x",
		".env":                            "_env",
		"   ":                             "upload",
		"  padded.txt  ":                  "padded.txt",
		"evil\x00name.txt":                "evil_name.txt",
		"a/b/c.txt":                       "c.txt", // basename only
		`C:\evil\name.txt`:                "name.txt",
		"..%2f..%2fsecret":                "_%2f..%2fsecret",
		strings.Repeat("x", 300) + ".txt": strings.Repeat("x", 196) + ".txt",
	}
	for in, want := range cases {
		if got := sanitizeUploadName(in); got != want {
			t.Errorf("sanitizeUploadName(%q) = %q, want %q", truncStr(in, 20), got, want)
		}
	}
}

func truncStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// audit2 finding 3 (accounting bypass): while the user's tree cannot be
// walked, uploads are REFUSED (usage unknown ≠ within quota); once read
// access is restored, uploads proceed — nothing was deleted or rewritten.
func TestUploadRejectedWhenUsageUnverifiable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; needs an unprivileged reader")
	}
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "blind@test.io", "gk-code-1")
	dirs, err := filepath.Glob(filepath.Join(cfg.Scratchpad.Root, "*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("user dirs = %v (%v)", dirs, err)
	}
	work := filepath.Join(dirs[0], "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(work, 0o700) })
	if err := os.Chmod(work, 0o000); err != nil {
		t.Fatal(err)
	}

	resp := a.upload("x.bin", []byte("small"))
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("upload over unreadable tree = %d, want 507", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "storage_unverified") {
		t.Errorf("body = %s, want storage_unverified with a clear message", body)
	}

	if err := os.Chmod(work, 0o700); err != nil {
		t.Fatal(err)
	}
	resp = a.upload("x.bin", []byte("small"))
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("upload after restore = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
}
