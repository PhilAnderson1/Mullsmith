package thinking

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Nothing is embedded: the canonical catalogues in configs/ are the
// reference data, read straight from the repo.
var (
	repoEndpoints = filepath.Join("..", "..", "configs", "endpoints.yaml")
	repoProfiles  = filepath.Join("..", "..", "configs", "thinking-profiles.yaml")
)

// catSettings fills in the repo catalogue paths on any selection that
// left them out; a test that names its own file keeps it.
func catSettings(s Settings) Settings {
	if s.EndpointsFile == "" {
		s.EndpointsFile = repoEndpoints
	}
	if s.ProfilesFile == "" {
		s.ProfilesFile = repoProfiles
	}
	return s
}

func gufoQwen38(t *testing.T) *Runtime {
	t.Helper()
	rt, err := Build(catSettings(Settings{
		EndpointURL:  "http://192.168.0.2:8080/v1",
		EndpointType: "gufo", ProfileID: "qwen3_8", MaxOutput: 16384,
	}))
	if err != nil {
		t.Fatalf("build gufo/qwen3_8: %v", err)
	}
	return rt
}

func choiceIDs(rt *Runtime) []string {
	var ids []string
	for _, c := range rt.Choices() {
		ids = append(ids, c.ID)
	}
	return ids
}

