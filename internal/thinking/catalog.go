// Package thinking implements the configurable thinking-level control
// (thinking-control-specification.md). Two catalogues drive it: endpoints
// describe how one request convention expresses a thinking choice as JSON
// request fields, profiles list the ordered choices for the single
// configured model. Both live exclusively in the operator-supplied YAML
// files named by llm.endpoints_file / llm.profiles_file — nothing is
// baked into the binary, so a new profile or request mapping is a YAML
// edit plus a restart, never a recompile, and removing an entry from a
// file truly removes it.
//
// The package knows nothing about HTTP: Build resolves the configured
// selection into a Runtime (effective choices, request-route, compiled
// fragments, applicability fingerprint); the chat layer merges rendered
// fragments into the normal request body.
//
// Both files carry a schema_version and are accepted at 1 or 2
// independently, so a mixed pair works. Version 1 keeps its original
// meaning and may not carry the version 2 additions (template_defaults,
// template_parameters, the $template.thinking_toggle key); each entry
// remembers the version of the file it came from, which is what decides
// this — nothing is deep-merged between files, an entry is exactly its
// file says (addendum §2).
package thinking

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// profileNone is the only built-in profile: it selects no choices at all,
// so the selector stays hidden and requests carry no thinking fields. It
// is the profile a selection falls back to when none is named, which is
// why profiles_file is optional exactly then.
const profileNone = "none"

// DefaultID is the built-in choice every visible selector starts with. It
// means "use server/model settings" and adds no request fields; the ID is
// reserved — catalogues may not define an option with it.
const DefaultID = "default"

// DefaultLabel is the Default choice's UI text.
const DefaultLabel = "Default"

// protocolOpenAI is the only transport this build speaks. The catalogues
// may carry native definitions (ollama_chat, lm_studio_chat) unused;
// selecting one is a configuration error, not a fallback.
const protocolOpenAI = "openai_chat"

// modes are the thinking operations a request fragment may provide.
var modes = []string{"off", "on", "effort", "budget"}

// Reserved whole-value substitution tokens (§3).
const (
	tokenEffort = "$effort"
	tokenBudget = "$budget_tokens"
)

// Fragment is one complete request fragment for a thinking mode: JSON-
// compatible values under arbitrary field names. Parsed entries are
// normalised (string-keyed maps, no YAML-only scalars) and token-checked.
type Fragment map[string]any

// Endpoint is one entry under `endpoints`.
type Endpoint struct {
	Protocol string              `yaml:"protocol"`
	Path     string              `yaml:"path"`
	Requests map[string]Fragment `yaml:"requests"`
	// TemplateDefaults is the version 2 compatibility default for the
	// model template parameters a request fragment may reference — the
	// name an older catalogue spelled literally. It is a default, never
	// evidence that every model uses that name, and it has no effect on
	// a fragment that does not reference the parameter.
	TemplateDefaults TemplateParams `yaml:"template_defaults"`
	EffortValues     []string       `yaml:"effort_values"`
	BudgetMin        int            `yaml:"budget_min"`
	BudgetMax        int            `yaml:"budget_max"` // 0 = no catalogue ceiling

	// version is the schema_version of the file this entry was read from.
	// It gates the version 2 fields and placeholder: an external version
	// 1 override can never inherit a new field by accident.
	version int
}

// Option is one entry of a profile's ordered options array.
type Option struct {
	ID           string `yaml:"id"`
	Label        string `yaml:"label"`
	Mode         string `yaml:"mode"`
	Effort       string `yaml:"effort"`
	BudgetTokens int    `yaml:"budget_tokens"`
}

// Profile is one entry under `profiles`.
type Profile struct {
	Options []Option `yaml:"options"`
	// TemplateParameters names this model family's template conventions
	// (version 2): the property its chat template calls its thinking
	// switch, or an explicit null when the template has no such
	// argument. Absent means "use the endpoint's default".
	TemplateParameters TemplateParams `yaml:"template_parameters"`

	version int // see Endpoint.version
}

