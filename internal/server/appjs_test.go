package server

import (
	"regexp"
	"strings"
	"testing"

	"gabkeep/web"
)

// A handler name used but never defined is a runtime ReferenceError that
// browsers swallow into the console — the exact silent "Send does nothing"
// bug this guards. node --check cannot see it; this test reads the embedded
// module and resolves every addEventListener target against the file.
func TestJSHandlersAreDefined(t *testing.T) {
	b, err := web.FS.ReadFile("assets/app/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	patterns := []*regexp.Regexp{
		// addEventListener("click", handler)
		regexp.MustCompile(`addEventListener\("[a-z]+",\s*([A-Za-z_$][\w$]*)\)`),
		// addEventListener("click", () => handler()) — zero-arg only, so
		// locals like () => resolve(x) inside dialogs are not miscounted.
		regexp.MustCompile(`addEventListener\("[a-z]+",\s*\(\)\s*=>\s*([A-Za-z_$][\w$]*)\(\)`),
	}
	seen := map[string]bool{}
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) < 8 {
		t.Fatalf("only %d handlers captured — regex no longer matches the code shape", len(seen))
	}
	for name := range seen {
		if !strings.Contains(src, "function "+name+"(") &&
			!strings.Contains(src, "const "+name+" =") {
			t.Errorf("app.js registers handler %q which is never defined", name)
		}
	}
}

// jsFuncBody extracts an embedded-app function body by convention: the
// signature line starts the body, and the first column-0 "}" ends it
// (internal blocks are indented).
func jsFuncBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("app.js no longer contains %q", sig)
	}
	end := strings.Index(src[i:], "\n}")
	if end < 0 {
		t.Fatalf("unterminated %q", sig)
	}
	return src[i : i+end]
}

// The ghost-row bugs: (1) overlapping loadFiles calls used to merge two
// responses into one list — the same path rendered twice; (2) deleting a
// folder removed only its own row, leaving every deleted child row on
// screen as an undeletable 404 ghost. Both fixes are pinned textually —
// the app has no browser test harness, so the source is the contract.
func TestJSFilesListRefreshDiscardsStaleAndGhosts(t *testing.T) {
	b, err := web.FS.ReadFile("assets/app/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	lf := jsFuncBody(t, src, "async function loadFiles(")
	if !strings.Contains(lf, "++filesLoadSeq") ||
		!strings.Contains(lf, "gen !== filesLoadSeq") {
		t.Error("loadFiles lost its stale-response guard (duplicate rows bug)")
	}

	dr := jsFuncBody(t, src, "function fileRow(")
	if !strings.Contains(dr, "loadFiles(true)") {
		t.Error("delete handler must refresh the list, not just remove one row (ghost-children bug)")
	}
	if !strings.Contains(dr, "status === 404") {
		t.Error("delete handler must treat an already-gone path as stale, not a hard error")
	}
}
