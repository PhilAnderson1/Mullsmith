package thinking

// Model template parameters (thinking-control-addendum.md). A model's chat
// template may call its boolean thinking switch `enable_thinking`,
// `thinking`, or have no such argument at all. That NAME belongs to the
// profile; whether a request carries template arguments under
// `chat_template_kwargs` at all belongs to the endpoint, which says so by
// referencing the whole-key placeholder $template.thinking_toggle in a
// request fragment and may ship a compatibility default for the name.
//
// So one endpoint definition serves both Qwen (`enable_thinking`) and Kimi
// (`thinking`), while a standardised mapping (OpenRouter's `reasoning`
// object, LM Studio's `reasoning_effort`) is untouched by profile
// parameters it never references.

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// paramThinkingToggle is the only template parameter this build defines.
const paramThinkingToggle = "thinking_toggle"

// tokenTemplateKey is the reserved placeholder, permitted ONLY as a whole
// object key of a request fragment. It renames that key to whatever
// thinking_toggle resolves to; its value must be a boolean literal and
// travels unchanged. It is never a value, a substring, a path expression,
// or part of a template language.
const tokenTemplateKey = "$template." + paramThinkingToggle

// paramNamePattern is what a switch may be called: one JSON property name,
// never a dotted path.
var paramNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Param is one template parameter in its three permitted states, which
// must stay distinguishable after parsing: absent (no entry in the map at
// all — use the endpoint's default), explicit null (present with null set:
// never emit this switch, which is NOT the same as Off), or a literal
// property name.
type Param struct {
	null bool
	name string
}

// TemplateParams is a `template_parameters` (profile) or
// `template_defaults` (endpoint) mapping. It decodes itself because the
// decoder's own null handling cannot tell an explicit null from a missing
// key, and because an unknown parameter name is an error even in a
// definition nothing selects.
type TemplateParams map[string]Param

// UnmarshalYAML reads the mapping node by node: a non-mapping body, a
// duplicate name, an unknown name, and a value that is neither a property
// name nor null all fail at load. A whole `template_parameters:` written
// as null reads as no parameters at all — the three states are per
// parameter, and a profile that means "no switch" nulls that parameter.
func (tp *TemplateParams) UnmarshalYAML(node *yaml.Node) error {
	n := resolveAlias(node)
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("template parameters must be a mapping of %q to a property name or null", paramThinkingToggle)
	}
	out := make(TemplateParams, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := resolveAlias(n.Content[i]), resolveAlias(n.Content[i+1])
		var name string
		if err := kn.Decode(&name); err != nil {
			return fmt.Errorf("template parameter name %q: %w", kn.Value, err)
		}
		if _, dup := out[name]; dup {
			return fmt.Errorf("template parameters list %q twice", name)
		}
		if name != paramThinkingToggle {
			return fmt.Errorf("unknown template parameter %q (this build knows %q)", name, paramThinkingToggle)
		}
		p, err := decodeParam(vn)
		if err != nil {
			return fmt.Errorf("template parameter %q: %w", name, err)
		}
		out[name] = p
	}
	*tp = out
	return nil
}

// decodeParam reads one value: explicit null, or a string that is a single
// JSON property name. Booleans, numbers, sequences, mappings and dotted
// paths are wrong types, not things to guess at.
func decodeParam(n *yaml.Node) (Param, error) {
	if n.Kind != yaml.ScalarNode {
		return Param{}, fmt.Errorf("must be a property name or null, not a %s", nodeKind(n))
	}
	if n.Tag == "!!null" {
		return Param{null: true}, nil
	}
	if n.Tag != "!!str" {
		return Param{}, fmt.Errorf("must be a property name or null, not %s", n.Tag)
	}
	var name string
	if err := n.Decode(&name); err != nil {
		return Param{}, err
	}
	if !paramNamePattern.MatchString(name) {
		return Param{}, fmt.Errorf("%q is not a single property name", name)
	}
	return Param{name: name}, nil
}