type endpointsFile struct {
	SchemaVersion int                  `yaml:"schema_version"`
	Endpoints     map[string]*Endpoint `yaml:"endpoints"`
}

type profilesFile struct {
	SchemaVersion int                 `yaml:"schema_version"`
	Profiles      map[string]*Profile `yaml:"profiles"`
}

// Settings carries the llm block's inputs. EndpointURL is the existing
// llm.endpoint: it never moves into a catalogue, the endpoint type only
// selects a request convention. The model ID is deliberately absent —
// Fingerprint takes it per conversation (the picker lets a conversation
// name a different model), so applicability always names the model the
// request will actually carry.
type Settings struct {
	EndpointURL  string
	EndpointType string
	// ProfileID is the DEFAULT entry's profile — the deployment-level
	// fallback for models outside the picker. ModelProfiles names every
	// picker entry's own profile ("" = the built-in "none"); the choice
	// list follows the conversation's model.
	ProfileID     string
	ModelProfiles map[string]string
	MaxOutput     int // llm.max_tokens_response; 0 = uncapped
	EndpointsFile string
	ProfilesFile  string
}

// Runtime is one validated, resolved selection. A nil *Runtime means the
// feature is off: legacy configuration, no selector, existing transport.
// modelState is one model's effective view: its profile's usable options
// with their compiled request fragments, and the warnings their
// exclusions produced.
type modelState struct {
	profID   string
	choices  []Option
	compiled map[string]map[string]any // choice id → immutable rendered fragment
	warnings []string
	canon    string // canonical resolved profile, part of the fingerprint
}

// Runtime is the validated selection: the endpoint fragment recipes, the
// profile catalogue, and the effective choices PER PICKER MODEL (the
// dropdown describes a model, and picker entries may name different
// profiles). states is built eagerly at Build and never mutated after;
// choices for models outside the picker are computed on demand from the
// default profile.
type Runtime struct {
	set     Settings
	ep      *Endpoint
	profs   map[string]*Profile
	states  map[string]*modelState
	epCanon string // canonical resolved endpoint definition
}

