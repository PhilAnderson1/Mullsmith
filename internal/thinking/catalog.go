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
// selection into a Runtime (effective choices, request-route, fragment
// renderer, applicability fingerprint); the chat layer merges rendered
// fragments into the normal request body.
package thinking

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	Protocol     string              `yaml:"protocol"`
	Path         string              `yaml:"path"`
	Requests     map[string]Fragment `yaml:"requests"`
	EffortValues []string            `yaml:"effort_values"`
	BudgetMin    int                 `yaml:"budget_min"`
	BudgetMax    int                 `yaml:"budget_max"` // 0 = no catalogue ceiling
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
// and the warnings their exclusions produced.
type modelState struct {
	profID   string
	choices  []Option
	warnings []string
}

// Runtime is the validated selection: the endpoint fragment recipes, the
// profile catalogue, and the effective choices PER PICKER MODEL (the
// dropdown describes a model, and picker entries may name different
// profiles). states is built eagerly at Build and never mutated after;
// choices for models outside the picker are computed on demand from the
// default profile.
type Runtime struct {
	set    Settings
	ep     *Endpoint
	profs  map[string]*Profile
	states map[string]*modelState
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
	// its usable options NOW — a picker entry naming a missing profile
	// is a startup error, and no lookup happens mid-flight.
	rt := &Runtime{set: set, ep: ep, profs: profiles, states: map[string]*modelState{}}
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
		usable, warnings, err := rt.usableOptions(pid, prof)
		if err != nil {
			return nil, err
		}
		rt.states[pid] = &modelState{profID: pid, choices: usable, warnings: warnings}
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

func decodeEndpoints(raw []byte, src string) (map[string]*Endpoint, error) {
	var f endpointsFile
	if err := decodeStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	if f.SchemaVersion != 1 {
		return nil, fmt.Errorf("%s: unsupported schema_version %d (this build speaks 1)", src, f.SchemaVersion)
	}
	if len(f.Endpoints) == 0 {
		return nil, fmt.Errorf("%s: no endpoints defined", src)
	}
	return f.Endpoints, nil
}

func decodeProfiles(raw []byte, src string) (map[string]*Profile, error) {
	var f profilesFile
	if err := decodeStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	if f.SchemaVersion != 1 {
		return nil, fmt.Errorf("%s: unsupported schema_version %d (this build speaks 1)", src, f.SchemaVersion)
	}
	if len(f.Profiles) == 0 {
		return nil, fmt.Errorf("%s: no profiles defined", src)
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
	for mode, frag := range ep.Requests {
		if !slices.Contains(modes, mode) {
			return fmt.Errorf("endpoint %q: unknown request mode %q (want off, on, effort or budget)", id, mode)
		}
		if err := checkFragment(frag, mode, id); err != nil {
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
// be the one this mode substitutes. Any other $-leading string is a
// misspelling caught at load, not silently forwarded at request time.
func checkFragment(frag Fragment, mode, epID string) error {
	where := fmt.Sprintf("endpoint %q requests[%q]", epID, mode)
	var walk func(path string, v any) error
	walk = func(path string, v any) error {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if strings.HasPrefix(k, "$") {
					return fmt.Errorf("%s%s: keys may not start with $ (%q)", where, path, k)
				}
				if err := walk(path+"."+k, child); err != nil {
					return err
				}
			}
		case Fragment:
			// yaml decodes nested mappings inside a named map type as the
			// same named type — the walk treats it as the map it is.
			for k, child := range map[string]any(t) {
				if strings.HasPrefix(k, "$") {
					return fmt.Errorf("%s%s: keys may not start with $ (%q)", where, path, k)
				}
				if err := walk(path+"."+k, child); err != nil {
					return err
				}
			}
		case []any:
			for i, child := range t {
				if err := walk(fmt.Sprintf("%s[%d]", path, i), child); err != nil {
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
	return walk("", map[string]any(frag))
}

func validateProfile(id string, p *Profile) error {
	if id == "" {
		return errors.New("profiles: an entry has an empty ID")
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

// usableOptions walks the profile in UI order and keeps the options the
// endpoint can actually express (§5). Endpoint limits exclude with a
// warning; a budget preset that could never leave room for the answer is
// a configuration error, and a nonempty profile with nothing usable left
// rejects the combination rather than silently hiding every control.
func (r *Runtime) usableOptions(profID string, prof *Profile) ([]Option, []string, error) {
	var (
		usable   []Option
		warnings []string
	)
	for _, o := range prof.Options {
		if _, ok := r.ep.Requests[o.Mode]; !ok {
			warnings = append(warnings, fmt.Sprintf(
				"profile %q option %q: endpoint %q offers no %q request — choice hidden",
				profID, o.ID, r.set.EndpointType, o.Mode))
			continue
		}
		switch o.Mode {
		case "effort":
			if len(r.ep.EffortValues) > 0 && !slices.Contains(r.ep.EffortValues, o.Effort) {
				warnings = append(warnings, fmt.Sprintf(
					"profile %q option %q: endpoint %q does not accept effort %q — choice hidden",
					profID, o.ID, r.set.EndpointType, o.Effort))
				continue
			}
		case "budget":
			lo, hi := r.budgetRange()
			if o.BudgetTokens < lo || (hi > 0 && o.BudgetTokens > hi) {
				warnings = append(warnings, fmt.Sprintf(
					"profile %q option %q: budget %d outside endpoint %q range — choice hidden",
					profID, o.ID, o.BudgetTokens, r.set.EndpointType))
				continue
			}
			if r.set.MaxOutput > 0 && o.BudgetTokens >= r.set.MaxOutput {
				return nil, nil, fmt.Errorf(
					"profile %q option %q: budget %d leaves no room inside max_tokens_response %d",
					profID, o.ID, o.BudgetTokens, r.set.MaxOutput)
			}
		}
		usable = append(usable, o)
	}
	if len(prof.Options) > 0 && len(usable) == 0 {
		return nil, nil, fmt.Errorf(
			"profile %q has no option endpoint %q can express (%s) — pick a different combination",
			profID, r.set.EndpointType, strings.Join(warnings, "; "))
	}
	return usable, warnings, nil
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
// endpoint URL, model ID, endpoint type and the profile IN FORCE FOR
// THAT MODEL — never the API key (§5). A model switch therefore keeps a
// saved choice only when its profile and choices genuinely still apply.
func (r *Runtime) Fingerprint(model string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		r.set.EndpointURL, model, r.set.EndpointType, r.profileID(model),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
