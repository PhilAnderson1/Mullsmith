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