// Build parses the catalogue files named by the settings (the sole
// source — nothing is embedded) and resolves the configured selection.
// It returns (nil, nil) when nothing thinking-related is configured —
// the legacy shape, which must keep working untouched. A spelled-out
// endpoint type builds the runtime for its transport (route and
// request recipes) even with no profile anywhere; the DROPDOWN is a
// separate question, answered by SelectorVisible: it appears exactly
// when some model's entry names a real profile.
func Build(set Settings) (*Runtime, error) {
	// The profiles are the selector's reason to exist; the endpoint type
	// is the transport's. Either one builds the runtime, but a type with
	// no profiles leaves nothing to choose, so the selector stays hidden.
	anyProfile := set.ProfileID != "" && set.ProfileID != profileNone
	for _, p := range set.ModelProfiles {
		if p != "" && p != profileNone {
			anyProfile = true
		}
	}
	enabled := set.EndpointType != "" || anyProfile
	if !enabled {
		// No selector. Configured catalogue files are still parsed and
		// validated — an operator who spelled them out wants typos caught
		// at startup even before an endpoint type references them.
		if set.EndpointsFile != "" || set.ProfilesFile != "" {
			if _, _, err := loadCatalogues(set); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	// A spelled-out profile with no endpoint type is the older, more
	// common typo — answered first, exactly as before the files became
	// mandatory.
	epType := set.EndpointType
	if epType == "" {
		if set.ProfileID != "" && set.ProfileID != profileNone {
			return nil, fmt.Errorf("llm.thinking_profile %q requires llm.endpoint_type", set.ProfileID)
		}
		return nil, errors.New("a llm.models thinking_profile requires llm.endpoint_type")
	}
	// The catalogue files are the only place an ID can come from, so a
	// selection is unusable without the file its IDs live in.
	if set.EndpointsFile == "" {
		return nil, errors.New("llm.endpoint_type requires llm.endpoints_file: the endpoint catalogue is an external file")
	}
	defPID := set.ProfileID
	if defPID == "" {
		defPID = profileNone
	}
	endpoints, profiles, err := loadCatalogues(set)
	if err != nil {
		return nil, err
	}
	ep, ok := endpoints[epType]
	if !ok {
		return nil, fmt.Errorf("llm.endpoint_type %q: no such entry in the endpoint catalogue", epType)
	}
	if ep.Protocol != protocolOpenAI {
		return nil, fmt.Errorf("llm.endpoint_type %q uses protocol %q, which this build does not implement (only %q); "+
			"selecting it would send a request body the native route cannot read",
			set.EndpointType, ep.Protocol, protocolOpenAI)
	}
	// Every profile any model resolves to is validated and reduced to
	// its usable, compiled options NOW — a picker entry naming a missing
	// profile is a startup error, and no lookup or rendering happens
	// mid-flight.
	rt := &Runtime{set: set, ep: ep, profs: profiles, states: map[string]*modelState{}, epCanon: canonical(endpointCanon(ep))}
	pids := []string{defPID}
	for _, p := range set.ModelProfiles {
		if p == "" {
			p = profileNone
		}
		pids = append(pids, p)
	}
	for _, pid := range pids {
		if _, done := rt.states[pid]; done {
			continue
		}
		if pid != profileNone && set.ProfilesFile == "" {
			return nil, fmt.Errorf("thinking_profile %q requires llm.profiles_file: the profile catalogue is an external file", pid)
		}
		prof, ok := profiles[pid]
		if !ok {
			return nil, fmt.Errorf("thinking_profile %q: no such entry in the profile catalogue", pid)
		}
		st, err := rt.buildState(pid, prof)
		if err != nil {
			return nil, err
		}
		rt.states[pid] = st
	}
	return rt, nil
}

// profileID names the profile in force for a model: its picker entry's
// (an entry spelling no profile means the built-in "none" — entries
// state the truth, they do not inherit), else the deployment default for
// models outside the picker entirely.
func (r *Runtime) profileID(model string) string {
	if pid, ok := r.set.ModelProfiles[model]; ok {
		if pid == "" {
			return profileNone
		}
		return pid
	}
	if r.set.ProfileID != "" {
		return r.set.ProfileID
	}
	return profileNone
}

// stateFor returns the model's validated view; every profile it can name
// was built and checked at Build, so the lookup cannot miss.
func (r *Runtime) stateFor(model string) *modelState {
	return r.states[r.profileID(model)]
}

// loadCatalogues decodes the two operator files — the sole source for
// every catalogue ID — and validates every entry. Callers guarantee the
// files a selection references are set; a file that is merely configured
// (selector off) is parsed and validated too.
func loadCatalogues(set Settings) (map[string]*Endpoint, map[string]*Profile, error) {
	raw, err := readFile(set.EndpointsFile)
	if err != nil {
		return nil, nil, err
	}
	ef, err := decodeEndpoints(raw, set.EndpointsFile)
	if err != nil {
		return nil, nil, err
	}
	pf := map[string]*Profile{}
	if set.ProfilesFile != "" {
		raw, err := readFile(set.ProfilesFile)
		if err != nil {
			return nil, nil, err
		}
		pf, err = decodeProfiles(raw, set.ProfilesFile)
		if err != nil {
			return nil, nil, err
		}
	}
	// "none" is the built-in no-choices profile, always available (which
	// is exactly when profiles_file may be omitted) and always empty. A
	// file may list it as `options: []`; giving it choices would fight
	// the documented meaning of llm.thinking_profile: none, so it fails.
	if p, ok := pf[profileNone]; ok && len(p.Options) > 0 {
		return nil, nil, fmt.Errorf("%s: profile %q is the built-in no-choices entry and must have no options", set.ProfilesFile, profileNone)
	}
	pf[profileNone] = &Profile{}
	for id, ep := range ef {
		if err := validateEndpoint(id, ep); err != nil {
			return nil, nil, err
		}
	}
	for id, p := range pf {
		if err := validateProfile(id, p); err != nil {
			return nil, nil, err
		}
	}
	return ef, pf, nil
}

func readFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator-supplied llm config value
	if err != nil {
		return nil, fmt.Errorf("read thinking catalogue %s: %w", path, err)
	}
	return raw, nil
}

// schema versions this build speaks: 1 (literal fragments only) and 2
// (model template parameters). Anything else, including 0/absent, fails.
func supportedSchema(v int) bool { return v == 1 || v == 2 }

func decodeEndpoints(raw []byte, src string) (map[string]*Endpoint, error) {
	var f endpointsFile
	if err := decodeStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	if !supportedSchema(f.SchemaVersion) {
		return nil, fmt.Errorf("%s: unsupported schema_version %d (this build speaks 1 and 2)", src, f.SchemaVersion)
	}
	if len(f.Endpoints) == 0 {
		return nil, fmt.Errorf("%s: no endpoints defined", src)
	}
	for _, ep := range f.Endpoints {
		ep.version = f.SchemaVersion
	}
	return f.Endpoints, nil
}

func decodeProfiles(raw []byte, src string) (map[string]*Profile, error) {
	var f profilesFile
	if err := decodeStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	if !supportedSchema(f.SchemaVersion) {
		return nil, fmt.Errorf("%s: unsupported schema_version %d (this build speaks 1 and 2)", src, f.SchemaVersion)
	}
	if len(f.Profiles) == 0 {
		return nil, fmt.Errorf("%s: no profiles defined", src)
	}
	for _, p := range f.Profiles {
		p.version = f.SchemaVersion
	}
	return f.Profiles, nil
}

// decodeStrict parses one YAML document with unknown structural fields
// rejected (yaml.v3 additionally rejects duplicate mapping keys by
// default). Exactly one document: a trailing second one is a splice
// nobody asked for.
func decodeStrict(raw []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty document")
		}
		return err
	}
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return errors.New("more than one YAML document")
	}
	return nil
}

