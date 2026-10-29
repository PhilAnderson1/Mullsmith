package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
)

// --- test doubles ---------------------------------------------------------------

// fakeTool is a deterministic registered tool: fixed result (or error),
// recording how often and with what arguments it ran.
type fakeTool struct {
	name     string
	content  string
	summary  string
	execErr  error
	mu       sync.Mutex
	calls    int
	lastArgs string
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake " + f.name }
func (f *fakeTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}
}
func (f *fakeTool) Execute(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	f.mu.Lock()
	f.calls++
	f.lastArgs = inv.Arguments
	f.mu.Unlock()
	if f.execErr != nil {
		return tools.Result{}, f.execErr
	}
	return tools.Result{Content: f.content, Summary: f.summary, Data: []string{"https://example.com/a"}}, nil
}

func (f *fakeTool) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTool) args() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastArgs
}

// toolLLM is a scripted OpenAI-compatible endpoint: streaming chat rounds
// consume scripted replies in order (the last one repeats so a runaway loop
// shows up as a failed assertion, not a hang). Non-streaming requests
// (auto-titler) answer with a plain JSON completion.
type toolLLM struct {
	mu       sync.Mutex
	requests []map[string]any
	replies  [][]string
	n        int
}

func (l *toolLLM) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)

	if streaming, _ := body["stream"].(bool); !streaming { // titler
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"Tool Chat"}}]}`))
		return
	}

	l.mu.Lock()
	idx := l.n
	l.n++
	l.requests = append(l.requests, body)
	chunks := l.replies[len(l.replies)-1]
	if idx < len(l.replies) {
		chunks = l.replies[idx]
	}
	l.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		sseUp(w, c)
	}
	sseUp(w, "[DONE]")
}

func (l *toolLLM) roundCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

func (l *toolLLM) roundBody(i int) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i >= len(l.requests) {
		return nil
	}
	return l.requests[i]
}

// toolCallChunk / answerChunk / finishChunk compose fake upstream streams.
func toolCallChunk(idx int, id, name, args string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		}}},
	}}})
	return string(b)
}

func answerChunk(text string) string {
	return deltaJSON(map[string]any{"content": text})
}

func finishChunk(reason string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":%q}]}`, reason)
}