func nodeKind(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "sequence"
	default:
		return "scalar"
	}
}

// resolveAlias follows an anchor reference so an aliased value is judged on
// what it points at, not on the alias node.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && n.Alias != nil && i < 8; i++ {
		n = n.Alias
	}
	return n
}

// params is the resolved parameter view of ONE endpoint/profile pair
// (addendum §5 rule 1): the endpoint's defaults, overlaid by every
// parameter the profile explicitly presents — including an explicit null,
// which is what beats a default.
type params map[string]Param

func resolveParams(ep *Endpoint, prof *Profile) params {
	p := params{}
	for k, v := range ep.TemplateDefaults {
		p[k] = v
	}
	for k, v := range prof.TemplateParameters {
		p[k] = v
	}
	return p
}

// canon renders the parameters as a hashable tree: an absent parameter has
// no entry, an explicit null is JSON null, a name is that string.
func (p params) canon() any {
	out := map[string]any{}
	for k, v := range p {
		if v.null {
			out[k] = nil
			continue
		}
		out[k] = v.name
	}
	return out
}

// --- semantic-alias audit ------------------------------------------------

// fieldFamilies groups the request fields that drive the SAME thinking
// setting under different spellings (the original specification §6 audit,
// here run over rendered paths because a dynamic template name is only
// known once a pair is resolved). Two fields of one family in a single
// fragment mean two owners of one control.
var fieldFamilies = map[string][]string{
	"thinking switch": {"enable_thinking", "thinking", "think", "thinking_enabled", "enabled"},
	"effort":          {"reasoning_effort", "reasoning", "effort"},
	"token budget":    {"reasoning_budget_tokens", "thinking_budget_tokens", "thinking_token_budget", "budget_tokens", "max_tokens"},
}

// familyOf names the setting one final path segment drives, "" for a field
// this audit has no opinion about.
func familyOf(segment string) string {
	for fam, names := range fieldFamilies {
		if slices.Contains(names, segment) {
			return fam
		}
	}
	return ""
}

// pathsByFamily collects the rendered field paths of one fragment, grouped
// by the setting they drive.
func pathsByFamily(frag map[string]any) map[string][]string {
	out := map[string][]string{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(t)) {
				walk(path+"."+k, t[k])
			}
		case []any:
			for _, child := range t {
				walk(path, child)
			}
		default:
			// A leaf drives whatever its final property name drives.
			if fam := familyOf(path[strings.LastIndex(path, ".")+1:]); fam != "" {
				out[fam] = append(out[fam], path)
			}
		}
	}
	walk("", frag)
	return out
}

// auditPaths checks the rendered fragments of one resolved pair (addendum
// §7.4): an error when a single fragment drives the same setting through
// two different fields, a warning when the pair's choices disagree about
// which field drives it. Names are already substituted here, so a literal
// key and a dynamic one that mean the same control cannot slip past.
func auditPaths(profID, epID string, order []string, compiled map[string]map[string]any) ([]string, error) {
	across := map[string]map[string]string{} // family → path → choice id
	for _, id := range order {
		fams := pathsByFamily(compiled[id])
		for _, fam := range slices.Sorted(maps.Keys(fams)) {
			paths := fams[fam]
			if len(paths) > 1 {
				return nil, fmt.Errorf("profile %q option %q: endpoint %q drives the %s through %d different request fields %s — one control needs one owner",
					profID, id, epID, fam, len(paths), fmt.Sprintf("%q", paths))
			}
			if across[fam] == nil {
				across[fam] = map[string]string{}
			}
			across[fam][paths[0]] = id
		}
	}
	var warnings []string
	for _, fam := range slices.Sorted(maps.Keys(across)) {
		paths := slices.Sorted(maps.Keys(across[fam]))
		if len(paths) < 2 {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"profile %q: endpoint %q drives the %s at %d different request paths across its choices %s — the server may read them as different settings",
			profID, epID, fam, len(paths), fmt.Sprintf("%q", paths)))
	}
	return warnings, nil
}