// --- entry validation ---------------------------------------------------------

func validateEndpoint(id string, ep *Endpoint) error {
	if id == "" {
		return errors.New("endpoints: an entry has an empty ID")
	}
	if ep.Protocol == "" {
		return fmt.Errorf("endpoint %q: protocol is required", id)
	}
	if !strings.HasPrefix(ep.Path, "/") {
		return fmt.Errorf("endpoint %q: path %q must start with / (it is appended to the base URL, which keeps its own prefix)", id, ep.Path)
	}
	if len(ep.Requests) == 0 {
		return fmt.Errorf("endpoint %q: requests must provide at least one mode", id)
	}
	// A version 1 file keeps its previous meaning outright: it cannot
	// carry the new fields or the new placeholder, so a v1 entry that
	// spells them is a mistake to report, not a thing to honour.
	if ep.version < 2 && ep.TemplateDefaults != nil {
		return fmt.Errorf("endpoint %q: template_defaults requires schema_version 2 (this file says %d)", id, ep.version)
	}
	dynamic := ep.version >= 2
	for mode, frag := range ep.Requests {
		if !slices.Contains(modes, mode) {
			return fmt.Errorf("endpoint %q: unknown request mode %q (want off, on, effort or budget)", id, mode)
		}
		if err := checkFragment(frag, mode, id, dynamic); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, v := range ep.EffortValues {
		if v == "" {
			return fmt.Errorf("endpoint %q: effort_values contains an empty string", id)
		}
		if seen[v] {
			return fmt.Errorf("endpoint %q: effort_values lists %q twice", id, v)
		}
		seen[v] = true
	}
	if ep.BudgetMin < 0 || ep.BudgetMax < 0 {
		return fmt.Errorf("endpoint %q: budget_min/budget_max must be positive integers when set", id)
	}
	if ep.BudgetMin > 0 && ep.BudgetMax > 0 && ep.BudgetMax < ep.BudgetMin {
		return fmt.Errorf("endpoint %q: budget_max %d is below budget_min %d", id, ep.BudgetMax, ep.BudgetMin)
	}
	return nil
}

// checkFragment walks one request fragment: every value must be JSON-
// compatible, keys must be plain strings, and reserved $-tokens may only
// be the ones this mode substitutes. Any other $-leading string is a
// misspelling caught at load, not silently forwarded at request time.
//
// dynamic says whether this entry may use the version 2 whole-key
// placeholder $template.thinking_toggle — as a key at any depth, and only
// with a boolean literal value, which is the one thing the renderer
// renames the key around.
func checkFragment(frag Fragment, mode, epID string, dynamic bool) error {
	where := fmt.Sprintf("endpoint %q requests[%q]", epID, mode)
	return walkFragment(where, "", map[string]any(frag), mode, dynamic)
}

// walkFragment checks one member of a fragment.
func walkFragment(where, path string, v any, mode string, dynamic bool) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if err := walkKey(where, path, k, child, mode, dynamic); err != nil {
				return err
			}
		}
	case Fragment:
		// yaml decodes nested mappings inside a named map type as the
		// same named type — the walk treats it as the map it is.
		for k, child := range map[string]any(t) {
			if err := walkKey(where, path, k, child, mode, dynamic); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range t {
			if err := walkFragment(where, fmt.Sprintf("%s[%d]", path, i), child, mode, dynamic); err != nil {
				return err
			}
		}
	case string:
		if !strings.HasPrefix(t, "$") {
			return nil
		}
		switch mode {
		case "effort":
			if t == tokenEffort {
				return nil
			}
		case "budget":
			if t == tokenBudget {
				return nil
			}
		}
		return fmt.Errorf("%s%s: %q is not a token this mode substitutes", where, path, t)
	case nil, bool, int, int64, float64:
		return nil
	default:
		return fmt.Errorf("%s: value of type %T is not JSON-compatible", where, v)
	}
	return nil
}

