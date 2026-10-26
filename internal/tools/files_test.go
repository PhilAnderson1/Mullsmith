package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"gabkeep/internal/scratch"
)

const (
	toolUser  = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	otherUser = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
)

func toolRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, u := range []string{toolUser, otherUser} {
		if err := scratch.EnsureUserDirs(root, u); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func inv(root, args string) Invocation {
	return Invocation{UserID: toolUser, Scratchpad: root, Arguments: args}
}

func argsJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func run(t *testing.T, tl Tool, root, args string) (Result, error) {
	t.Helper()
	return tl.Execute(context.Background(), inv(root, args))
}

func writeDirect(t *testing.T, root, rel, content string) {
	t.Helper()
	f, err := scratch.CreateFile(root, toolUser, rel, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
}

func TestFsWriteReadListRoundTrip(t *testing.T) {
	root := toolRoot(t)

	res, err := run(t, FsWrite{}, root, argsJSON(t, map[string]string{
		"path": "artifacts/reports/2026/q3.md", "content": "# Q3\nall good",
	}))
	if err != nil {
		t.Fatalf("fs_write: %v", err)
	}
	if !strings.Contains(res.Content, `"bytes_written":13`) {
		t.Errorf("write result = %s", res.Content)
	}
	// Parent dirs were created inside the user tree.
	if _, err := os.Stat(filepath.Join(root, toolUser, "artifacts/reports/2026/q3.md")); err != nil {
		t.Fatal(err)
	}

	res, err = run(t, FsRead{}, root, argsJSON(t, map[string]string{"path": "artifacts/reports/2026/q3.md"}))
	if err != nil {
		t.Fatalf("fs_read: %v", err)
	}
	var out struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		Note      string `json:"note"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "# Q3\nall good" || out.Truncated {
		t.Errorf("read content = %q, trunc=%v", out.Content, out.Truncated)
	}
	if !strings.Contains(out.Note, "Untrusted") {
		t.Errorf("missing untrusted-data note: %q", out.Note)
	}

	res, err = run(t, FsList{}, root, argsJSON(t, map[string]string{"path": "artifacts/reports/2026"}))
	if err != nil {
		t.Fatalf("fs_list: %v", err)
	}
	if !strings.Contains(res.Content, "q3.md") {
		t.Errorf("list = %s", res.Content)
	}
}

func TestFsToolsRejectEscapes(t *testing.T) {
	root := toolRoot(t)
	secret := filepath.Join(t.TempDir(), "victim.txt")
	os.WriteFile(secret, []byte("outside"), 0o600)

	// Path-level rejects happen through the sandbox on every fs tool.
	badPaths := []string{"../etc/passwd", "/etc/passwd", "a/../../b", "..\\..\\x", "uploads/../../outside"}
	for _, p := range badPaths {
		if _, err := run(t, FsRead{}, root, argsJSON(t, map[string]string{"path": p})); err == nil ||
			!strings.Contains(err.Error(), "sandbox") {
			t.Errorf("fs_read(%q) = %v, want sandbox rejection", p, err)
		}
		if _, err := run(t, FsWrite{}, root,
			argsJSON(t, map[string]string{"path": p, "content": "x"})); err == nil ||
			!strings.Contains(err.Error(), "sandbox") && !strings.Contains(err.Error(), "uploads") {
			t.Errorf("fs_write(%q) = %v, want rejection", p, err)
		}
		if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": p})); err == nil ||
			!strings.Contains(err.Error(), "sandbox") && !strings.Contains(err.Error(), "uploads") {
			t.Errorf("fs_delete(%q) = %v, want rejection", p, err)
		}
	}

	// Symlink planted inside the tree must not be followed (§9.7).
	os.Symlink(secret, filepath.Join(root, toolUser, "work", "slip.txt"))
	if _, err := run(t, FsRead{}, root, argsJSON(t, map[string]string{"path": "work/slip.txt"})); err == nil ||
		!strings.Contains(err.Error(), "sandbox") {
		t.Errorf("symlink read = %v, want sandbox rejection", err)
	}

	// Another user's tree is physically unreachable: identical rel path,
	// different invocation → not found, never the other's bytes.
	writeDirect(t, root, "work/mine.txt", "MINE") // written as toolUser
	if _, err := run(t, FsRead{}, root, argsJSON(t, map[string]string{"path": "work/mine.txt"})); err != nil {
		t.Fatalf("own read: %v", err)
	}
	other := Invocation{UserID: otherUser, Scratchpad: root,
		Arguments: argsJSON(t, map[string]string{"path": "work/mine.txt"})}
	if _, err := (FsRead{}).Execute(context.Background(), other); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("cross-user read = %v, want not found", err)
	}
}

func TestUploadsNamespaceProtected(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "uploads/data.csv", "a,b\n1,2\n")

	// Readable (that's the point of the manifest).
	if _, err := run(t, FsRead{}, root, argsJSON(t, map[string]string{"path": "uploads/data.csv"})); err != nil {
		t.Fatalf("reading uploads: %v", err)
	}
	// Not writable, not deletable.
	if _, err := run(t, FsWrite{}, root,
		argsJSON(t, map[string]string{"path": "uploads/data.csv", "content": "pwned"})); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Errorf("upload write = %v, want read-only refusal", err)
	}
	if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "uploads/data.csv"})); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Errorf("upload delete = %v, want read-only refusal", err)
	}
	// Even a fresh file inside uploads/ is refused.
	if _, err := run(t, FsWrite{}, root,
		argsJSON(t, map[string]string{"path": "uploads/new.txt", "content": "x"})); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Errorf("upload create = %v, want read-only refusal", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, toolUser, "uploads/data.csv")); string(b) != "a,b\n1,2\n" {
		t.Error("uploads content was modified")
	}
}

func TestFsWriteQuotaRollback(t *testing.T) {
	root := toolRoot(t)
	w := FsWrite{UserQuota: 200}

	// 190 bytes fits.
	if _, err := w.Execute(context.Background(), inv(root, argsJSON(t, map[string]string{
		"path": "work/a.txt", "content": strings.Repeat("x", 190)}))); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// 50 more crosses the line: check passes pre-write (190 < 200), the
	// post-close recheck must delete the file and fail (§9.7).
	_, err := w.Execute(context.Background(), inv(root, argsJSON(t, map[string]string{
		"path": "work/b.txt", "content": strings.Repeat("y", 50)})))
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("over-quota write = %v, want quota error", err)
	}
	if _, err := os.Stat(filepath.Join(root, toolUser, "work/b.txt")); !os.IsNotExist(err) {
		t.Error("over-quota file must be rolled back")
	}
	// And a write that would exceed even before starting is refused early.
	if _, err := w.Execute(context.Background(), inv(root, argsJSON(t, map[string]string{
		"path": "work/c.txt", "content": strings.Repeat("z", 500)}))); err == nil ||
		!strings.Contains(err.Error(), "quota") {
		t.Errorf("pre-check = %v, want quota error", err)
	}
}

func TestFsReadTruncationAndBinary(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "work/big.txt", strings.Repeat("a", fsReadLimit+100))
	res, err := run(t, FsRead{}, root, argsJSON(t, map[string]string{"path": "work/big.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, `"truncated":true`) {
		t.Error("oversized read must be flagged truncated")
	}
	var out struct {
		Content string `json:"content"`
	}
	json.Unmarshal([]byte(res.Content), &out)
	if len(out.Content) != fsReadLimit {
		t.Errorf("content length = %d, want %d", len(out.Content), fsReadLimit)
	}

	writeDirect(t, root, "work/bin.dat", "\xff\xfe not utf8 \x00")
	if _, err := run(t, FsRead{}, root, argsJSON(t, map[string]string{"path": "work/bin.dat"})); err == nil ||
		!strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("binary read = %v, want UTF-8 refusal", err)
	}
}

func TestFsDelete(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "work/gone.txt", "bye")
	if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "work/gone.txt"})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "work/gone.txt"})); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("double delete = %v", err)
	}
	// Non-empty directories refuse; "." (the root) refuses.
	writeDirect(t, root, "work/keep/inner.txt", "x")
	if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "work/keep"})); err == nil {
		t.Error("deleting a non-empty dir must fail")
	}
	if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "."})); err == nil {
		t.Error("deleting the root must fail")
	}
	// Now emptied, it deletes.
	run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "work/keep/inner.txt"}))
	if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "work/keep"})); err != nil {
		t.Errorf("deleting an empty dir: %v", err)
	}
}

type fakeSink struct {
	recs []ArtifactRecord
	id   string
	err  error
}

func (f *fakeSink) RegisterArtifact(_ context.Context, rec ArtifactRecord) (string, error) {
	f.recs = append(f.recs, rec)
	return f.id, f.err
}

func TestFilesShare(t *testing.T) {
	root := toolRoot(t)
	sink := &fakeSink{id: "01FILEID000000000000000000"}
	share := FilesShare{Sink: sink}
	writeDirect(t, root, "artifacts/report.md", "# report\n42\n")

	invoc := inv(root, argsJSON(t, map[string]string{
		"path": "artifacts/report.md", "note": "your summary"}))
	invoc.Conversation = "01CONV000000000000000000"
	res, err := share.Execute(context.Background(), invoc)
	if err != nil {
		t.Fatalf("files_share: %v", err)
	}
	if len(sink.recs) != 1 {
		t.Fatalf("sink records = %d", len(sink.recs))
	}
	rec := sink.recs[0]
	if rec.UserID != toolUser || rec.RelPath != "artifacts/report.md" ||
		rec.ConversationID != "01CONV000000000000000000" ||
		rec.Name != "report.md" || rec.MediaType != "text/markdown" && rec.MediaType != "text/plain" {
		t.Errorf("sink record = %+v", rec)
	}
	if len(rec.SHA256) != 32 || rec.Size != 12 {
		t.Errorf("hash/size = %x / %d", rec.SHA256, rec.Size)
	}
	// UI card payload (§10.2): Data.file carries id + download URL.
	data, _ := res.Data.(map[string]any)
	file, _ := data["file"].(map[string]any)
	if file["id"] != sink.id || file["url"] != "/api/v1/files/"+sink.id+"/download" {
		t.Errorf("card data = %v", data)
	}
	if !strings.Contains(res.Content, "/api/v1/files/"+sink.id+"/download") {
		t.Errorf("model content = %s", res.Content)
	}

	// Directories and missing files refuse; sink failures sanitize.
	if _, err := run(t, share, root, argsJSON(t, map[string]string{"path": "artifacts"})); err == nil ||
		!strings.Contains(err.Error(), "directory") {
		t.Errorf("share a dir = %v", err)
	}
	if _, err := run(t, share, root, argsJSON(t, map[string]string{"path": "artifacts/nope.md"})); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("share missing = %v", err)
	}
	sink.err = fmt.Errorf("db down")
	if _, err := run(t, share, root, argsJSON(t, map[string]string{"path": "artifacts/report.md"})); err == nil ||
		!strings.Contains(err.Error(), "register") {
		t.Errorf("sink failure = %v", err)
	}
	res2, err := FilesShare{}.Execute(context.Background(), invoc)
	if err == nil || strings.Contains(res2.Content, "file_id") {
		t.Errorf("nil sink = %v %v, want configured error", res2, err)
	}
}

func TestToolsNeedScratchpad(t *testing.T) {
	// A misassembled invocation (no root) must fail cleanly, not panic.
	for _, tl := range []Tool{FsList{}, FsRead{}, FsWrite{}, FsDelete{},
		FilesShare{Sink: &fakeSink{id: "x"}}} {
		_, err := tl.Execute(context.Background(), Invocation{UserID: toolUser, Arguments: `{}`})
		if err == nil || !strings.Contains(err.Error(), "scratchpad") {
			t.Errorf("%s with no root = %v", tl.Name(), err)
		}
	}
}

func TestBadArgsSanitized(t *testing.T) {
	root := toolRoot(t)
	_, err := run(t, FsRead{}, root, `{"path":`) // malformed JSON
	if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("bad JSON = %v", err)
	}
	_, err = run(t, FsRead{}, root, `{}`) // missing required path
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("missing path = %v", err)
	}
}

func TestMediaTypesAndUtf8Helpers(t *testing.T) {
	for p, want := range map[string]string{
		"a/b.csv": "text/csv", "x.md": "text/plain", "no_ext": "application/octet-stream",
		"IMG.PNG": "image/png", "d.html": "text/html",
	} {
		if got := mediaTypeForPath(p); got != want {
			t.Errorf("mediaTypeForPath(%q) = %q, want %q", p, got, want)
		}
	}
	// humanBytes must stay ASCII-safe for summaries.
	if humanBytes(2048) != "2.0 KiB" || !utf8.ValidString(humanBytes(5)) {
		t.Errorf("humanBytes broken")
	}
}

// --- audit finding 7: uploads protection must run on normalized segments ----

func TestUploadsNamespaceCoversDotPrefixedPaths(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "uploads/protected.txt", "user data\n")

	w := FsWrite{UserQuota: 1 << 20}
	for _, bypass := range []string{"./uploads/protected.txt", "././uploads/protected.txt"} {
		_, err := run(t, w, root, argsJSON(t, map[string]string{"path": bypass, "content": "clobbered"}))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("fs_write %q: err = %v, want uploads read-only refusal", bypass, err)
		}
	}
	for _, target := range []string{"./uploads/protected.txt", "./uploads"} {
		_, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": target}))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("fs_delete %q: err = %v, want uploads read-only refusal", target, err)
		}
	}

	// The user's upload must be physically untouched.
	b, err := os.ReadFile(filepath.Join(root, toolUser, "uploads", "protected.txt"))
	if err != nil || string(b) != "user data\n" {
		t.Errorf("upload modified through the bypass: %q %v", b, err)
	}

	// Normalized "./" stays legal OUTSIDE uploads, and ".uploads" is a
	// different directory — the rule is the normalized first segment, not
	// string matching.
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": "./work/ok.txt", "content": "fine"})); err != nil {
		t.Errorf("./work/ok.txt refused: %v", err)
	}
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": ".uploads/x", "content": "fine"})); err != nil {
		t.Errorf(".uploads/x (hidden sibling dir) refused: %v", err)
	}
	// Plain form still refused, and traversal dies in ParseRel as before.
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": "uploads/plain.txt", "content": "x"})); err == nil {
		t.Error("plain uploads/ write accepted")
	}
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": "../uploads/x", "content": "x"})); err == nil {
		t.Error("../uploads/ write accepted")
	}
}
