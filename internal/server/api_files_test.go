package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gabkeep/internal/auth"
	"gabkeep/internal/config"
	"gabkeep/internal/id"
	"gabkeep/internal/store"
)

// scratchCfg is authCfg plus the scratchpad knobs (test configs bypass
// config.Load, so defaults must be set by hand).
func scratchCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := authCfg(t)
	cfg.Scratchpad.MaxFileSize = 1 << 20
	cfg.Scratchpad.UserQuota = 4 << 20
	cfg.Scratchpad.ArtifactTTL = config.Duration(168 * time.Hour)
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

func TestFileUploadDownloadDelete(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "f@test.io", "gk-code-1")

	body := []byte("name,qty\nwidget,3\ngadget,7\n")
	resp := a.upload("sales.csv", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %v", resp.StatusCode, decodedBody(t, resp))
	}
	j := decodedBody(t, resp)
	fileID, _ := j["id"].(string)
	if fileID == "" || j["kind"] != "upload" || j["name"] != "sales.csv" ||
		j["media_type"] != "text/csv" || j["size"].(float64) != float64(len(body)) {
		t.Fatalf("upload JSON = %v", j)
	}
	dl, _ := j["download_url"].(string)
	if dl != "/api/v1/files/"+fileID+"/download" {
		t.Errorf("download_url = %v", dl)
	}

	// Stored name is the file id (+ext); the display name is metadata (§9.4).
	row, err := a.db.GetFileForUser(context.Background(), meID(a), fileID)
	if err != nil || !strings.HasPrefix(row.RelPath, "uploads/"+fileID) || row.Name != "sales.csv" {
		t.Fatalf("stored row = %+v, %v", row, err)
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

	// Safe type + ?inline=1 ⇒ inline allowed (§9.4 allowlist).
	resp = a.do(http.MethodGet, dl+"?inline=1", nil)
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("csv inline: Content-Disposition = %q", cd)
	}

	// List shows the file + quota.
	lj := decodedBody(t, a.do(http.MethodGet, "/api/v1/files", nil))
	if items, _ := lj["items"].([]any); len(items) != 1 {
		t.Fatalf("list = %v", lj)
	}
	quota, _ := lj["quota"].(map[string]any)
	if quota["total_bytes"].(float64) != 4<<20 {
		t.Errorf("quota = %v", quota)
	}

	// Delete: row and bytes both gone.
	resp = a.do(http.MethodDelete, "/api/v1/files/"+fileID, nil)
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
	fileID := decodedBody(t, resp)["id"].(string)

	// Second user on the same server: cross-user access is a plain 404 and
	// the file never shows in their list (§9.2).
	b := newAuthTestOn(t, a.ts, a.db)
	signupAs(b, "other@test.io", "gk-code-2")

	resp = b.do(http.MethodGet, "/api/v1/files/"+fileID+"/download", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user download: %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp = b.do(http.MethodDelete, "/api/v1/files/"+fileID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user delete: %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	lj := decodedBody(t, b.do(http.MethodGet, "/api/v1/files", nil))
	if items, _ := lj["items"].([]any); len(items) != 0 {
		t.Errorf("other user list = %v", items)
	}
}

// TestInlineNeverForActiveContent: an artifact row whose media_type claims
// text/plain but whose path ends in .html must still download as an
// attachment — the extension veto always wins (§9.4).
func TestInlineNeverForActiveContent(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "xss@test.io", "gk-code-1")
	uid := meID(a)

	rel := "artifacts/report.html"
	if err := os.MkdirAll(filepath.Join(cfg.Scratchpad.Root, uid, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfg.Scratchpad.Root, uid, rel),
		[]byte("<script>alert(1)</script>"), 0o600)
	fake := &store.File{ID: id.New(), UserID: uid, Kind: store.FileKindArtifact,
		RelPath: rel, MediaType: "text/plain", Name: "report.html", Size: 25,
		SHA256: []byte("00000000000000000000000000000000"), CreatedAt: time.Now()}
	if err := a.db.CreateFile(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	resp := a.do(http.MethodGet, "/api/v1/files/"+fake.ID+"/download?inline=1", nil)
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("html artifact inline attempt: Content-Disposition = %q", cd)
	}
}

func TestSanitizeDisplayName(t *testing.T) {
	cases := map[string]string{
		"":                                "upload",
		"..":                              "upload",
		".":                               "upload",
		"   ":                             "upload",
		"evil\x00name.txt":                "evil_name.txt",
		"a/b/c.txt":                       "c.txt",    // basename only
		`C:\evil\name.txt`:                "name.txt", // windows path → basename
		"..%2f..%2fsecret":                "..%2f..%2fsecret",
		strings.Repeat("x", 300) + ".txt": strings.Repeat("x", 196) + ".txt",
	}
	for in, want := range cases {
		if got := sanitizeDisplayName(in); got != want {
			t.Errorf("sanitizeDisplayName(%q) = %q, want %q", truncStr(in, 20), got, want)
		}
	}
}

func truncStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