// walkKey checks one object member by name. The placeholder is legal only
// as a whole key carrying a boolean; every other $-leading key fails.
func walkKey(where, path, k string, child any, mode string, dynamic bool) error {
	if k == tokenTemplateKey {
		if !dynamic {
			return fmt.Errorf("%s%s: %s requires schema_version 2", where, path, tokenTemplateKey)
		}
		if _, ok := child.(bool); !ok {
			return fmt.Errorf("%s%s: %s must carry a boolean literal, not %T (its value is what the switch is set to)",
				where, path, tokenTemplateKey, child)
		}
		return nil
	}
	if strings.HasPrefix(k, "$") {
		return fmt.Errorf("%s%s: keys may not start with $ (%q)", where, path, k)
	}
	return walkFragment(where, path+"."+k, child, mode, dynamic)
}

func validateProfile(id string, p *Profile) error {
	if id == "" {
		return errors.New("profiles: an entry has an empty ID")
	}
	if p.version < 2 && p.TemplateParameters != nil {
		return fmt.Errorf("profile %q: template_parameters requires schema_version 2 (this file says %d)", id, p.version)
	}
	seen := map[string]bool{}
	for i, o := range p.Options {
		if o.ID == "" {
			return fmt.Errorf("profile %q option %d: id is required", id, i)
		}
		if o.ID == DefaultID {
			return fmt.Errorf("profile %q: option id %q is reserved for the application's built-in choice", id, DefaultID)
		}
		if seen[o.ID] {
			return fmt.Errorf("profile %q: duplicate option id %q", id, o.ID)
		}
		seen[o.ID] = true
		if strings.TrimSpace(o.Label) == "" {
			return fmt.Errorf("profile %q option %q: label is required", id, o.ID)
		}
		if !slices.Contains(modes, o.Mode) {
			return fmt.Errorf("profile %q option %q: unknown mode %q", id, o.ID, o.Mode)
		}
		switch o.Mode {
		case "effort":
			if o.Effort == "" {
				return fmt.Errorf("profile %q option %q: effort is required for mode effort", id, o.ID)
			}
			if o.BudgetTokens != 0 {
				return fmt.Errorf("profile %q option %q: budget_tokens belongs to mode budget only", id, o.ID)
			}
		case "budget":
			if o.BudgetTokens <= 0 {
				return fmt.Errorf("profile %q option %q: budget_tokens must be a positive integer (Off stays its own mode)", id, o.ID)
			}
			if o.Effort != "" {
				return fmt.Errorf("profile %q option %q: effort belongs to mode effort only", id, o.ID)
			}
		default: // off, on carry no mode-specific fields
			if o.Effort != "" || o.BudgetTokens != 0 {
				return fmt.Errorf("profile %q option %q: mode %q takes no effort/budget_tokens", id, o.ID, o.Mode)
			}
		}
	}
	return nil
}

