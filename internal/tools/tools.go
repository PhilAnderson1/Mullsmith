// Package tools implements the §7 tool framework: registration driven by
// config, execution with per-tool results, and the audit data the server
// persists to tool_calls. Results are marked untrusted data — the model is
// told tool output is data, never instructions (§7.1).
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"mullsmith/internal/llm"
)

// Invocation is one execution of a tool on behalf of a user.
type Invocation struct {
	UserID     string
	Scratchpad string // absolute base dir for tools that access the user's files
	Arguments  string // raw JSON proposed by the model
}

// Result is what a tool hands back.
type Result struct {
	// Content is the JSON (or text) sent to the model; the loop truncates
	// it to the per-call budget before storing/forwarding.
	Content string
	// Summary is the one-line UI card text (§10.2).
	Summary string
	// Data is structured payload for the UI card (e.g. search results);
	// must be JSON-serializable; never contains secrets.
	Data any
	// Failed marks a tool-reported failure whose content should still be sent
	// to the model (so it can self-correct) and persisted with error status.
	Failed bool
	// StatusOnly hides the generic success card in useful-card mode. Structured
	// Data may still drive a dedicated presentation such as an inline image.
	StatusOnly bool
	// AttachPath, when non-empty, is a canonical uploads/-relative image
	// path the chat loop turns into a persisted synthetic user message
	// carrying the picture (view_image): the one way an image can reach
	// the model, since tool answers themselves are text-only.
	AttachPath string
}

// Tool is one registered function (§7.1).
type Tool interface {
	Name() string
	Description() string
	// Parameters returns the JSON Schema for the argument object.
	Parameters() map[string]any
	// Execute runs the call. Returned errors must be sanitized for both
	// the model and the browser (no internals, §9.12).
	Execute(ctx context.Context, inv Invocation) (Result, error)
}

// RichCardTool marks tools whose successful result contains structured data
// intended for the browser, rather than only a completion summary for audit.
type RichCardTool interface{ HasRichCard() bool }

// Approval policies (§2.2 tools.agent.approval).
const (
	ApprovalAuto    = "auto"
	ApprovalConfirm = "confirm"
)

// Registry holds the enabled tools in stable order with their approval
// policy resolved from config.
type Registry struct {
	order    []Tool
	byName   map[string]Tool
	policyFn func(name string) string
	closers  []func() error
	users    UserToolProvider
}

type UserToolProvider interface {
	Specs(userID string) []llm.ToolSpec
	Get(userID, name string) (Tool, bool)
	Policy(userID, name string) string
}

// NewRegistry builds an empty registry. policyFn maps a tool name to
// "auto"|"confirm"; anything other than "auto" is treated as confirm
// (fail closed).
func NewRegistry(policyFn func(name string) string) *Registry {
	if policyFn == nil {
		policyFn = func(string) string { return ApprovalConfirm }
	}
	return &Registry{byName: map[string]Tool{}, policyFn: policyFn}
}

// Register adds a tool (later registrations of the same name lose).
func (r *Registry) Register(t Tool) {
	if r.byName == nil {
		r.byName = map[string]Tool{}
	}
	if _, dup := r.byName[t.Name()]; dup {
		return
	}
	r.byName[t.Name()] = t
	r.order = append(r.order, t)
}

// Specs returns the OpenAI function-call descriptors in registration order.
func (r *Registry) Specs() []llm.ToolSpec {
	var out []llm.ToolSpec
	for _, t := range r.order {
		out = append(out, llm.ToolSpec{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return out
}

func (r *Registry) SpecsFor(userID string) []llm.ToolSpec {
	out := r.Specs()
	if r.users != nil {
		out = append(out, r.users.Specs(userID)...)
	}
	return out
}

// Get looks a tool up by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

func (r *Registry) GetFor(userID, name string) (Tool, bool) {
	if t, ok := r.Get(name); ok {
		return t, true
	}
	if r.users != nil {
		return r.users.Get(userID, name)
	}
	return nil, false
}

func (r *Registry) SetUserToolProvider(p UserToolProvider) { r.users = p }

func (r *Registry) PolicyFor(userID, name string) string {
	if _, ok := r.Get(name); ok {
		return r.Policy(name)
	}
	if r.users != nil && r.users.Policy(userID, name) == ApprovalAuto {
		return ApprovalAuto
	}
	return ApprovalConfirm
}

// StatusOnly reports whether a successful call can be visually quiet. Errors,
// denials and approval prompts remain visible regardless of this classification.
func (r *Registry) StatusOnly(name string) bool {
	t, ok := r.Get(name)
	if !ok {
		return false
	}
	rich, ok := t.(RichCardTool)
	return !ok || !rich.HasRichCard()
}

func (r *Registry) StatusOnlyFor(userID, name string) bool {
	t, ok := r.GetFor(userID, name)
	if !ok {
		return false
	}
	rich, ok := t.(RichCardTool)
	return !ok || !rich.HasRichCard()
}

// AddCloser registers process-lifetime resources owned by tools.
func (r *Registry) AddCloser(close func() error) { r.closers = append(r.closers, close) }

// Close releases tool transports. It is safe to call during server shutdown.
func (r *Registry) Close() error {
	var errs []error
	for i := len(r.closers) - 1; i >= 0; i-- {
		if err := r.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	r.closers = nil
	return errors.Join(errs...)
}

// Policy returns auto|confirm for a registered tool; unregistered or
// misconfigured names confirm (fail closed).
func (r *Registry) Policy(name string) string {
	if r.policyFn(name) == ApprovalAuto {
		return ApprovalAuto
	}
	return ApprovalConfirm
}

// Enabled reports whether any tool is registered.
func (r *Registry) Enabled() bool { return len(r.order) > 0 }

// Names lists registered tool names (diagnostics/tests).
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, t.Name())
	}
	sort.Strings(out)
	return out
}

// TruncateMiddle reduces s to at most maxRunes keeping head and tail with
// an elision marker (spec §6.1 step 5: results truncated, middle-elided).
func TruncateMiddle(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes < 32 { // no room for a marker; plain head cut
		return string(r[:maxRunes])
	}
	keep := maxRunes - 24
	head := keep * 2 / 3
	tail := keep - head
	return fmt.Sprintf("%s\n…[elided %d runes]…\n%s",
		string(r[:head]), len(r)-keep, string(r[len(r)-tail:]))
}

// Elide one-line text for log/card previews.
func Elide(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
