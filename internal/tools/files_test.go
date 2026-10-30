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

	"mullsmith/internal/scratch"
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

// Astra finding 5: a script can mint hundreds of thousands of zero-byte
// files; fs_list must read a bounded listing and say so, never load the
// flood into daemon memory.
func TestFsListTruncatedAfterFlood(t *testing.T) {
	root := toolRoot(t)
	for i := 0; i < 201; i++ {
		writeDirect(t, root, fmt.Sprintf("work/f%06d", i), "")
	}
	res, err := run(t, FsList{}, root, argsJSON(t, map[string]string{"path": "work"}))
	if err != nil {
		t.Fatalf("fs_list over 201 entries: %v", err)
	}
	var out struct {
		Entries   []map[string]any `json:"entries"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 200 || !out.Truncated {
		t.Errorf("flooded listing = %d entries (truncated=%v), want 200/true", len(out.Entries), out.Truncated)
	}
	if !strings.Contains(res.Summary, "(more entries exist)") {
		t.Errorf("summary = %q, want a truncation note", res.Summary)
	}
	if truncated, ok := res.Data.(map[string]any)["truncated"].(bool); !ok || !truncated {
		t.Errorf("Data[truncated] = %v, want true", res.Data)
	}
	// A normal directory lists unchanged: no truncated key set true.
	res, err = run(t, FsList{}, root, argsJSON(t, map[string]string{"path": "artifacts"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Summary, "(more entries exist)") {
		t.Errorf("small listing flagged truncated: %q", res.Summary)
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
	// (fs_delete CAN delete uploads files — see TestFsDeleteUploadsFiles.)
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

// --- files_share: copy-into-artifacts semantics -----------------------------

func shareAudit(t *testing.T, res Result) (path, name, url, note string, bytes int64) {
	t.Helper()
	var a struct {
		Path  string `json:"path"`
		Name  string `json:"name"`
		Bytes int64  `json:"bytes"`
		URL   string `json:"url"`
		Note  string `json:"note"`
	}
	if err := json.Unmarshal([]byte(res.Content), &a); err != nil {
		t.Fatalf("audit JSON unparsable: %v (%s)", err, res.Content)
	}
	return a.Path, a.Name, a.URL, a.Note, a.Bytes
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFilesShareCopiesIntoArtifacts(t *testing.T) {
	root := toolRoot(t)
	share := FilesShare{UserQuota: 1 << 20}
	writeDirect(t, root, "work/report.md", "# report\n42\n")

	res, err := run(t, share, root, argsJSON(t, map[string]string{
		"path": "work/report.md", "note": "your summary"}))
	if err != nil {
		t.Fatalf("files_share: %v", err)
	}
	// The audit JSON alone must reproduce the UI card (§10.2): path, name,
	// bytes, url, note — and the path must live in artifacts/.
	dst, name, url, note, bytes := shareAudit(t, res)
	if dst != "artifacts/report.md" || name != "report.md" || bytes != 12 ||
		note != "your summary" ||
		url != "/api/v1/files/download?path=artifacts%2Freport.md" {
		t.Fatalf("audit payload = %s", res.Content)
	}
	cp, err := os.ReadFile(filepath.Join(root, toolUser, "artifacts/report.md"))
	if err != nil || string(cp) != "# report\n42\n" {
		t.Fatalf("copy on disk = %q, %v", cp, err)
	}
	src, _ := os.ReadFile(filepath.Join(root, toolUser, "work/report.md"))
	if string(src) != "# report\n42\n" {
		t.Error("the source must survive the share untouched")
	}
	// Data.file feeds the live tool_end SSE event.
	data, _ := res.Data.(map[string]any)
	file, _ := data["file"].(map[string]any)
	if file["path"] != "artifacts/report.md" || file["url"] != url {
		t.Errorf("card data = %v", data)
	}

	// SNAPSHOT semantics: edit the draft, re-share ⇒ "report (1).md". The
	// first card keeps pointing at the ORIGINAL bytes.
	if err := os.WriteFile(filepath.Join(root, toolUser, "work/report.md"),
		[]byte("# report v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res2, err := run(t, share, root, argsJSON(t, map[string]string{"path": "work/report.md"}))
	if err != nil {
		t.Fatalf("re-share: %v", err)
	}
	dst2, _, _, _, _ := shareAudit(t, res2)
	if dst2 != "artifacts/report (1).md" {
		t.Fatalf("re-share landed at %q, want bumped name", dst2)
	}
	if cp, _ := os.ReadFile(filepath.Join(root, toolUser, "artifacts/report.md")); string(cp) != "# report\n42\n" {
		t.Error("earlier snapshot was overwritten")
	}

	// Sharing a file that already lives in artifacts/ delivers it in
	// place: no copy, no second entry.
	res3, err := run(t, share, root, argsJSON(t, map[string]string{"path": dst}))
	if err != nil {
		t.Fatalf("share artifacts file: %v", err)
	}
	dst3, _, _, _, _ := shareAudit(t, res3)
	if dst3 != "artifacts/report.md" {
		t.Errorf("as-is share moved the deliverable to %q", dst3)
	}
	if n := listNames(t, filepath.Join(root, toolUser, "artifacts")); len(n) != 2 {
		t.Errorf("artifacts dir = %v, want exactly report.md + snapshot", n)
	}

	// Directories and missing files refuse.
	if _, err := run(t, share, root, argsJSON(t, map[string]string{"path": "artifacts"})); err == nil ||
		!strings.Contains(err.Error(), "directory") {
		t.Errorf("share a dir = %v", err)
	}
	if _, err := run(t, share, root, argsJSON(t, map[string]string{"path": "artifacts/nope.md"})); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("share missing = %v", err)
	}
	// No staging debris left in the scratch root.
	leftovers, _ := filepath.Glob(filepath.Join(root, ".share-*"))
	if len(leftovers) != 0 {
		t.Errorf("share temps left behind: %v", leftovers)
	}
}

// The share copy is a WRITE: same quota doctrine as fs_write — refused up
// front when the tree is already full, and rolled back when a copy that
// slipped past the pre-check crosses the line (audit2 finding 3).
func TestFilesShareQuota(t *testing.T) {
	root := toolRoot(t)
	// Tree: work/a.txt 190 + work/b.txt 50 = 240 used, quota 250. The
	// pre-check passes (240 < 250), but landing the 50-byte copy crosses
	// the line ⇒ the fail-closed re-check must delete the copy (§9.7).
	share := FilesShare{UserQuota: 250}
	writeDirect(t, root, "work/a.txt", strings.Repeat("x", 190))
	writeDirect(t, root, "work/b.txt", strings.Repeat("y", 50))
	_, err := run(t, share, root, argsJSON(t, map[string]string{"path": "work/b.txt"}))
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("over-quota share = %v, want quota error", err)
	}
	if _, err := os.Stat(filepath.Join(root, toolUser, "artifacts/b.txt")); !os.IsNotExist(err) {
		t.Error("over-quota copy must be rolled back")
	}
	if _, err := os.Stat(filepath.Join(root, toolUser, "work/b.txt")); err != nil {
		t.Error("the rollback must not touch the source")
	}

	// A tree already at/over the line is refused up front: no copy lands.
	shareFull := FilesShare{UserQuota: 200}
	if _, err := run(t, shareFull, root, argsJSON(t, map[string]string{"path": "work/b.txt"})); err == nil ||
		!strings.Contains(err.Error(), "quota") {
		t.Errorf("share over a full tree = %v, want quota error", err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, toolUser, "artifacts"))
	if len(entries) != 0 {
		t.Errorf("refused shares still wrote: %v", listNames(t, filepath.Join(root, toolUser, "artifacts")))
	}
}

func TestToolsNeedScratchpad(t *testing.T) {
	// A misassembled invocation (no root) must fail cleanly, not panic.
	for _, tl := range []Tool{FsList{}, FsRead{}, FsWrite{}, FsDelete{}, FilesShare{}} {
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
		if got := scratch.MediaTypeForPath(p); got != want {
			t.Errorf("MediaTypeForPath(%q) = %q, want %q", p, got, want)
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
	// fs_delete may remove uploads FILES, but the normalized checks still
	// refuse the uploads DIRECTORY itself through every "./" spelling.
	for _, target := range []string{"./uploads", "././uploads"} {
		if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": target})); err == nil {
			t.Errorf("fs_delete %q: accepted, want the uploads-directory refusal", target)
		}
	}

	// The user's upload must be physically untouched.
	b, err := os.ReadFile(filepath.Join(root, toolUser, "uploads", "protected.txt"))
	if err != nil || string(b) != "user data\n" {
		t.Errorf("upload modified through the bypass: %q %v", b, err)
	}

	// Normalized "./" stays legal inside work/, and a raw-prefix bypass
	// of the uploads rule is still dead: the rule reads NORMALIZED first
	// segments, not strings. ".uploads/x" — once allowed as a sibling
	// directory — now falls to the root rule: nothing may be created
	// beside work/, uploads/ and artifacts/.
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": "./work/ok.txt", "content": "fine"})); err != nil {
		t.Errorf("./work/ok.txt refused: %v", err)
	}
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": ".uploads/x", "content": "fine"})); err == nil ||
		!strings.Contains(err.Error(), "root is not writable") {
		t.Errorf(".uploads/x accepted: %v, want root-litter refusal", err)
	}
	// Plain form still refused, and traversal dies in ParseRel as before.
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": "uploads/plain.txt", "content": "x"})); err == nil {
		t.Error("plain uploads/ write accepted")
	}
	if _, err := run(t, w, root, argsJSON(t, map[string]string{"path": "../uploads/x", "content": "x"})); err == nil {
		t.Error("../uploads/ write accepted")
	}
}

// --- audit finding 8: sharing must never point a TTL at user uploads -------

func TestFilesShareRejectsUploads(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "uploads/protected.txt", "user data\n")
	share := FilesShare{UserQuota: 1 << 20}

	for _, p := range []string{"uploads/protected.txt", "./uploads/protected.txt"} {
		_, err := run(t, share, root, argsJSON(t, map[string]string{"path": p}))
		if err == nil || !strings.Contains(err.Error(), "already downloadable") {
			t.Errorf("share %q: err = %v, want 'already downloadable' refusal", p, err)
		}
	}
	// Nothing may appear in artifacts/ from an uploads path (audit
	// finding 8): the TTL sweep must never reach the user's originals.
	if n := listNames(t, filepath.Join(root, toolUser, "artifacts")); len(n) != 0 {
		t.Errorf("artifacts dir = %v, want empty", n)
	}
	b, err := os.ReadFile(filepath.Join(root, toolUser, "uploads", "protected.txt"))
	if err != nil || string(b) != "user data\n" {
		t.Errorf("upload touched: %q %v", b, err)
	}

	// Generated files stay shareable — TTL on those is the design.
	writeDirect(t, root, "work/gen.md", "# report\n")
	if _, err := run(t, share, root, argsJSON(t, map[string]string{"path": "work/gen.md"})); err != nil {
		t.Errorf("share work/gen.md: %v", err)
	}
}

// The scratchpad ROOT is reserved: only work/ and artifacts/ accept model
// writes, at any depth, but never the layout directories themselves and
// never a bare root-level name (files there are invisible to the Files
// page and unaccounted for by the workflow).
func TestWriteTargetRootIsReserved(t *testing.T) {
	w := FsWrite{}
	// Refused: bare root entries, the layout dirs, and the root itself.
	for _, p := range []string{
		"stray.txt", "mydir/nested.txt", "bin_listing.txt",
		"work", "artifacts", "uploads", ".", "/",
	} {
		if _, err := run(t, w, toolRoot(t), argsJSON(t, map[string]string{"path": p, "content": "x"})); err == nil {
			t.Errorf("fs_write(%q) accepted; root is reserved", p)
		}
	}
	// Allowed: real files inside the two writable namespaces, nested too.
	for _, p := range []string{"work/a.txt", "work/sub/deep/b.txt", "artifacts/r.md"} {
		if _, err := run(t, w, toolRoot(t), argsJSON(t, map[string]string{"path": p, "content": "x"})); err != nil {
			t.Errorf("fs_write(%q) refused: %v", p, err)
		}
	}
}

// --- fs_delete on uploads/ (the one tool allowed there) ---------------------

func TestFsDeleteUploadsFiles(t *testing.T) {
	// The premise of letting fs_delete touch uploads/ at all: it is
	// approval-gated (default confirm — unknown tools fail closed), so
	// deleting user data is always a decision made in the UI.
	if got := NewRegistry(func(string) string { return ApprovalConfirm }).Policy("fs_delete"); got != ApprovalConfirm {
		t.Errorf("fs_delete default policy = %q, want confirm", got)
	}

	root := toolRoot(t)
	writeDirect(t, root, "uploads/a.png", "\x89PNG\r\n\x1a\nfake")
	writeDirect(t, root, "uploads/report.csv", "a,b\n")
	writeDirect(t, root, "uploads/keep/inner.txt", "x")
	os.MkdirAll(filepath.Join(root, toolUser, "uploads", "dir.png"), 0o755)

	// Files go, any extension, nested too.
	for _, target := range []string{"uploads/a.png", "uploads/report.csv", "uploads/keep/inner.txt"} {
		if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": target})); err != nil {
			t.Errorf("deleting %s: %v", target, err)
		}
	}
	// Directories never go — neither the uploads/ dir itself nor a
	// directory wearing a file extension.
	for _, target := range []string{"uploads", "uploads/dir.png", "uploads/keep"} {
		if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": target})); err == nil {
			t.Errorf("%s: deleted, want refusal", target)
		}
	}
	if _, err := os.Stat(filepath.Join(root, toolUser, "uploads", "dir.png")); err != nil {
		t.Error("directory named dir.png must survive")
	}
	// The exception is DELETIONS only: fs_write keeps its uploads wall.
	if _, err := run(t, FsWrite{UserQuota: 1 << 20}, root,
		argsJSON(t, map[string]string{"path": "uploads/x.png", "content": "c"})); err == nil {
		t.Error("fs_write into uploads must stay refused")
	}
	// work/ semantics unchanged: files and empty directories.
	writeDirect(t, root, "work/w.txt", "w")
	if _, err := run(t, FsDelete{}, root, argsJSON(t, map[string]string{"path": "work/w.txt"})); err != nil {
		t.Errorf("work delete: %v", err)
	}
}
