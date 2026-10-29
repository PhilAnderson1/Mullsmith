package thinking

import (
	"fmt"
	"slices"
)

// Fragment renders the request JSON a choice adds to the normal chat
// request — the whole thinking operation, nothing else. Default adds no
// override (nil): server/model settings stay in force. Every other choice
// copies its endpoint's ONE fragment for its mode and substitutes the
// permitted token; effort and budget never merge, because a fragment is
// already complete for its operation (§3). The result is a fresh copy on
// every call, so a request never inherits a previous turn's fields and
// the chat layer is free to merge it destructively into its payload.
func (r *Runtime) Fragment(c Choice) (map[string]any, error) {
	if r == nil {
		return nil, fmt.Errorf("thinking control is not configured; choice %q cannot be sent", c.ID)
	}
	if !r.SelectorVisible() {
		return nil, fmt.Errorf("thinking selector is off (profile %q): choice %q cannot be sent", r.profID, c.ID)
	}
	if c.ID == DefaultID {
		return nil, nil
	}
	if _, ok := r.Resolve(c.ID); !ok {
		// Never trust a client-provided choice past this point (§6).
		return nil, fmt.Errorf("unknown thinking choice %q for profile %q", c.ID, r.profID)
	}
	frag, ok := r.ep.Requests[c.Mode]
	if !ok {
		return nil, fmt.Errorf("endpoint %q offers no %q request (choice %q)", r.set.EndpointType, c.Mode, c.ID)
	}
	switch c.Mode {
	case "effort":
		if c.Effort == "" {
			return nil, fmt.Errorf("choice %q carries no effort to substitute", c.ID)
		}
		if len(r.ep.EffortValues) > 0 && !slices.Contains(r.ep.EffortValues, c.Effort) {
			return nil, fmt.Errorf("endpoint %q does not accept effort %q (choice %q)", r.set.EndpointType, c.Effort, c.ID)
		}
	case "budget":
		if c.BudgetTokens <= 0 {
			return nil, fmt.Errorf("choice %q carries no positive budget to substitute", c.ID)
		}
		lo, hi := r.budgetRange()
		if c.BudgetTokens < lo || (hi > 0 && c.BudgetTokens > hi) {
			return nil, fmt.Errorf("choice %q budget %d outside endpoint %q range [%d, %s]",
				c.ID, c.BudgetTokens, r.set.EndpointType, lo, hiOrOpen(hi))
		}
	}
	out, err := substitute(frag, c)
	if err != nil {
		return nil, fmt.Errorf("choice %q: %w", c.ID, err)
	}
	om, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("choice %q: fragment did not render to an object", c.ID)
	}
	return om, nil
}

func hiOrOpen(hi int) string {
	if hi > 0 {
		return fmt.Sprintf("%d", hi)
	}
	return "∞"
}

// substitute deep-copies a fragment, replacing whole reserved tokens with
// the selected option's values. Substitution happens nowhere else: not in
// keys, not inside larger strings, no expressions, no environment —
// there is no template language here (§3).
func substitute(v any, c Choice) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			cv, err := substitute(child, c)
			if err != nil {
				return nil, fmt.Errorf(".%s%w", k, err)
			}
			out[k] = cv
		}
		return out, nil
	case Fragment: // nested named map from the YAML decoder
		out := make(map[string]any, len(t))
		for k, child := range map[string]any(t) {
			cv, err := substitute(child, c)
			if err != nil {
				return nil, fmt.Errorf(".%s%w", k, err)
			}
			out[k] = cv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for _, child := range t {
			cv, err := substitute(child, c)
			if err != nil {
				return nil, err
			}
			out = append(out, cv)
		}
		return out, nil
	case string:
		switch t {
		case tokenEffort:
			if c.Mode != "effort" || c.Effort == "" {
				return nil, fmt.Errorf("%s has no value for this choice", tokenEffort)
			}
			return c.Effort, nil
		case tokenBudget:
			if c.Mode != "budget" || c.BudgetTokens <= 0 {
				return nil, fmt.Errorf("%s has no value for this choice", tokenBudget)
			}
			return c.BudgetTokens, nil // stays an integer, never a numeric string
		}
		return t, nil
	default:
		return v, nil // booleans stay booleans, numbers stay numbers
	}
}