// newToolServer wires a signed-in session to a server whose registry and
// scripted LLM the test controls.
func newToolServer(t *testing.T, cfg *config.Config, reg *tools.Registry,
	llmReplies [][]string,
) (*authTest, *toolLLM) {
	t.Helper()
	script := &toolLLM{replies: llmReplies}
	fake := newFakeLLM(t, script.handler)
	cfg.LLM.Endpoint = fake.URL

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := New(cfg, db, logger, "test", nil, nil)
	s.tools = reg
	ts := httptest.NewServer(s.Handler())

	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("tools@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a, script
}

func toolChatCfg(t *testing.T, rounds int, policies map[string]string) *config.Config {
	cfg := chatCfg(t, "http://unused.invalid") // endpoint replaced in newToolServer
	cfg.Tools.Agent.MaxToolRounds = rounds
	cfg.Tools.Agent.Approval = policies
	return cfg
}

func registryFor(policies map[string]string, registered ...tools.Tool) *tools.Registry {
	reg := tools.NewRegistry(func(name string) string {
		if p, ok := policies[name]; ok {
			return p
		}
		return "confirm"
	})
	for _, tl := range registered {
		reg.Register(tl)
	}
	return reg
}

// eventStream records every SSE event (ignoring the async titler's
// "title" events) until done/error.
type eventStream struct {
	events []string
	data   map[string]map[string]any // last payload per event name
	done   map[string]any
}

func drainSSE(t *testing.T, rd *sseReader) eventStream {
	t.Helper()
	es := eventStream{data: map[string]map[string]any{}}
	for {
		ev, d := rd.next()
		if ev == "title" {
			continue
		}
		es.events = append(es.events, ev)
		es.data[ev] = d
		if ev == "done" {
			es.done = d
			return es
		}
		if ev == "error" {
			return es
		}
	}
}

// --- end-to-end tool loop -------------------------------------------------------------

func TestChatToolLoopEndToEnd(t *testing.T) {
	search := &fakeTool{name: "web_search", content: `{"results":[]}`, summary: "Searched — 3 result(s)"}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})

	a, script := newToolServer(t, cfg, reg, [][]string{
		{ // round 1: model proposes a call (arguments split over two chunks)
			toolCallChunk(0, "call_9", "web_search", `{"que`),
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ry\":\"kyoto\"}"}}]}}]}`,
			finishChunk("tool_calls"),
		},
		{ // round 2: answered
			answerChunk("Kyoto is "),
			answerChunk("lovely."),
			finishChunk("stop"),
		},
	})
	c := createConv(t, a, map[string]any{"title": "Tools"})

	resp := sendChat(t, a, c.ID, "search kyoto")
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))

	want := []string{"start", "tool_start", "tool_args", "tool_args", "tool_end", "token", "token", "done"}
	if strings.Join(es.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", es.events, want)
	}
	if es.data["tool_start"]["tool"] != "web_search" || es.data["tool_start"]["call_id"] != "call_9" {
		t.Errorf("tool_start = %v", es.data["tool_start"])
	}
	if es.data["tool_end"]["status"] != "ok" || es.data["tool_end"]["summary"] != "Searched — 3 result(s)" {
		t.Errorf("tool_end = %v", es.data["tool_end"])
	}
	if es.done["status"] != "complete" {
		t.Errorf("done = %v", es.done)
	}
	if search.callCount() != 1 || search.args() != `{"query":"kyoto"}` {
		t.Errorf("tool executed %d times with %s", search.callCount(), search.lastArgs)
	}

	// Round-2 upstream request replays the protocol faithfully and the
	// system prompt carries the injection guidance (§7.1).
	if script.roundCount() != 2 {
		t.Fatalf("upstream rounds = %d, want 2", script.roundCount())
	}
	toolMsgs := payloadRole(t, script.roundBody(1), "tool")
	if len(toolMsgs) != 1 || toolMsgs[0]["tool_call_id"] != "call_9" ||
		toolMsgs[0]["content"] != `{"results":[]}` {
		t.Errorf("round-2 tool message = %+v", toolMsgs)
	}
	asstMsgs := payloadRole(t, script.roundBody(1), "assistant")
	if len(asstMsgs) != 1 {
		t.Fatalf("round-2 assistant messages = %+v", asstMsgs)
	}
	calls, _ := asstMsgs[0]["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("round-2 assistant tool_calls = %+v", asstMsgs[0]["tool_calls"])
	}
	b2, _ := json.Marshal(script.roundBody(1))
	if !strings.Contains(string(b2), "untrusted") {
		t.Error("round-2 system prompt lacks tool guidance")
	}

	// Persisted shape: user, assistant(calls), tool, assistant(answer).
	hist, err := a.db.ContextHistory(context.Background(), c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 4 {
		t.Fatalf("history = %d rows: %+v", len(hist), hist)
	}
	if hist[0].Role != "user" || hist[1].Role != "assistant" ||
		hist[2].Role != "tool" || hist[3].Role != "assistant" {
		t.Fatalf("roles = %s/%s/%s/%s", hist[0].Role, hist[1].Role, hist[2].Role, hist[3].Role)
	}
	if len(hist[1].Calls) != 1 || hist[1].Calls[0].LLMCallID != "call_9" || hist[1].Calls[0].Status != "ok" {
		t.Errorf("audit on assistant row: %+v", hist[1].Calls)
	}
	if hist[1].Calls[0].Result != `{"results":[]}` {
		t.Errorf("audit result = %q", hist[1].Calls[0].Result)
	}
	if hist[2].ToolCallID != "call_9" || hist[2].Content != `{"results":[]}` {
		t.Errorf("tool row = %+v", hist[2])
	}
	if hist[3].Content != "Kyoto is lovely." {
		t.Errorf("final answer = %q", hist[3].Content)
	}
}

// --- approvals ------------------------------------------------------------------------

// approvalCase is a live generation parked on an awaiting_approval event.
type approvalCase struct {
	a          *authTest
	script     *toolLLM
	conv       convResp
	resp       *http.Response
	rd         *sseReader
	genID      string
	approvalID string
}

func startApprovalFlow(t *testing.T, risky *fakeTool) *approvalCase {
	t.Helper()
	reg := registryFor(map[string]string{"code_run": "confirm"}, risky)
	cfg := toolChatCfg(t, 4, map[string]string{"code_run": "confirm"})
	a, script := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "call_x", "code_run", `{"script":"ls"}`), finishChunk("tool_calls")},
		{answerChunk("understood"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Risk"})
	resp := sendChat(t, a, c.ID, "run it")
	rd := newSSEReader(t, resp)

	ac := &approvalCase{a: a, script: script, conv: c, resp: resp, rd: rd}
	for {
		ev, d := rd.next()
		switch ev {
		case "start":
			ac.genID, _ = d["generation_id"].(string)
		case "awaiting_approval":
			ac.approvalID, _ = d["approval_id"].(string)
			if ac.approvalID == "" || ac.genID == "" {
				t.Fatalf("awaiting_approval payload = %v (gen %q)", d, ac.genID)
			}
			if d["tool"] != "code_run" {
				t.Fatalf("approval tool = %v", d["tool"])
			}
			return ac
		case "title", "tool_start", "tool_args":
			continue
		default:
			t.Fatalf("unexpected event %q before awaiting_approval", ev)
		}
	}
}

func TestApprovalApproveRunsTool(t *testing.T) {
	risky := &fakeTool{name: "code_run", content: `{"exit":0}`, summary: "ran"}
	ac := startApprovalFlow(t, risky)
	defer ac.resp.Body.Close()

	if r := ac.a.do(http.MethodPost, "/api/v1/generations/"+ac.genID+"/approvals",
		map[string]any{"approval_id": ac.approvalID, "approve": true}); r.StatusCode != 204 {
		t.Fatalf("approve: %d %s", r.StatusCode, dumpBody(r))
	}
	es := drainSSE(t, ac.rd)
	if risky.callCount() != 1 {
		t.Errorf("tool ran %d times after approval", risky.callCount())
	}
	if es.data["tool_end"]["status"] != "ok" {
		t.Errorf("tool_end = %v", es.data["tool_end"])
	}
	if es.done["status"] != "complete" {
		t.Errorf("done = %v", es.done)
	}

	// Audit row went pending_approval → approved → ok.
	hist, err := ac.a.db.ContextHistory(context.Background(), ac.conv.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist[1].Calls) != 1 || hist[1].Calls[0].Status != "ok" {
		t.Errorf("audit after approve: %+v", hist[1].Calls)
	}
}

func TestApprovalDenySkipsTool(t *testing.T) {
	risky := &fakeTool{name: "code_run", content: "never"}
	ac := startApprovalFlow(t, risky)
	defer ac.resp.Body.Close()

	if r := ac.a.do(http.MethodPost, "/api/v1/generations/"+ac.genID+"/approvals",
		map[string]any{"approval_id": ac.approvalID, "approve": false}); r.StatusCode != 204 {
		t.Fatalf("deny: %d %s", r.StatusCode, dumpBody(r))
	}
	es := drainSSE(t, ac.rd)
	if risky.callCount() != 0 {
		t.Error("denied tool executed anyway")
	}
	if es.data["tool_end"]["status"] != "denied" {
		t.Errorf("tool_end = %v", es.data["tool_end"])
	}
	if es.done["status"] != "complete" {
		t.Errorf("loop must still finish the answer: done = %v", es.done)
	}
	// Round 2 tells the model about the denial.
	b2, _ := json.Marshal(ac.script.roundBody(1))
	if !strings.Contains(string(b2), "denied") {
		t.Errorf("round-2 payload lacks denial notice:\n%s", b2)
	}
	// No tool-result row for a denied call; audit row says denied.
	hist, err := ac.a.db.ContextHistory(context.Background(), ac.conv.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range hist {
		if m.Role == "tool" {
			t.Fatalf("denied call persisted a tool row: %+v", m)
		}
	}
	if len(hist[1].Calls) != 1 || hist[1].Calls[0].Status != "denied" {
		t.Errorf("audit status after deny: %+v", hist[1].Calls)
	}
}

func TestApprovalTimeoutDenies(t *testing.T) {
	old := approvalTimeout
	approvalTimeout = 150 * time.Millisecond
	defer func() { approvalTimeout = old }()

	risky := &fakeTool{name: "code_run", content: "never"}
	ac := startApprovalFlow(t, risky)
	defer ac.resp.Body.Close()

	// No decision: the loop must time out into a denial on its own.
	es := drainSSE(t, ac.rd)
	if risky.callCount() != 0 {
		t.Error("timed-out tool executed")
	}
	if es.data["tool_end"]["status"] != "denied" || es.done["status"] != "complete" {
		t.Errorf("events=%v tool_end=%v done=%v", es.events, es.data["tool_end"], es.done)
	}
	hist, err := ac.a.db.ContextHistory(context.Background(), ac.conv.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist[1].Calls) != 1 || hist[1].Calls[0].Status != "denied" {
		t.Errorf("audit after timeout: %+v", hist[1].Calls)
	}
}

func TestApprovalUnknownOrForeignIs404(t *testing.T) {
	risky := &fakeTool{name: "code_run", content: "x"}
	ac := startApprovalFlow(t, risky)
	defer ac.resp.Body.Close()

	// Wrong approval id ⇒ 404 (indistinguishable, §9.2).
	if r := ac.a.do(http.MethodPost, "/api/v1/generations/"+ac.genID+"/approvals",
		map[string]any{"approval_id": "01BOGUS", "approve": true}); r.StatusCode != 404 {
		t.Errorf("bogus approval id: %d, want 404", r.StatusCode)
	}
	// Unknown generation ⇒ 404.
	if r := ac.a.do(http.MethodPost, "/api/v1/generations/01NOPE/approvals",
		map[string]any{"approval_id": ac.approvalID, "approve": true}); r.StatusCode != 404 {
		t.Errorf("unknown generation: %d, want 404", r.StatusCode)
	}
	// Missing approval_id ⇒ 400.
	if r := ac.a.do(http.MethodPost, "/api/v1/generations/"+ac.genID+"/approvals",
		map[string]any{"approve": true}); r.StatusCode != 400 {
		t.Errorf("missing approval_id: %d, want 400", r.StatusCode)
	}
	// Resolve the real one so the stream can finish.
	ac.a.do(http.MethodPost, "/api/v1/generations/"+ac.genID+"/approvals",
		map[string]any{"approval_id": ac.approvalID, "approve": false})
	drainSSE(t, ac.rd)
}

// --- round cap, unknown tools, truncation -------------------------------------------------

func TestToolRoundCap(t *testing.T) {
	// The fake insists on calling tools every round; once the cap is
	// reached the server must advertise no tools and must not execute the
	// past-cap proposal.
	search := &fakeTool{name: "web_search", content: `{}`, summary: "s"}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 2, map[string]string{"web_search": "auto"})
	a, script := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "c1", "web_search", `{}`), finishChunk("tool_calls")},
		{toolCallChunk(0, "c2", "web_search", `{}`), finishChunk("tool_calls")},
		{toolCallChunk(0, "c3", "web_search", `{}`), finishChunk("tool_calls")}, // past cap: must not run
	})
	c := createConv(t, a, map[string]any{"title": "Cap"})
	resp := sendChat(t, a, c.ID, "go")
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))

	if es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}
	if script.roundCount() != 3 {
		t.Fatalf("rounds = %d, want 3 (2 tool rounds + forced final)", script.roundCount())
	}
	if _, ok := script.roundBody(2)["tools"]; ok {
		t.Error("past-cap round still advertises tools")
	}
	if _, ok := script.roundBody(0)["tools"]; !ok {
		t.Error("first round must advertise tools")
	}
	if search.callCount() != 2 {
		t.Errorf("tool ran %d times, want 2 (c1+c2, not c3)", search.callCount())
	}
}

func TestUnknownToolReportedToModel(t *testing.T) {
	reg := registryFor(map[string]string{})
	cfg := toolChatCfg(t, 4, map[string]string{})
	a, script := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "u1", "not_registered", `{}`), finishChunk("tool_calls")},
		{answerChunk("sorry"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "U"})
	resp := sendChat(t, a, c.ID, "call something weird")
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))

	if es.data["tool_end"]["status"] != "error" {
		t.Errorf("tool_end = %v, want error", es.data["tool_end"])
	}
	if es.done["status"] != "complete" {
		t.Errorf("loop must recover: done = %v", es.done)
	}
	b2, _ := json.Marshal(script.roundBody(1))
	if !strings.Contains(string(b2), "unknown tool") {
		t.Errorf("model never told about unknown tool:\n%s", b2)
	}
}

func TestToolResultTruncatedForModelAndStore(t *testing.T) {
	huge := strings.Repeat("ü", 20_000) // runes: exercise the rune-based budget
	search := &fakeTool{name: "web_search", content: huge, summary: "big"}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
	a, script := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "t1", "web_search", `{}`), finishChunk("tool_calls")},
		{answerChunk("ok"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Big"})
	resp := sendChat(t, a, c.ID, "fetch big")
	defer resp.Body.Close()
	drainSSE(t, newSSEReader(t, resp))

	hist, err := a.db.ContextHistory(context.Background(), c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	var toolRow *store.Message
	for i := range hist {
		if hist[i].Role == "tool" {
			toolRow = &hist[i]
		}
	}
	if toolRow == nil {
		t.Fatal("no tool row persisted")
	}
	if n := len([]rune(toolRow.Content)); n > toolResultBudgetRunes+100 {
		t.Errorf("tool row %d runes, budget %d", n, toolResultBudgetRunes)
	}
	if !strings.Contains(toolRow.Content, "elided") {
		t.Error("tool row lacks elision marker")
	}
	// The model sees exactly the stored (truncated) content.
	b2, _ := json.Marshal(script.roundBody(1))
	if !strings.Contains(string(b2), "elided") {
		t.Error("replayed content not truncated")
	}
}

// payloadRole extracts the messages array entries with a given role from a
// captured upstream request body.
func payloadRole(t *testing.T, body map[string]any, role string) []map[string]any {
	t.Helper()
	if body == nil {
		t.Fatal("upstream request was never made")
	}
	msgs, _ := body["messages"].([]any)
	var out []map[string]any
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == role {
			out = append(out, mm)
		}
	}
	return out
}

// --- history replay rules (unit) ---------------------------------------------------------

func TestHistoryToLLMPairing(t *testing.T) {
	hist := []store.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "", Calls: []store.ToolCall{
			{LLMCallID: "a1", Tool: "web_search", Arguments: "{}"},
			{LLMCallID: "a2", Tool: "web_search", Arguments: "{}"}, // denied: never answered
		}},
		{Role: "tool", ToolCallID: "a1", Content: `{"r":1}`},
		{Role: "tool", ToolCallID: "orphan", Content: "crash leftover"},
		{Role: "assistant", Content: "done"},
	}
	s := &Server{cfg: testCfg()} // history here carries no attachments; the
	// userID only roots their manifests
	out := s.historyToLLM(hist, true, "")
	// a1 survives; the unanswered a2 and the orphan tool row are dropped.
	if len(out) != 4 {
		t.Fatalf("out = %+v", out)
	}
	if len(out[1].ToolCalls) != 1 || out[1].ToolCalls[0].ID != "a1" {
		t.Errorf("assistant replay = %+v", out[1].ToolCalls)
	}
	if out[2].Role != "tool" || out[2].ToolCallID != "a1" {
		t.Errorf("tool row = %+v", out[2])
	}
	// Tools off: everything protocol-shaped disappears, but real assistant
	// text survives; the empty call-only row drops out entirely.
	plain := s.historyToLLM(hist, false, "")
	for _, m := range plain {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Errorf("tools-off replay leaked protocol row: %+v", m)
		}
	}
	if len(plain) != 2 || plain[0].Content != "q1" || plain[1].Content != "done" {
		t.Fatalf("plain = %+v, want [q1, done]", plain)
	}
}
