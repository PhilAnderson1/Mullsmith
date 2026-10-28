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
// (2) opening a chat must NOT reorder the list — only posting moves a
// conversation (revised owner rule: last-post ordering, touch removed);
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
	if strings.Contains(sel, "promoteConversation(") {
		t.Error("opening a chat must NOT promote it (only posts reorder the list)")
	}
	promote := jsFuncBody(t, src, "function promoteConversation(")
	if strings.Contains(promote, "touchConversation(") {
		t.Error("the touch-on-open endpoint is gone; promotion must be local only")
	}
	if strings.Contains(src, "touchConversation") {
		t.Error("no client code may still reference the removed touch endpoint")
	}
	send := jsFuncBody(t, src, "async function sendFromComposer(")
	if !strings.Contains(send, "promoteConversation(") {
		t.Error("posting a message must promote the chat (mirror server order)")
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

func TestJSModelPickerShowsModelInUse(t *testing.T) {
	// No "Default model" placeholder: the dropdown lists concrete model
	// names, and an unset conversation displays the server default it
	// actually runs on (meta.default_model). A conversation whose model was
	// since removed from the allow-list still shows it (the transient
	// data-fellout option), because that is the model in use.
	b, err := web.FS.ReadFile("assets/app/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Contains(src, "Default model") {
		t.Error(`app.js still renders a "Default model" placeholder option`)
	}
	rc := jsFuncBody(t, src, "function renderConversation(")
	for _, want := range []string{"state.meta.default_model", "data-fellout", "sel.value = active"} {
		if !strings.Contains(rc, want) {
			t.Errorf("renderConversation must contain %q", want)
		}
	}
}

// The email_features SPA contract (plan §6, §8): the shell owns the new
// forms, the hash router recognizes #forgot/#reset/#confirm (tokens scrubbed
// after consumption), api.js names the five endpoints, and the register flow
// branches to the check-inbox panel. No browser harness exists — the source
// is the contract, pinned textually like the rest of this file.
func TestJSEmailFeatureSurface(t *testing.T) {
	appSrc, err := web.FS.ReadFile("assets/app/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(appSrc)
	apiSrc, err := web.FS.ReadFile("assets/app/api.js")
	if err != nil {
		t.Fatal(err)
	}
	apiSrcStr := string(apiSrc)
	html, err := web.FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	hs := string(html)

	for _, id := range []string{
		"form-forgot", "form-reset", "form-confirm", "register-confirm-note",
		"login-forgot", "forgot-email", "reset-password", "reset-confirm",
		"confirm-error", "btn-resend-confirm", "settings-email-pending",
		"pending-email-addr", "btn-cancel-email", "auth-done-note",
	} {
		if !strings.Contains(hs, `id="`+id+`"`) {
			t.Errorf("index.html lost #%s", id)
		}
	}

	for _, want := range []string{
		`h === "#forgot"`, // router knows the three new routes
		`startsWith("#reset")`,
		`startsWith("#confirm")`,
		"history.replaceState",        // consumed tokens must not linger in URL/history
		"email_confirmation_required", // register swaps to the check-inbox panel
		"state.pendingInbox",
		"email_features",    // meta gate for #login-forgot
		"unconfirmed_email", // Settings pending notice
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js lost %q", want)
		}
	}

	for _, m := range []struct{ fn, path string }{
		{"forgotPassword", "/api/v1/auth/forgot-password"},
		{"resetPassword", "/api/v1/auth/reset-password"},
		{"confirmEmail", "/api/v1/auth/confirm-email"},
		{"resendConfirm", "/api/v1/auth/resend-confirm"},
		{"cancelEmail", "/api/v1/auth/me/cancel-email"},
	} {
		if !strings.Contains(apiSrcStr, m.fn+":") || !strings.Contains(apiSrcStr, `"`+m.path+`"`) {
			t.Errorf("api.js lost %s → %s", m.fn, m.path)
		}
	}
}