func fragmentJSON(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The shipped configuration: Default first, profile labels verbatim, and
// High sending xhigh under Gufo.
func TestGufoQwen38ChoicesAndFragments(t *testing.T) {
	rt := gufoQwen38(t)
	if !rt.SelectorVisible() {
		t.Fatal("selector hidden for a usable combination")
	}
	if got := choiceIDs(rt); !slices.Equal(got, []string{"default", "off", "low", "medium", "high"}) {
		t.Fatalf("choices = %v, want default off low medium high", got)
	}

	high, ok := rt.Resolve("high")
	if !ok {
		t.Fatal("high missing")
	}
	if high.Label != "High" {
		t.Errorf("label = %q, want High", high.Label)
	}
	frag, err := rt.Fragment(high)
	if err != nil {
		t.Fatal(err)
	}
	// Spec §8 table: the exact thinking fragment.
	if got := fragmentJSON(t, frag); got != `{"chat_template_kwargs":{"enable_thinking":true},"reasoning_effort":"xhigh"}` {
		t.Errorf("High fragment = %s", got)
	}

	off, _ := rt.Resolve("off")
	frag, err = rt.Fragment(off)
	if err != nil {
		t.Fatal(err)
	}
	if got := fragmentJSON(t, frag); got != `{"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("Off fragment = %s", got)
	}
	if frag["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Error("enable_thinking must stay a JSON boolean")
	}

	// Default sends no override at all.
	def, ok := rt.Resolve("")
	if !ok || def.ID != DefaultID {
		t.Fatalf(`Resolve("") = %+v`, def)
	}
	if frag, err := rt.Fragment(def); frag != nil || err != nil {
		t.Errorf("Default fragment = %v (%v), want nil", frag, err)
	}
}

// GPT-OSS High sends the plain string "high"; the Qwen3.8 label High
// sending xhigh is profile data, not a global alias. And neither the
// thinking-only Qwen nor GLM-5.3 ever gains an Off choice.
func TestProfileEffortsAndMissingOff(t *testing.T) {
	oss, err := Build(catSettings(Settings{EndpointType: "gufo", ProfileID: "gpt_oss"}))
	if err != nil {
		t.Fatal(err)
	}
	high, _ := oss.Resolve("high")
	frag, err := oss.Fragment(high)
	if err != nil {
		t.Fatal(err)
	}
	if frag["reasoning_effort"] != "high" {
		t.Errorf("GPT-OSS High = %v, want high", frag["reasoning_effort"])
	}

	for _, prof := range []string{"glm5_3", "qwen3_8_thinking_only"} {
		rt, err := Build(catSettings(Settings{EndpointType: "gufo", ProfileID: prof}))
		if err != nil {
			t.Fatalf("%s: %v", prof, err)
		}
		if slices.Contains(choiceIDs(rt), "off") {
			t.Errorf("%s exposes an Off choice", prof)
		}
	}
}

// Every supplied profile walked against every openai_chat endpoint:
// recipes and allowlists exclude choices (§5 step 3) with warnings, a
// toggle profile on an effort-only endpoint has nothing usable and is
// rejected, and no combination sneaks effort+budget together.
func TestRecipeAndAllowlistFiltering(t *testing.T) {
	openaiEndpoints := []string{"gufo", "llama_cpp", "vllm", "lm_studio", "ollama_openai", "openrouter"}
	profiles := []string{"none", "qwen3_5", "qwen3_6", "qwen3_8", "qwen3_8_thinking_only",
		"gpt_oss", "gemma4", "deepseek_v4", "deepseek_v4_0731", "glm5_3"}

	for _, ep := range openaiEndpoints {
		for _, prof := range profiles {
			rt, err := Build(catSettings(Settings{EndpointType: ep, ProfileID: prof, MaxOutput: 16384}))
			if !slices.Contains([]string{"qwen3_5", "qwen3_6", "gemma4"}, prof) || ep != "lm_studio" {
				if err != nil {
					t.Errorf("%s/%s: %v", ep, prof, err)
					continue
				}
			} else if err == nil {
				// Toggle-only profile on the effort-only lm_studio endpoint:
				// nothing usable, the combination must be rejected.
				t.Errorf("%s/%s: combination accepted, want rejection", ep, prof)
				continue
			} else {
				continue
			}
			for _, c := range rt.Choices() {
				if c.ID == DefaultID {
					continue
				}
				ch, ok := rt.Resolve(c.ID)
				if !ok {
					t.Errorf("%s/%s: choice %s unresolvable", ep, prof, c.ID)
					continue
				}
				frag, err := rt.Fragment(ch)
				if err != nil {
					t.Errorf("%s/%s: fragment %s: %v", ep, prof, c.ID, err)
					continue
				}
				if _, hasEffort := frag["reasoning_effort"]; hasEffort && ch.Mode == "budget" {
					t.Errorf("%s/%s: budget fragment %s also carries effort", ep, prof, c.ID)
				}
				// No leftover tokens anywhere.
				if s := fragmentJSON(t, frag); strings.Contains(s, "$") {
					t.Errorf("%s/%s: unsubstituted token in %s", ep, prof, s)
				}
			}
		}
	}

	// The LM Studio native allowlist: High (xhigh) excluded — but that
	// definition cannot be selected at all in this build, so the pair is
	// a configuration error, checked beside it.
	if _, err := Build(catSettings(Settings{EndpointType: "lm_studio_native", ProfileID: "qwen3_8"})); err == nil ||
		!strings.Contains(err.Error(), "lm_studio_chat") {
		t.Errorf("lm_studio_native accepted: %v", err)
	}
	if _, err := Build(catSettings(Settings{EndpointType: "ollama", ProfileID: "qwen3_5"})); err == nil ||
		!strings.Contains(err.Error(), "ollama_chat") {
		t.Errorf("ollama native accepted: %v", err)
	}
}

// Budget presets: valid ones survive with the integer substituted, ones
// outside the endpoint range are hidden with a warning, and one that
// would swallow the whole output allowance rejects the configuration.
func TestBudgetOptions(t *testing.T) {
	write := func(t *testing.T, name, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	profiles := write(t, "profiles.yaml", `
schema_version: 1
profiles:
  capped:
    options:
    - id: short
      label: Short
      mode: budget
      budget_tokens: 2048
`)

	// vLLM speaks thinking_token_budget; 2048 of a 16384 answer cap fits.
	rt, err := Build(Settings{EndpointType: "vllm", ProfileID: "capped",
		EndpointsFile: repoEndpoints, ProfilesFile: profiles, MaxOutput: 16384})
	if err != nil {
		t.Fatal(err)
	}
	short, _ := rt.Resolve("short")
	frag, err := rt.Fragment(short)
	if err != nil {
		t.Fatal(err)
	}
	if got := frag["thinking_token_budget"]; got != 2048 {
		t.Errorf("budget = %#v, want the integer 2048", got)
	}

	// Endpoint ceiling: a definition with budget_max 1024 hides 2048 —
	// and hides everything, so the combination itself is rejected.
	ceiled := write(t, "endpoints.yaml", `
schema_version: 1
endpoints:
  capped_ep:
    protocol: openai_chat
    path: /chat/completions
    requests:
      budget:
        thinking_token_budget: $budget_tokens
    budget_min: 1
    budget_max: 1024
`)
	if _, err := Build(Settings{EndpointType: "capped_ep", ProfileID: "capped",
		EndpointsFile: ceiled, ProfilesFile: profiles, MaxOutput: 16384}); err == nil {
		t.Fatal("budget 2048 accepted over endpoint ceiling 1024")
	}

	// A preset >= the total output cap can never leave room for an
	// answer: configuration error, not a quiet hide.
	if _, err := Build(Settings{EndpointType: "vllm", ProfileID: "capped",
		EndpointsFile: repoEndpoints, ProfilesFile: profiles, MaxOutput: 2048}); err == nil {
		t.Fatal("budget 2048 accepted with max_tokens_response 2048")
	}
}

// The configured files are the SOLE catalogue source: an entry is exactly
// what its file says, new IDs are simply available, unknown referenced
// IDs fail clearly, and an unusable file is a startup error (the legacy
// path parses configured files too).
func TestCatalogueFilesAreSoleSource(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	endpoints := write("endpoints.yaml", `
schema_version: 1
endpoints:
  gufo:
    protocol: openai_chat
    path: /v9/chat
    requests:
      'off':
        nope: true
  mine:
    protocol: openai_chat
    path: /chat/completions
    requests:
      'off':
        stop_thinking: true
`)
	profiles := write("profiles.yaml", `
schema_version: 1
profiles:
  qwen3_8:
    options:
    - id: high
      label: High Only
      mode: effort
      effort: xhigh
`)

	// An entry is its file, wholly: this gufo has only what is written
	// here — the qwen3_8 effort recipes it used to pair with are gone.
	if _, err := Build(Settings{EndpointType: "gufo", ProfileID: "qwen3_8",
		EndpointsFile: endpoints, ProfilesFile: profiles}); err == nil {
		t.Fatal("gufo/qwen3_8 accepted with the whole entry replaced (effort recipe is gone)")
	}
	if rt := mustBuild(t, Settings{EndpointType: "gufo", ProfileID: "none",
		EndpointsFile: endpoints, ProfilesFile: profiles}); rt.Route() != "/v9/chat" {
		t.Errorf("route = %q, want the replaced /v9/chat", rt.Route())
	}

	// New IDs are simply available.
	rt2 := mustBuild(t, Settings{EndpointType: "mine", ProfileID: "mine_p",
		EndpointsFile: endpoints,
		ProfilesFile: write("p2.yaml", `
schema_version: 1
profiles:
  mine_p:
    options:
    - id: 'off'
      label: 'Off'
      mode: 'off'
`)})
	off, _ := rt2.Resolve("off")
	frag, err := rt2.Fragment(off)
	if err != nil || frag["stop_thinking"] != true {
		t.Errorf("new endpoint fragment = %v (%v)", frag, err)
	}

	// Unknown explicit selections fail with the ID in the message.
	if _, err := Build(Settings{EndpointType: "nope",
		EndpointsFile: repoEndpoints, ProfilesFile: profiles}); err == nil ||
		!strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown endpoint_type: %v", err)
	}
	if _, err := Build(Settings{EndpointType: "gufo", ProfileID: "nope",
		EndpointsFile: repoEndpoints}); err == nil ||
		!strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown thinking_profile: %v", err)
	}
	// A thinking profile without any endpoint type is an error too.
	if _, err := Build(Settings{ProfileID: "qwen3_8"}); err == nil {
		t.Error("profile without endpoint_type accepted")
	}
	// Unreadable configured files start nothing.
	if _, err := Build(Settings{EndpointType: "gufo", EndpointsFile: filepath.Join(dir, "gone.yaml")}); err == nil {
		t.Error("unreadable endpoints_file accepted")
	}
	// Legacy: nothing thinking-related configured → off, no error.
	if rt, err := Build(Settings{}); rt != nil || err != nil {
		t.Errorf("legacy Build = %v (%v), want nil runtime", rt, err)
	}
}

// The explicit switch (llm.thinking_selector_enable): it outranks the
// legacy shape both ways, and enabling still demands the endpoint type
// and the catalogue files its IDs come from.
func TestSelectorEnableSwitch(t *testing.T) {
	yes, no := true, false
	// The switch alone is not a selection: the endpoint type must still
	// be named (there is no built-in type default).
	if _, err := Build(Settings{SelectorEnable: &yes}); err == nil ||
		!strings.Contains(err.Error(), "endpoint_type") {
		t.Errorf("bare enable must fail without an endpoint_type: %v", err)
	}
	// Type named, catalogue file missing: the file error precedes any
	// lookup — nothing is embedded to fall back on.
	if _, err := Build(Settings{SelectorEnable: &yes, EndpointType: "gufo",
		ProfilesFile: repoProfiles}); err == nil ||
		!strings.Contains(err.Error(), "endpoints_file") {
		t.Errorf("enable without endpoints_file must fail: %v", err)
	}
	// Type spelled out, profile left empty → built-in "none": runtime
	// exists but the list stays hidden (no profiles_file needed).
	if rt := mustBuild(t, Settings{SelectorEnable: &yes, EndpointType: "gufo",
		EndpointsFile: repoEndpoints}); rt.SelectorVisible() {
		t.Error(`enabled selection without a profile must behave like "none"`)
	}
	if rt := mustBuild(t, catSettings(Settings{SelectorEnable: &yes,
		EndpointType: "gufo", ProfileID: "qwen3_8"})); !rt.SelectorVisible() {
		t.Error("enable with the full pair must show the selector")
	}
	if rt, err := Build(catSettings(Settings{EndpointType: "gufo", ProfileID: "qwen3_8",
		SelectorEnable: &no})); rt != nil || err != nil {
		t.Errorf("explicit disable must override a spelled-out selection: %v (%v)", rt, err)
	}
	if rt := mustBuild(t, catSettings(Settings{SelectorEnable: &yes,
		EndpointType: "gufo", ProfileID: "none"})); rt.SelectorVisible() {
		t.Error(`thinking_profile "none" must hide the list even when enabled`)
	}
}

func mustBuild(t *testing.T, set Settings) *Runtime {
	t.Helper()
	rt, err := Build(set)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// The strict parser: duplicate keys, unknown fields, wrong schema
// versions, unknown modes, empty/duplicate IDs, stray $-tokens and
// broken budgets all fail at load.
func TestStrictParsing(t *testing.T) {
	cases := []struct {
		name  string
		file  string // "e" or "p"
		body  string
		wants string // substring of the error
	}{
		{"dup key", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    protocol: openai_chat\n    path: /c\n    requests: {'off': {x: 1}}\n", ""},
		{"unknown field", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {'off': {x: 1}}\n    discover: true\n", "discover"},
		{"schema v2", "e", "schema_version: 2\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {'off': {x: 1}}\n", "schema_version"},
		{"bad mode", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests:\n      turbo: {x: 1}\n", "turbo"},
		{"path no slash", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: chat\n    requests: {'off': {x: 1}}\n", "path"},
		{"stray token", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {'off': {x: $efforts}}\n", "$efforts"},
		{"token wrong mode", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {'off': {x: $effort}}\n", "$effort"},
		{"budget token in effort", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {effort: {x: $budget_tokens}}\n", "$budget_tokens"},
		{"dollar key", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {'off': {$meta: 1}}\n", "$meta"},
		{"empty requests", "e", "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {}\n", "at least one mode"},
		{"dup option", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: x, label: X, mode: 'off'}\n    - {id: x, label: Y, mode: 'on'}\n", "duplicate"},
		{"reserved id", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: default, label: D, mode: 'off'}\n", "reserved"},
		{"empty label", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: x, label: '', mode: 'off'}\n", "label"},
		{"bad mode", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: x, label: X, mode: turbo}\n", "turbo"},
		{"effort without value", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: x, label: X, mode: effort}\n", "effort is required"},
		{"off with effort", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: x, label: X, mode: 'off', effort: low}\n", "takes no effort"},
		{"zero budget", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: x, label: X, mode: budget, budget_tokens: 0}\n", "positive"},
		{"nonint budget", "p", "schema_version: 1\nprofiles:\n  a:\n    options:\n    - {id: x, label: X, mode: budget, budget_tokens: lots}\n", "into int"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), tc.file+".yaml")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			set := Settings{EndpointType: "gufo"}
			if tc.file == "p" {
				// The endpoint side is real, so the failure under test
				// is the profile file's own.
				set.EndpointsFile = repoEndpoints
				set.ProfilesFile = p
				set.ProfileID = "a"
			} else {
				set.EndpointsFile = p
			}
			_, err := Build(set)
			if err == nil {
				t.Fatalf("accepted:\n%s", tc.body)
			}
			if tc.wants != "" && !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q lacks %q", err, tc.wants)
			}
		})
	}
}

// Fragments are fresh copies per call: mutating one request's merged
// payload cannot leak into the next, and effort is never a numeric
// string / a bool an int.
func TestFragmentCopiesAndTypes(t *testing.T) {
	rt := gufoQwen38(t)
	high, _ := rt.Resolve("high")
	a, err := rt.Fragment(high)
	if err != nil {
		t.Fatal(err)
	}
	a["chat_template_kwargs"].(map[string]any)["enable_thinking"] = "mutated"
	a["extra"] = true

	b, err := rt.Fragment(high)
	if err != nil {
		t.Fatal(err)
	}
	if b["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
		t.Error("second render inherited mutations of the first")
	}
	if _, ok := b["extra"]; ok {
		t.Error("second render inherited an unrelated key")
	}
}

// Saved selections restore only with an intact applicability context; a
// model change (part of the fingerprint) forces the visible Default.
func TestFingerprintAndSavedResolution(t *testing.T) {
	rt := gufoQwen38(t)
	ctx := rt.Fingerprint("Qwen3.8 Flash Next")
	if got, ok := rt.ResolveSaved("high", ctx, "Qwen3.8 Flash Next"); !ok || got != "high" {
		t.Errorf("intact saved choice = %q (%v)", got, ok)
	}
	if _, ok := rt.ResolveSaved("high", ctx, "other-model"); ok {
		t.Error("saved choice survived a model change")
	}
	if _, ok := rt.ResolveSaved("high", ctx, "Qwen3.8 Flash Next"); !ok {
		t.Error("same context rejected")
	}
	// "" is Default and always resolves.
	if got, ok := rt.ResolveSaved("", "", "anything"); !ok || got != DefaultID {
		t.Errorf(`empty saved = %q (%v)`, got, ok)
	}
	// The fingerprint must carry no secret material.
	if strings.Contains(ctx, "sk-") {
		t.Error("fingerprint contains key-like material")
	}
	// A different profile/endpoint type means a different context.
	other := mustBuild(t, catSettings(Settings{EndpointURL: "http://192.168.0.2:8080/v1",
		EndpointType: "vllm", ProfileID: "qwen3_8"}))
	if other.Fingerprint("Qwen3.8 Flash Next") == ctx {
		t.Error("fingerprint ignores endpoint_type")
	}
}
