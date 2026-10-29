// Package thinking implements the configurable thinking-level control
// (thinking-control-specification.md). Two catalogues drive it: endpoints
// describe how one request convention expresses a thinking choice as JSON
// request fields, profiles list the ordered choices for the single
// configured model. Both ship embedded in the binary (data/) and may be
// extended or replaced per ID by operator-supplied YAML files — a new
// profile or request mapping is a YAML edit plus a restart, never a
// recompile.
//
// The package knows nothing about HTTP: Build resolves the configured
// selection into a Runtime (effective choices, request-route, fragment
// renderer, applicability fingerprint); the chat layer merges rendered
// fragments into the normal request body.
package thinking

import (
	"bytes"
	"crypto/sha256"
	_ "embed" // required by the go:embed directives below
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed data/endpoints.yaml
var embeddedEndpoints []byte

//go:embed data/thinking-profiles.yaml
var embeddedProfiles []byte

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
	EndpointURL   string
	EndpointType  string
	ProfileID     string
	MaxOutput     int // llm.max_tokens_response; 0 = uncapped
	EndpointsFile string
	ProfilesFile  string
	// SelectorEnable is llm.thinking_selector_enable: nil falls back to
	// the legacy shape (a selector exists when an endpoint type or a
	// non-"none" profile is spelled out); true/false decides outright.
	// A true switch still requires an explicit endpoint_type and profile
	// to name — the sample config shows the usual pair.
	SelectorEnable *bool
}

// Runtime is one validated, resolved selection. A nil *Runtime means the
// feature is off: legacy configuration, no selector, existing transport.
type Runtime struct {
	set      Settings
	ep       *Endpoint
	profID   string
	choices  []Option // usable profile options, UI order, Default excluded
	warnings []string
}

// Build parses the catalogues (embedded, then per-ID external
// replacement/addition) and resolves the configured selection. It returns
// (nil, nil) when the selector is off — explicitly via SelectorEnable,
// or by the legacy shape of nothing thinking-related configured, which
// must keep working untouched.
func Build(set Settings) (*Runtime, error) {
	// The explicit switch decides outright; without it the selector
	// exists exactly when the operator spelled out an endpoint type or a
	// non-"none" profile (the pre-switch behaviour, kept intact).
	explicit := set.SelectorEnable != nil
	enabled := set.EndpointType != "" || (set.ProfileID != "" && set.ProfileID != "none")
	if explicit {
		enabled = *set.SelectorEnable
	}
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
	endpoints, profiles, err := loadCatalogues(set)
	if err != nil {
		return nil, err
	}
	epType, profID := set.EndpointType, set.ProfileID
	if epType == "" {
		if explicit {
			return nil, errors.New("llm.thinking_selector_enable requires llm.endpoint_type")
		}
		return nil, fmt.Errorf("llm.thinking_profile %q requires llm.endpoint_type", set.ProfileID)
	}
	ep, ok := endpoints[epType]
	if !ok {
		return nil, fmt.Errorf("llm.endpoint_type %q: no such entry in the endpoint catalogue", epType)
	}
	if profID == "" {
		profID = "none"
	}
	prof, ok := profiles[profID]
	if !ok {
		return nil, fmt.Errorf("llm.thinking_profile %q: no such entry in the profile catalogue", profID)
	}
	if ep.Protocol != protocolOpenAI {
		return nil, fmt.Errorf("llm.endpoint_type %q uses protocol %q, which this build does not implement (only %q); "+
			"selecting it would send a request body the native route cannot read",
			set.EndpointType, ep.Protocol, protocolOpenAI)
	}
	rt := &Runtime{set: set, ep: ep, profID: profID}
	usable, warnings, err := rt.usableOptions(prof)
	if err != nil {
		return nil, err
	}
	rt.choices = usable
	rt.warnings = warnings
	return rt, nil
}