// --- effective choices -----------------------------------------------------------

// buildState resolves one profile against the active endpoint: the options
// the pair can express, each carrying its compiled request fragment, plus
// the warnings their exclusions produced.
//
// Exclusions come in three kinds (§5, addendum §6). Endpoint limits — no
// recipe for the mode, an effort outside the allowlist, a budget outside
// the endpoint's range — hide the option with a warning. A budget preset
// that could never leave room for the answer, and anything the renderer
// cannot make sense of (an unresolved required parameter, colliding keys,
// a non-boolean switch), are configuration errors, never a quietly
// filtered choice. A fragment that renders EMPTY because its dynamic
// switch resolved to null hides the option with a warning too: an empty
// request is not a working control. A nonempty profile with nothing
// usable left rejects the combination rather than hiding every control.
func (r *Runtime) buildState(profID string, prof *Profile) (*modelState, error) {
	pr := resolveParams(r.ep, prof)
	st := &modelState{
		profID:   profID,
		compiled: map[string]map[string]any{},
		canon:    canonical(profileCanon(prof, pr)),
	}
	for _, o := range prof.Options {
		if _, ok := r.ep.Requests[o.Mode]; !ok {
			st.warnings = append(st.warnings, fmt.Sprintf(
				"profile %q option %q: endpoint %q offers no %q request — choice hidden",
				profID, o.ID, r.set.EndpointType, o.Mode))
			continue
		}
		switch o.Mode {
		case "effort":
			if len(r.ep.EffortValues) > 0 && !slices.Contains(r.ep.EffortValues, o.Effort) {
				st.warnings = append(st.warnings, fmt.Sprintf(
					"profile %q option %q: endpoint %q does not accept effort %q — choice hidden",
					profID, o.ID, r.set.EndpointType, o.Effort))
				continue
			}
		case "budget":
			lo, hi := r.budgetRange()
			if o.BudgetTokens < lo || (hi > 0 && o.BudgetTokens > hi) {
				st.warnings = append(st.warnings, fmt.Sprintf(
					"profile %q option %q: budget %d outside endpoint %q range — choice hidden",
					profID, o.ID, o.BudgetTokens, r.set.EndpointType))
				continue
			}
			if r.set.MaxOutput > 0 && o.BudgetTokens >= r.set.MaxOutput {
				return nil, fmt.Errorf(
					"profile %q option %q: budget %d leaves no room inside max_tokens_response %d",
					profID, o.ID, o.BudgetTokens, r.set.MaxOutput)
			}
		}
		c := Choice{ID: o.ID, Label: o.Label, Mode: o.Mode, Effort: o.Effort, BudgetTokens: o.BudgetTokens}
		rendered, omitted, err := compile(r.ep.Requests[o.Mode], c, pr, r.set.EndpointType, "")
		if err != nil {
			return nil, fmt.Errorf("profile %q option %q: %w", profID, o.ID, err)
		}
		frag, isObject := rendered.(map[string]any)
		if omitted || !isObject || len(frag) == 0 {
			st.warnings = append(st.warnings, fmt.Sprintf(
				"profile %q option %q: endpoint %q renders no request fields for it (its template switch resolved to none) — choice hidden",
				profID, o.ID, r.set.EndpointType))
			continue
		}
		st.choices = append(st.choices, o)
		st.compiled[o.ID] = frag
	}
	if len(prof.Options) > 0 && len(st.choices) == 0 {
		return nil, fmt.Errorf(
			"profile %q has no option endpoint %q can express (%s) — pick a different combination",
			profID, r.set.EndpointType, strings.Join(st.warnings, "; "))
	}
	warnings, err := auditPaths(profID, r.set.EndpointType, optionIDs(st.choices), st.compiled)
	if err != nil {
		return nil, err
	}
	st.warnings = append(st.warnings, warnings...)
	return st, nil
}

