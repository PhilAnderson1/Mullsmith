package thinking

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Acceptance tests for the model template-parameter addendum: the same
// endpoint definition expressing Qwen's `enable_thinking` and Kimi's
// `thinking`, explicit null beating an endpoint default, generated-object
// pruning, collision detection, version 1/2 compatibility, and the
// catalogue meaning being part of a saved choice's fingerprint.

func writeCatalogue(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The addendum's v2 profiles: a Kimi family whose template calls the
// switch `thinking`, an effort-only family whose template has no toggle at
// all, and one whose Off rides the switch alone.
const v2Profiles = `
schema_version: 2
profiles:
  kimi_k2_6:
    template_parameters:
      thinking_toggle: thinking
    options:
    - {id: 'off', label: 'Off', mode: 'off'}
    - {id: 'on', label: 'On', mode: 'on'}
  no_toggle:
    template_parameters:
      thinking_toggle: null
    options:
    - {id: 'off', label: 'Off', mode: 'off'}
    - {id: low, label: Low, mode: effort, effort: low}
    - {id: medium, label: Medium, mode: effort, effort: medium}
  no_params:
    options:
    - {id: 'off', label: 'Off', mode: 'off'}
    - {id: high, label: High, mode: effort, effort: xhigh}
  empty:
    template_parameters:
      thinking_toggle: null
    options: []
`

// fragmentFor renders one choice of one profile against one endpoint
// entry, both read from the given catalogue files.
func fragmentFor(t *testing.T, epFile, epID, profFile, profID, choiceID string) string {
	t.Helper()
	rt, err := Build(Settings{EndpointType: epID, ProfileID: profID,
		EndpointsFile: epFile, ProfilesFile: profFile, MaxOutput: 16384})
	if err != nil {
		t.Fatalf("build %s/%s: %v", epID, profID, err)
	}
	c, ok := rt.ResolveFor("", choiceID)
	if !ok {
		t.Fatalf("%s/%s has no choice %q (have %v)", epID, profID, choiceID, choiceIDs(rt))
	}
	frag, err := rt.Fragment("", c)
	if err != nil {
		t.Fatal(err)
	}
	return fragmentJSON(t, frag)
}

// fixtureFragment is fragmentFor against the addendum fixture profiles.
func fixtureFragment(t *testing.T, epFile, epID, profID, choiceID string) string {
	t.Helper()
	return fragmentFor(t, epFile, epID, writeCatalogue(t, "profiles.yaml", v2Profiles), profID, choiceID)
}

// The addendum's request-expectation table: one v2 endpoint definition,
// four template conventions.
func TestTemplateParameterRequestShapes(t *testing.T) {
	cases := []struct {
		name   string
		epID   string
		prof   string // fixture profile id, or repo:qwen3_8 for the shipped one
		choice string
		want   string
	}{
		{"vllm kimi off", "vllm", "kimi_k2_6", "off", `{"chat_template_kwargs":{"thinking":false}}`},
		{"vllm kimi on", "vllm", "kimi_k2_6", "on", `{"chat_template_kwargs":{"thinking":true}}`},
		{"vllm qwen high", "vllm", "repo:qwen3_8", "high", `{"chat_template_kwargs":{"enable_thinking":true},"reasoning_effort":"xhigh"}`},
		{"vllm null toggle low", "vllm", "no_toggle", "low", `{"reasoning_effort":"low"}`},
		{"vllm default name applies", "vllm", "no_params", "off", `{"chat_template_kwargs":{"enable_thinking":false}}`},
		{"lm studio off ignores switch name", "lm_studio", "kimi_k2_6", "off", `{"reasoning_effort":"none"}`},
		{"lm studio effort ignores switch name", "lm_studio", "no_toggle", "low", `{"reasoning_effort":"low"}`},
		{"openrouter effort carries no template args", "openrouter", "repo:qwen3_8", "high", `{"reasoning":{"effort":"xhigh"}}`},
		{"openrouter off carries no template args", "openrouter", "kimi_k2_6", "off", `{"reasoning":{"enabled":false}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profFile, profID := writeCatalogue(t, "profiles.yaml", v2Profiles), tc.prof
			if strings.HasPrefix(profID, "repo:") {
				profFile, profID = repoProfiles, strings.TrimPrefix(profID, "repo:")
			}
			if got := fragmentFor(t, repoEndpoints, tc.epID, profFile, profID, tc.choice); got != tc.want {
				t.Errorf("fragment = %s, want %s", got, tc.want)
			}
		})
	}
}

// LM Studio's mapping has no On recipe of its own: the toggle profile's On
// is simply not offered there, and nothing is invented to fill the gap.
// Its tested Off (`reasoning_effort: none`) stands regardless of what the
// model's template calls its switch.
func TestLMStudioKeepsItsTestedOffMapping(t *testing.T) {
	rt, err := Build(catSettings(Settings{EndpointType: "lm_studio", ProfileID: "kimi_k2_6",
		ProfilesFile: writeCatalogue(t, "p.yaml", v2Profiles)}))
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(rt.Choices("")); !slices.Equal(got, []string{"default", "off"}) {
		t.Errorf("choices = %v, want default and off only (no on recipe)", got)
	}
	off, _ := rt.ResolveFor("", "off")
	frag, err := rt.Fragment("", off)
	if err != nil {
		t.Fatal(err)
	}
	if got := fragmentJSON(t, frag); got != `{"reasoning_effort":"none"}` {
		t.Errorf("off fragment = %s", got)
	}
	if n := len(rt.Warnings()); n == 0 {
		t.Error("the hidden on option should have produced a warning")
	}
}

// An endpoint fragment that keeps an unrelated template argument: the
// null toggle removes only the switch, and the object survives because it
// still has a member (addendum §5 rule 5).
func TestNullTogglePrunesOnlyTheSwitch(t *testing.T) {
	ep := writeCatalogue(t, "endpoints.yaml", `
schema_version: 2
endpoints:
  tmpl:
    protocol: openai_chat
    path: /chat/completions
    template_defaults:
      thinking_toggle: enable_thinking
    requests:
      'off':
        chat_template_kwargs:
          $template.thinking_toggle: false
          keep_this: true
`)
	if got := fixtureFragment(t, ep, "tmpl", "no_toggle", "off"); got != `{"chat_template_kwargs":{"keep_this":true}}` {
		t.Errorf("off fragment = %s, want the unrelated argument alone", got)
	}
	// Same fragment with a named switch keeps both.
	if got := fixtureFragment(t, ep, "tmpl", "no_params", "off"); got != `{"chat_template_kwargs":{"enable_thinking":false,"keep_this":true}}` {
		t.Errorf("named-switch fragment = %s", got)
	}
}

// A null toggle that empties an Off fragment makes that option
// unavailable — an empty request must never be offered as a working
// control (§6).
func TestEmptyRenderedFragmentHidesChoice(t *testing.T) {
	rt, err := Build(Settings{EndpointType: "vllm", ProfileID: "no_toggle", MaxOutput: 16384,
		EndpointsFile: repoEndpoints, ProfilesFile: writeCatalogue(t, "p.yaml", v2Profiles)})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(rt.Choices("")); slices.Contains(got, "off") {
		t.Errorf("choices = %v, want Off hidden (its whole fragment renders empty)", got)
	}
	if !slices.Contains(idsOf(rt.Choices("")), "low") {
		t.Error("the effort choice must survive")
	}
	var warned bool
	for _, w := range rt.Warnings() {
		if strings.Contains(w, `"off"`) && strings.Contains(w, "no request fields") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings = %v, want one naming the hidden off choice", rt.Warnings())
	}
}

// An unresolved required parameter, a colliding rendered key, and a
// non-boolean switch are configuration errors rather than a silently
// filtered choice (§6).
func TestTemplateParameterConfigurationErrors(t *testing.T) {
	unresolved := writeCatalogue(t, "unresolved.yaml", `
schema_version: 2
endpoints:
  bare:
    protocol: openai_chat
    path: /chat/completions
    requests:
      'off':
        chat_template_kwargs:
          $template.thinking_toggle: false
`)
	nonBool := writeCatalogue(t, "nonbool.yaml", `
schema_version: 2
endpoints:
  bad:
    protocol: openai_chat
    path: /chat/completions
    template_defaults:
      thinking_toggle: enable_thinking
    requests:
      'off':
        chat_template_kwargs:
          $template.thinking_toggle: 'no'
`)
	collide := writeCatalogue(t, "collide.yaml", `
schema_version: 2
endpoints:
  clash:
    protocol: openai_chat
    path: /chat/completions
    template_defaults:
      thinking_toggle: enable_thinking
    requests:
      'off':
        chat_template_kwargs:
          $template.thinking_toggle: false
          enable_thinking: false
`)
	cases := []struct {
		name  string
		entry string
		ep    string
		prof  string
		wants string
	}{
		{"dynamic key, no parameter anywhere", "bare", unresolved, "no_params", "neither its template_defaults nor the profile names it"},
		{"non-boolean dynamic switch", "bad", nonBool, "kimi_k2_6", "boolean literal"},
		{"literal and substituted key collide", "clash", collide, "no_params", `renders key "enable_thinking" twice`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(Settings{EndpointType: tc.entry, ProfileID: tc.prof,
				EndpointsFile: tc.ep, ProfilesFile: writeCatalogue(t, "p.yaml", v2Profiles), MaxOutput: 16384})
			if err == nil {
				t.Fatal("accepted a configuration error")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q lacks %q", err, tc.wants)
			}
		})
	}
	// The same entry beside a profile that calls the switch `thinking`
	// no longer collides on one key — but the audit still catches the two
	// fields driving the same setting, which is the point of running it
	// over RENDERED paths (§7.4).
	_, err := Build(Settings{EndpointType: "clash", ProfileID: "kimi_k2_6",
		EndpointsFile: collide, ProfilesFile: writeCatalogue(t, "p2.yaml", v2Profiles)})
	if err == nil || !strings.Contains(err.Error(), "drives the thinking switch through 2 different request fields") {
		t.Errorf("alias audit over rendered paths: %v", err)
	}
}

// A fragment that spells its template switch literally is untouched by what
// a profile calls its switch: a profile parameter has an effect only where
// the endpoint fragment references the placeholder. Version 1 files are
// refused outright — this build speaks one catalogue dialect.
func TestLiteralFragmentsAndSchemaGate(t *testing.T) {
	literal := writeCatalogue(t, "endpoints.yaml", `
schema_version: 2
endpoints:
  old:
    protocol: openai_chat
    path: /chat/completions
    requests:
      'off':
        chat_template_kwargs:
          enable_thinking: false
      effort:
        chat_template_kwargs:
          enable_thinking: true
        reasoning_effort: $effort
    effort_values: [low, medium, high, xhigh]
`)
	profiles := writeCatalogue(t, "profiles.yaml", `
schema_version: 2
profiles:
  legacy:
    options:
    - {id: 'off', label: 'Off', mode: 'off'}
    - {id: low, label: Low, mode: effort, effort: low}
`)
	// The body is exactly what the file says, and a profile naming a
	// different switch does not rewrite it automatically.
	if got := fragmentFor(t, literal, "old", profiles, "legacy", "off"); got != `{"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("off = %s", got)
	}
	if got := fixtureFragment(t, literal, "old", "kimi_k2_6", "off"); got != `{"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("literal endpoint + a profile naming `thinking` = %s, want the literal field untouched", got)
	}
	// Version 1 is not translated, it is rejected, with the version this
	// build speaks in the message.
	endpoints := "schema_version: 1\nendpoints:\n  a:\n    protocol: openai_chat\n    path: /c\n    requests: {'off': {x: 1}}\n"
	profilesFile := "schema_version: 1\nprofiles:\n  a:\n    options: [{id: 'off', label: 'Off', mode: 'off'}]\n"
	_, err := Build(Settings{EndpointType: "a", ProfileID: "a",
		EndpointsFile: writeCatalogue(t, "e1.yaml", endpoints),
		ProfilesFile:  writeCatalogue(t, "p1.yaml", profilesFile)})
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("version 1 catalogues accepted: %v", err)
	}
}

// Any empty-options profile hides the selector — not just the built-in
// `none` — and hides it without forcing the model's thinking off (§6).
func TestEmptyProfileHidesSelectorWithoutForcingOff(t *testing.T) {
	rt, err := Build(catSettings(Settings{EndpointType: "gufo", ProfileID: "empty",
		ProfilesFile: writeCatalogue(t, "p.yaml", v2Profiles)}))
	if err != nil {
		t.Fatal(err)
	}
	if rt.SelectorVisible() {
		t.Error("an empty profile must hide the selector")
	}
	if got := idsOf(rt.Choices("m")); len(got) != 1 || got[0] != DefaultID {
		t.Errorf("choices = %v, want Default only", got)
	}
	if frag := rt.QuietFragment("m"); frag != nil {
		t.Errorf("quiet fragment = %v, want nothing sent: omission is not Off", frag)
	}
}

// A saved choice belongs to the definitions it was made under. Editing an
// endpoint recipe or a profile's switch name under the SAME ids resets it
// visibly to Default; a reordering that changes no meaning does not
// (addendum §7.8).
func TestFingerprintTracksCatalogueMeaning(t *testing.T) {
	const model = "Kimi K2.6"
	write := func(t *testing.T, epBody, profBody string) *Runtime {
		t.Helper()
		rt, err := Build(Settings{EndpointURL: "http://h/v1", EndpointType: "vllm", ProfileID: "kimi_k2_6",
			EndpointsFile: writeCatalogue(t, "e.yaml", epBody),
			ProfilesFile:  writeCatalogue(t, "p.yaml", profBody), MaxOutput: 16384})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	vllm := `
schema_version: 2
endpoints:
  vllm:
    protocol: openai_chat
    path: /chat/completions
    template_defaults:
      thinking_toggle: enable_thinking
    requests:
      'off':
        chat_template_kwargs:
          $template.thinking_toggle: false
      'on':
        chat_template_kwargs:
          $template.thinking_toggle: true
`
	vllmRenamed := strings.Replace(vllm, "thinking_toggle: enable_thinking", "thinking_toggle: thinking", 1)
	vllmReordered := strings.Replace(vllm,
		"    protocol: openai_chat\n    path: /chat/completions\n",
		"    path: /chat/completions\n    protocol: openai_chat\n", 1)

	home := write(t, vllm, v2Profiles)
	ctx := home.Fingerprint(model)
	if got, ok := home.ResolveSaved("off", ctx, model); !ok || got != "off" {
		t.Fatalf("intact saved choice = %q (%v)", got, ok)
	}
	// Same ids, same meaning, different file layout: still applicable.
	if reordered := write(t, vllmReordered, v2Profiles); reordered.Fingerprint(model) != ctx {
		t.Error("fingerprint moved on a layout-only edit")
	}
	// Same ids, different meaning: the saved choice no longer applies.
	if renamed := write(t, vllmRenamed, v2Profiles); renamed.Fingerprint(model) == ctx {
		t.Error("fingerprint ignores a changed endpoint default")
	} else if _, ok := renamed.ResolveSaved("off", ctx, model); ok {
		t.Error("a saved choice survived the meaning of its endpoint changing")
	}
	// The profile's own switch name is part of it too.
	otherProf := strings.Replace(v2Profiles, "thinking_toggle: thinking", "thinking_toggle: enable_thinking", 1)
	if changed := write(t, vllm, otherProf); changed.Fingerprint(model) == ctx {
		t.Error("fingerprint ignores a changed profile switch name")
	}
}

// Two requests rendering different choices of the same pair concurrently
// must not see each other's values or mutations: fragments are compiled
// once and cloned per request.
func TestConcurrentChoiceRendering(t *testing.T) {
	rt, err := Build(catSettings(Settings{EndpointType: "vllm", ProfileID: "qwen3_8", MaxOutput: 16384}))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"off", "low", "medium", "high"}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		id := ids[i%len(ids)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, ok := rt.ResolveFor("", id)
			if !ok {
				t.Errorf("%s unresolvable", id)
				return
			}
			frag, err := rt.Fragment("", c)
			if err != nil {
				t.Errorf("%s: %v", id, err)
				return
			}
			want := id != "off"
			if got := frag["chat_template_kwargs"].(map[string]any)["enable_thinking"]; got != want {
				t.Errorf("%s: enable_thinking = %v, want %v", id, got, want)
			}
			if m, hasEffort := frag["reasoning_effort"]; hasEffort != want {
				t.Errorf("%s: reasoning_effort present = %v (%v)", id, hasEffort, m)
			}
			// Mutating this request's copy must not reach anyone else's.
			frag["late"] = true
			delete(frag["chat_template_kwargs"].(map[string]any), "enable_thinking")
		}()
	}
	wg.Wait()
	// And the runtime's own copies are untouched.
	high, _ := rt.ResolveFor("", "high")
	frag, err := rt.Fragment("", high)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := frag["late"]; leaked {
		t.Error("a caller's mutation leaked into the compiled fragment")
	}
	if frag["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
		t.Error("a caller's delete leaked into the compiled fragment")
	}
}