// loadCatalogues decodes embedded + external files, replaces embedded
// entries by matching ID (the entire entry, never a recursive merge) and
// adds new IDs, then validates every resulting entry.
func loadCatalogues(set Settings) (map[string]*Endpoint, map[string]*Profile, error) {
	ef, err := decodeEndpoints(embeddedEndpoints, "embedded endpoints")
	if err != nil {
		return nil, nil, err
	}
	pf, err := decodeProfiles(embeddedProfiles, "embedded profiles")
	if err != nil {
		return nil, nil, err
	}
	if set.EndpointsFile != "" {
		raw, err := readFile(set.EndpointsFile)
		if err != nil {
			return nil, nil, err
		}
		over, err := decodeEndpoints(raw, set.EndpointsFile)
		if err != nil {
			return nil, nil, err
		}
		for id, ep := range over {
			ef[id] = ep
		}
	}
	if set.ProfilesFile != "" {
		raw, err := readFile(set.ProfilesFile)
		if err != nil {
			return nil, nil, err
		}
		over, err := decodeProfiles(raw, set.ProfilesFile)
		if err != nil {
			return nil, nil, err
		}
		for id, p := range over {
			pf[id] = p
		}
	}
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
func (r *Runtime) usableOptions(prof *Profile) ([]Option, []string, error) {
	var (
		usable   []Option
		warnings []string
	)
	for _, o := range prof.Options {
		if _, ok := r.ep.Requests[o.Mode]; !ok {
			warnings = append(warnings, fmt.Sprintf(
				"profile %q option %q: endpoint %q offers no %q request — choice hidden",
				r.profID, o.ID, r.set.EndpointType, o.Mode))
			continue
		}
		switch o.Mode {
		case "effort":
			if len(r.ep.EffortValues) > 0 && !slices.Contains(r.ep.EffortValues, o.Effort) {
				warnings = append(warnings, fmt.Sprintf(
					"profile %q option %q: endpoint %q does not accept effort %q — choice hidden",
					r.profID, o.ID, r.set.EndpointType, o.Effort))
				continue
			}
		case "budget":
			lo, hi := r.budgetRange()
			if o.BudgetTokens < lo || (hi > 0 && o.BudgetTokens > hi) {
				warnings = append(warnings, fmt.Sprintf(
					"profile %q option %q: budget %d outside endpoint %q range — choice hidden",
					r.profID, o.ID, o.BudgetTokens, r.set.EndpointType))
				continue
			}
			if r.set.MaxOutput > 0 && o.BudgetTokens >= r.set.MaxOutput {
				return nil, nil, fmt.Errorf(
					"profile %q option %q: budget %d leaves no room inside max_tokens_response %d",
					r.profID, o.ID, o.BudgetTokens, r.set.MaxOutput)
			}
		}
		usable = append(usable, o)
	}
	if len(prof.Options) > 0 && len(usable) == 0 {
		return nil, nil, fmt.Errorf(
			"profile %q has no option endpoint %q can express (%s) — pick a different combination",
			r.profID, r.set.EndpointType, strings.Join(warnings, "; "))
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
// the default /chat/completions suffix even when the profile is `none`.
func (r *Runtime) Enabled() bool { return r != nil && r.ep != nil }

// SelectorVisible reports a profile with at least one usable option.
// `none` (or a legacy config) hides the selector entirely: the feature is
// off, and a model that thinks by default keeps thinking.
func (r *Runtime) SelectorVisible() bool { return r != nil && len(r.choices) > 0 }

// Route is the request path appended to the base URL, exactly once.
func (r *Runtime) Route() string {
	if !r.Enabled() {
		return ""
	}
	return r.ep.Path
}

// Warnings are the exclusions logged at startup (hidden options).
func (r *Runtime) Warnings() []string {
	if r == nil {
		return nil
	}
	return r.warnings
}

// Choices returns the full UI list: built-in Default first, then the
// usable profile options in profile order.
func (r *Runtime) Choices() []PublicChoice {
	out := make([]PublicChoice, 0, len(r.choices)+1)
	out = append(out, PublicChoice{ID: DefaultID, Label: DefaultLabel})
	for _, o := range r.choices {
		out = append(out, PublicChoice{ID: o.ID, Label: o.Label})
	}
	return out
}

// Resolve maps a choice ID ("" = Default) to its full option.
func (r *Runtime) Resolve(id string) (Choice, bool) {
	if r == nil {
		return Choice{}, false
	}
	if id == "" || id == DefaultID {
		return Choice{ID: DefaultID, Label: DefaultLabel}, true
	}
	for _, o := range r.choices {
		if o.ID == id {
			return Choice{ID: o.ID, Label: o.Label, Mode: o.Mode,
				Effort: o.Effort, BudgetTokens: o.BudgetTokens}, true
		}
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
	if _, ok := r.Resolve(id); !ok {
		return "", false
	}
	return id, true
}

// Fingerprint is the stable applicability context for a saved choice:
// endpoint URL, model ID, endpoint type and profile ID — never the API
// key (§5).
func (r *Runtime) Fingerprint(model string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		r.set.EndpointURL, model, r.set.EndpointType, r.profID,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