func optionIDs(opts []Option) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.ID)
	}
	return out
}

// --- canonical definitions (applicability fingerprint) -------------------

// canonical renders one resolved definition as stable text for the
// fingerprint: encoding/json sorts map keys, so the same definitions
// always read the same whatever order the file listed them in, and a
// meaning change under an unchanged ID produces different text (addendum
// §7.8). Catalogues hold no credentials, so nothing needs excluding.
func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v) // never happens for these trees
	}
	return string(b)
}

func endpointCanon(ep *Endpoint) any {
	reqs := make(map[string]any, len(ep.Requests))
	for mode, frag := range ep.Requests {
		reqs[mode] = frag
	}
	return map[string]any{
		"schema_version":    ep.version,
		"protocol":          ep.Protocol,
		"path":              ep.Path,
		"requests":          reqs,
		"effort_values":     ep.EffortValues,
		"budget_min":        ep.BudgetMin,
		"budget_max":        ep.BudgetMax,
		"template_defaults": params(ep.TemplateDefaults).canon(),
	}
}

func profileCanon(prof *Profile, pr params) any {
	opts := make([]any, 0, len(prof.Options))
	for _, o := range prof.Options {
		opts = append(opts, map[string]any{
			"id": o.ID, "label": o.Label, "mode": o.Mode,
			"effort": o.Effort, "budget_tokens": o.BudgetTokens,
		})
	}
	return map[string]any{
		"schema_version": prof.version,
		"options":        opts,
		// The resolved pair view, so an endpoint default this profile
		// relies on counts as part of the profile's meaning too.
		"template_parameters": pr.canon(),
	}
}

// budgetRange resolves the endpoint's budget bounds; budget_min absent
// means 1 (§3).
func (r *Runtime) budgetRange() (lo, hi int) {
	lo, hi = 1, r.ep.BudgetMax
	if r.ep.BudgetMin > 1 {
		lo = r.ep.BudgetMin
	}
	return lo, hi
}

// Choice is one selectable value; PublicChoice is its frontend shape.
type Choice struct {
	ID           string
	Label        string
	Mode         string
	Effort       string
	BudgetTokens int
}

type PublicChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Enabled reports a selected endpoint definition — its route replaces
// the default /chat/completions suffix even when no profile choices are
// selected ("none").
func (r *Runtime) Enabled() bool { return r != nil && r.ep != nil }

// SelectorVisible reports that some picker model has at least one usable
// option. All-"none" (or a legacy config) hides the selector entirely:
// the feature is off, and a model that thinks by default keeps thinking.
func (r *Runtime) SelectorVisible() bool {
	if r == nil || r.ep == nil {
		return false
	}
	for _, st := range r.states {
		if len(st.choices) > 0 {
			return true
		}
	}
	return false
}

