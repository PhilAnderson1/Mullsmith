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

// The 2026-10 owner batch, pinned where it can silently rot:
// (1) the regenerate button died after the first click because only
// msgNode-attached bubbles had a listener — a completed stream (the new
// last bubble) rendered the button with no handler;
// (2) opening a chat must count as activity (list re-orders by use, not
// creation);
// (3) with registration closed the login page must hide the whole
// "No account yet?" sentence, not just the link;
// (4) the waiting spinner is CSS-driven (no braille JS timer — the frozen
// glyph was the iPad complaint) and must never leave a setInterval behind.
func TestJSRegenerateActivationLoginSpinnerContracts(t *testing.T) {
	b, err := web.FS.ReadFile("assets/app/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	finish := jsFuncBody(t, src, "function finishGeneration(")
	if !strings.Contains(finish, "attachRegen(") {
		t.Error("finishGeneration must attach the regen listener (only-worked-once bug)")
	}
	if !strings.Contains(src, "attachRegen(el);") {
		t.Error("msgNode must attach the regen listener too (server-loaded bubbles)")
	}

	sel := jsFuncBody(t, src, "async function selectConversation(")
	if !strings.Contains(sel, "promoteConversation(") {
		t.Error("selectConversation must promote the opened chat (activity ordering)")
	}
	promote := jsFuncBody(t, src, "function promoteConversation(")
	if !strings.Contains(promote, "touchConversation(") {
		t.Error("promoteConversation must bump the server, or the order resets on reload")
	}

	meta := jsFuncBody(t, src, "function applyMetaFlags(")
	if !strings.Contains(meta, `login-switch`) {
		t.Error("closed registration must hide the whole #login-switch sentence, not just the link")
	}

	spinner := jsFuncBody(t, src, "function spinnerNode(")
	if strings.Contains(spinner, "setInterval") {
		t.Error("spinner must be CSS-animated; the JS braille timer frozen on iOS was the bug")
	}
}

// Owner rule: saving settings (profile or password) closes the panel and
// returns to the chat; only a FAILED save may keep it open, so a user
// never loses typed input to a silent navigation.
func TestJSSettingsSaveReturnsToChat(t *testing.T) {
	b, err := web.FS.ReadFile("assets/app/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, fn := range []string{"async function onSettingsSave(", "async function onPasswordSave("} {
		body := jsFuncBody(t, src, fn)
		li := strings.Index(body, "leaveSettings(")
		if li < 0 {
			t.Errorf("%s must call leaveSettings() — save should close the panel", fn)
			continue
		}
		if try := strings.Index(body, "catch (err)"); try >= 0 && li > try {
			t.Errorf("%s must close the panel in the success path, not after the catch", fn)
		}
	}
}
