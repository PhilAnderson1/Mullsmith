package thinking

import (
	"fmt"
	"maps"
	"slices"
)

// Fragment renders the request JSON a choice adds to the normal chat
// request — the whole thinking operation, nothing else. Default adds no
// override (nil): server/model settings stay in force. Every other choice
// is the compiled fragment of its endpoint recipe for its mode: rendered
// once at Build (tokens substituted, the dynamic template key resolved
// against the endpoint/profile pair) and DEEP-COPIED here, so a request
// never inherits a previous turn's fields and the chat layer is free to
// merge it destructively into its payload (§3, addendum §6).
//
// QuietFragment renders the gentlest request fragment the catalogues can
// express for a model — the side-quest choice (title quest, §12): the
// endpoint-expressible "off" option, else the profile's lightest rung.
// No selector, no options, or a selector turned off: nil — the caller's
// own fallback stands. A render error cannot happen for a Quietest
// option (they are built from the same validated state); nil it too, so
// side quests stay silent rather than fail loud.
func (r *Runtime) QuietFragment(model string) map[string]any {
	ch, ok := r.Quietest(model)
	if !ok {
		return nil
	}
	frag, err := r.Fragment(model, ch)
	if err != nil {
		return nil
	}
	return frag
}

func (r *Runtime) Fragment(model string, c Choice) (map[string]any, error) {
	if r == nil {
		return nil, fmt.Errorf("thinking control is not configured; choice %q cannot be sent", c.ID)
	}
	if !r.SelectorVisible() {
		return nil, fmt.Errorf("thinking selector is off (profile %q): choice %q cannot be sent", r.profileID(model), c.ID)
	}
	if c.ID == DefaultID {
		return nil, nil
	}
	st := r.stateFor(model)
	opt, ok := r.ResolveFor(model, c.ID)
	if !ok {
		// Never trust a client-provided choice past this point (§6).
		return nil, fmt.Errorf("unknown thinking choice %q for profile %q", c.ID, r.profileID(model))
	}
	switch c.Mode {
	case "effort":
		if c.Effort != opt.Effort {
			return nil, fmt.Errorf("choice %q does not carry effort %q", c.ID, opt.Effort)
		}
		if len(r.ep.EffortValues) > 0 && !slices.Contains(r.ep.EffortValues, c.Effort) {
			return nil, fmt.Errorf("endpoint %q does not accept effort %q (choice %q)", r.set.EndpointType, c.Effort, c.ID)
		}
	case "budget":
		if c.BudgetTokens != opt.BudgetTokens || c.BudgetTokens <= 0 {
			return nil, fmt.Errorf("choice %q does not carry its positive budget", c.ID)
		}
		lo, hi := r.budgetRange()
		if c.BudgetTokens < lo || (hi > 0 && c.BudgetTokens > hi) {
			return nil, fmt.Errorf("choice %q budget %d outside endpoint %q range [%d, %s]",
				c.ID, c.BudgetTokens, r.set.EndpointType, lo, hiOrOpen(hi))
		}
	}
	frag, ok := st.compiled[c.ID]
	if !ok {
		return nil, fmt.Errorf("choice %q has no compiled fragment under profile %q", c.ID, st.profID)
	}
	return cloneFragment(frag), nil
}

func hiOrOpen(hi int) string {
	if hi > 0 {
		return fmt.Sprintf("%d", hi)
	}
	return "∞"
}

// compile renders one request fragment into the immutable form a choice
// carries: whole reserved tokens replaced with the option's values, and
// every $template.thinking_toggle key renamed to the property this pair
// resolved (addendum §5). Rules, in order:
//
//   - a dynamic key whose parameter resolves to a name is renamed, its
//     boolean value untouched;
//   - a dynamic key whose parameter resolved to null disappears;
//   - an object left empty SOLELY because of such omissions disappears too,
//     recursively — while explicitly authored empty objects and unrelated
//     members survive;
//   - a key produced twice (a literal name colliding with a substituted
//     one) is a configuration error, never a silent winner chosen by
//     iteration order;
//   - $effort and $budget_tokens substitute wholesale, keeping their
//     string/integer types;
//   - anything else travels as it was authored.
func compile(v any, c Choice, pr params, epID, path string) (any, bool, error) {
	switch t := v.(type) {
	case map[string]any:
		return compileMap(t, c, pr, epID, path)
	case Fragment: // nested named map from the YAML decoder
		return compileMap(map[string]any(t), c, pr, epID, path)
	case []any:
		out := make([]any, 0, len(t))
		for _, child := range t {
			cv, omitted, err := compile(child, c, pr, epID, path)
			if err != nil {
				return nil, false, err
			}
			if omitted {
				continue // a pruned member leaves the list
			}
			out = append(out, cv)
		}
		return out, false, nil
	case string:
		switch t {
		case tokenEffort:
			if c.Mode != "effort" || c.Effort == "" {
				return nil, false, fmt.Errorf("%s has no value for this choice", tokenEffort)
			}
			return c.Effort, false, nil
		case tokenBudget:
			if c.Mode != "budget" || c.BudgetTokens <= 0 {
				return nil, false, fmt.Errorf("%s has no value for this choice", tokenBudget)
			}
			return c.BudgetTokens, false, nil // stays an integer, never a numeric string
		}
		return t, false, nil
	default:
		return v, false, nil // booleans stay booleans, numbers stay numbers
	}
}

// compileMap compiles one object and reports whether it should be pruned
// because omitting dynamic keys emptied it.
func compileMap(t map[string]any, c Choice, pr params, epID, path string) (any, bool, error) {
	out := make(map[string]any, len(t))
	prunedAny := false
	for _, k := range slices.Sorted(maps.Keys(t)) {
		target := k
		if k == tokenTemplateKey {
			if _, ok := t[k].(bool); !ok {
				return nil, false, fmt.Errorf("%s%s: the dynamic switch value must be a boolean literal, not %T", path, k, t[k])
			}
			p, named := pr[paramThinkingToggle]
			if !named {
				return nil, false, fmt.Errorf("%s%s: endpoint %q references the %q parameter but neither its template_defaults nor the profile names it",
					path, k, epID, paramThinkingToggle)
			}
			if p.null {
				prunedAny = true // §5 rule 4: no switch in this request
				continue
			}
			target = p.name
		} else if _, ok := out[k]; ok {
			return nil, false, fmt.Errorf("%s%s: renders key %q twice", path, k, k)
		}
		cv, omitted, err := compile(t[k], c, pr, epID, path+"."+target)
		if err != nil {
			return nil, false, err
		}
		if omitted {
			prunedAny = true
			continue
		}
		if _, clash := out[target]; clash {
			return nil, false, fmt.Errorf("%s%s: renders key %q twice (a literal key collides with a substituted one)", path, k, target)
		}
		out[target] = cv
	}
	if len(out) == 0 && prunedAny {
		return nil, true, nil
	}
	return out, false, nil
}

// cloneJSON deep-copies a compiled fragment: nested objects and lists
// included, so a caller mutating its merged payload cannot reach the
// runtime's copy or another request's.
func cloneJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = cloneJSON(child)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, child := range t {
			out = append(out, cloneJSON(child))
		}
		return out
	default:
		return v
	}
}

// cloneFragment copies one compiled root for a single request.
func cloneFragment(frag map[string]any) map[string]any {
	out := make(map[string]any, len(frag))
	for k, v := range frag {
		out[k] = cloneJSON(v)
	}
	return out
}