// Route is the request path appended to the base URL, exactly once.
func (r *Runtime) Route() string {
	if !r.Enabled() {
		return ""
	}
	return r.ep.Path
}

// Warnings are the exclusions logged at startup (hidden options); each
// line already names the profile that produced it. Iteration is by
// sorted profile id, so the order is stable.
func (r *Runtime) Warnings() []string {
	if r == nil {
		return nil
	}
	pids := make([]string, 0, len(r.states))
	for pid := range r.states {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	var out []string
	for _, pid := range pids {
		out = append(out, r.states[pid].warnings...)
	}
	return out
}

// Choices returns the UI list for one model: built-in Default first,
// then that model's profile's usable options in profile order.
func (r *Runtime) Choices(model string) []PublicChoice {
	st := r.stateFor(model)
	out := make([]PublicChoice, 0, len(st.choices)+1)
	out = append(out, PublicChoice{ID: DefaultID, Label: DefaultLabel})
	for _, o := range st.choices {
		out = append(out, PublicChoice{ID: o.ID, Label: o.Label})
	}
	return out
}

// ResolveFor maps a choice ID ("" = Default) to its full option under
// the model's profile.
func (r *Runtime) ResolveFor(model, id string) (Choice, bool) {
	if r == nil {
		return Choice{}, false
	}
	if id == "" || id == DefaultID {
		return Choice{ID: DefaultID, Label: DefaultLabel}, true
	}
	for _, o := range r.stateFor(model).choices {
		if o.ID == id {
			return Choice{ID: o.ID, Label: o.Label, Mode: o.Mode,
				Effort: o.Effort, BudgetTokens: o.BudgetTokens}, true
		}
	}
	return Choice{}, false
}

// Quietest names the gentlest usable option for a model: the "off" one
// where the endpoint can express it, else the first of the profile's
// choices — catalogues list them lightest-first, so that is the lowest
// effort rung. This is the title quest's pick (§12): side quests run as
// quietly as the catalogues allow, regardless of the conversation's
// selection. False when the model has no options at all.
func (r *Runtime) Quietest(model string) (Choice, bool) {
	if !r.SelectorVisible() {
		return Choice{}, false
	}
	st := r.stateFor(model)
	for _, o := range st.choices {
		if o.Mode == "off" {
			return r.ResolveFor(model, o.ID)
		}
	}
	if len(st.choices) > 0 {
		return r.ResolveFor(model, st.choices[0].ID)
	}
	return Choice{}, false
}

// ResolveSaved validates a persisted (choice, fingerprint) pair against
// the current context: the saved choice survives only when the
// endpoint/model/profile fingerprint still matches AND the choice is
// still available (§5). Anything else is a false — the caller resets
// visibly to Default.
func (r *Runtime) ResolveSaved(id, fingerprint, model string) (string, bool) {
	if r == nil {
		return "", false
	}
	if id == "" {
		return DefaultID, true
	}
	if fingerprint != r.Fingerprint(model) {
		return "", false
	}
	if _, ok := r.ResolveFor(model, id); !ok {
		return "", false
	}
	return id, true
}

// Fingerprint is the stable applicability context for a saved choice:
// endpoint URL, model ID, endpoint type, the profile IN FORCE FOR THAT
// MODEL, and a canonical rendering of the resolved endpoint and profile
// DEFINITIONS themselves — so editing what an entry means (a recipe
// field, an effort list, a switch name) under an unchanged ID changes the
// fingerprint too and resets a saved choice visibly to Default (addendum
// §7.8). Never the API key (§5). A model switch therefore keeps a saved
// choice only when the definitions behind it genuinely still apply.
func (r *Runtime) Fingerprint(model string) string {
	if r == nil {
		return ""
	}
	st := r.stateFor(model)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		r.set.EndpointURL, model, r.set.EndpointType, r.profileID(model),
		r.epCanon + "\x00" + st.canon,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
