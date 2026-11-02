package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/store"
)

// thinkingCfg prepares gufo/qwen3_8 using the repository catalogues.
func thinkingCfg(t *testing.T, endpoint string) *config.Config {
	cfg := chatCfg(t, endpoint)
	cfg.LLM.Models = []config.LLMModel{
		{ID: "m1", Default: true, ThinkingProfile: "qwen3_8"},
		{ID: "m2", ThinkingProfile: "qwen3_8"},
	}
	cfg.LLM.EndpointType = "gufo"
	cfg.LLM.EndpointsFile = filepath.Join("..", "..", "configs", "endpoints.yaml")
	cfg.LLM.ProfilesFile = filepath.Join("..", "..", "configs", "thinking-profiles.yaml")
	cfg.Tools.Agent.MaxToolRounds = 4
	return cfg
}

// recLLM records every streaming chat request body (the non-streaming
// titler is ignored) and serves scripted replies, one list per round.
type recLLM struct {
	mu       sync.Mutex
	bodies   []map[string]any
	replies  [][]string
	n        int
	release  chan struct{} // round 1 waits here when set
	released bool
}

func (l *recLLM) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	if streaming, _ := body["stream"].(bool); !streaming {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"Title"}}]}`))
		return
	}
	l.mu.Lock()
	idx := l.n
	l.n++
	l.bodies = append(l.bodies, body)
	first := idx == 0 && l.release != nil
	chunks := l.replies[len(l.replies)-1]
	if idx < len(l.replies) {
		chunks = l.replies[idx]
	}
	l.mu.Unlock()

	if first {
		<-l.release
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		sseUp(w, c)
	}
	sseUp(w, "[DONE]")
}

func (l *recLLM) streamBodies() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.bodies...)
}

// waitFor polls cond until true within d, failing the test otherwise.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// --- meta and conversation projection ------------------------------------------

func TestThinkingMetaAndDefaultProjection(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("ok"))
	a := signedInChat(t, thinkingCfg(t, fake.URL))

	var meta struct {
		Thinking *struct {
			ChoicesByModel map[string][]struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"choices_by_model"`
		} `json:"thinking"`
	}
	getJSON(t, a, "/api/v1/client-config", &meta)
	if meta.Thinking == nil {
		t.Fatal("meta.thinking = null with a visible profile")
	}
	// Every picker model gets its own list; the SPA reads the row
	// matching the conversation's active model.
	if len(meta.Thinking.ChoicesByModel) != 2 {
		t.Fatalf("choices_by_model = %v, want a row per picker model", meta.Thinking.ChoicesByModel)
	}
	var got []string
	for _, c := range meta.Thinking.ChoicesByModel["m1"] {
		got = append(got, c.ID)
	}
	want := []string{"default", "off", "low", "medium", "high"}
	if len(got) != len(want) {
		t.Fatalf("choices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("choices = %v, want %v", got, want)
		}
	}
	// m2 names the same profile, so its row matches — spelled per model,
	// not shared by reference assumption.
	var got2 []string
	for _, c := range meta.Thinking.ChoicesByModel["m2"] {
		got2 = append(got2, c.ID)
	}
	if strings.Join(got2, ",") != strings.Join(got, ",") {
		t.Errorf("m2 row = %v, want same as m1 %v", got2, got)
	}

	// New chats project Default.
	c := createConv(t, a, map[string]any{"title": "T"})
	var proj struct {
		Thinking string `json:"thinking"`
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj.Thinking != "default" {
		t.Errorf("new conversation thinking = %q, want default", proj.Thinking)
	}

	// Legacy config (no fields): meta.thinking is null.
	a2 := signedInChat(t, chatCfg(t, fake.URL))
	var meta2 struct {
		Thinking any `json:"thinking"`
	}
	getJSON(t, a2, "/api/v1/client-config", &meta2)
	if meta2.Thinking != nil {
		t.Errorf("legacy meta.thinking = %v, want null", meta2.Thinking)
	}
}

// PATCH stores a valid choice, clears both columns for Default, and
// rejects anything outside the effective list — before the network.
func TestThinkingPatch(t *testing.T) {
	var upstream atomic.Int32
	fake := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		scriptReply("ok")(w, r)
	})
	a := signedInChat(t, thinkingCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "T"})

	patch := func(body map[string]any) (int, string) {
		resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID, body)
		code := resp.StatusCode
		var proj struct {
			Thinking string `json:"thinking"`
		}
		if code == 200 {
			if err := json.NewDecoder(resp.Body).Decode(&proj); err != nil {
				t.Fatal(err)
			}
		}
		resp.Body.Close()
		return code, proj.Thinking
	}

	if code, th := patch(map[string]any{"thinking": "high"}); code != 200 || th != "high" {
		t.Fatalf("patch high = %d/%q", code, th)
	}
	if row := thinkingRow(t, a, c.ID); row.Thinking != "high" || row.ThinkingCtx == "" {
		t.Errorf("stored = %q/%q, want high with a fingerprint", row.Thinking, row.ThinkingCtx)
	}
	// Unknown ID and reserved-looking IDs are refused.
	if code, _ := patch(map[string]any{"thinking": "turbo"}); code != 400 {
		t.Errorf("patch turbo = %d, want 400", code)
	}
	if code, _ := patch(map[string]any{"thinking": "default"}); code != 200 {
		t.Errorf(`patch "default" = %d, want 200 (stores as '')`, code)
	}
	row := thinkingRow(t, a, c.ID)
	if row.Thinking != "" || row.ThinkingCtx != "" {
		t.Errorf("Default left stored %q/%q, want empty", row.Thinking, row.ThinkingCtx)
	}

	// A selector-less deployment rejects every non-Default choice.
	a2 := signedInChat(t, chatCfg(t, fake.URL))
	c2 := createConv(t, a2, map[string]any{"title": "T2"})
	if resp := a2.do(http.MethodPatch, "/api/v1/conversations/"+c2.ID,
		map[string]any{"thinking": "high"}); resp.StatusCode != 400 {
		t.Errorf("hidden-selector patch = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Validation happens before anything else: the fake endpoint was
	// never contacted by any of the above.
	if upstream.Load() != 0 {
		t.Errorf("upstream requests = %d, want 0", upstream.Load())
	}
}

// thinkingRow reads the stored columns through the store for the
// session's own conversation.
func thinkingRow(t *testing.T, a *authTest, convID string) store.Conversation {
	t.Helper()
	row, err := a.db.ConversationForUser(context.Background(), ownerOf(t, a).ID, convID)
	if err != nil {
		t.Fatal(err)
	}
	return *row
}

// The fingerprint ties the choice to its context: repointing the
// conversation at another model resets the restored choice visibly to
// Default; returning to the original model revives it.
func TestThinkingContextFingerprintReset(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("ok"))
	a := signedInChat(t, thinkingCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "T"})

	if resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"thinking": "high"}); resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, dumpBody(resp))
	} else {
		resp.Body.Close()
	}

	var proj struct {
		Thinking string `json:"thinking"`
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj.Thinking != "high" {
		t.Fatalf("thinking = %q before model change", proj.Thinking)
	}

	if resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"model": "m2"}); resp.StatusCode != 200 {
		t.Fatalf("model patch: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj.Thinking != "default" {
		t.Errorf("after model switch thinking = %q, want default", proj.Thinking)
	}

	// Back to the original model: the saved choice applies again.
	if resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"model": "m1"}); resp.StatusCode != 200 {
		t.Fatalf("model patch back: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj.Thinking != "high" {
		t.Errorf("after model switch back thinking = %q, want high", proj.Thinking)
	}
}

// --- the fragment on the wire ------------------------------------------------------

// fragmentOf returns the request body's thinking fields as a compact JSON
// string ("{}" when there are none).
func fragmentOf(t *testing.T, body map[string]any) string {
	t.Helper()
	out := map[string]any{}
	for _, k := range []string{"chat_template_kwargs", "reasoning_effort",
		"reasoning_budget_tokens", "thinking_token_budget", "reasoning"} {
		if v, found := body[k]; found {
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sendChatWith(t *testing.T, a *authTest, convID, content string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"content": content}
	for k, v := range extra {
		body[k] = v
	}
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body)
	if resp.StatusCode != 200 {
		t.Fatalf("chat send: %d %s", resp.StatusCode, dumpBody(resp))
	}
	rd := newSSEReader(t, resp)
	drainSSE(t, rd)
}

// An invalid thinking choice on a send is a plain 400 before anything is
// persisted or streamed; a valid one rides the request and persists.
func TestChatSendThinkingParam(t *testing.T) {
	rec := &recLLM{replies: [][]string{{answerChunk("ok"), finishChunk("stop")}}}
	fake := newFakeLLM(t, rec.handler)
	a := signedInChat(t, thinkingCfg(t, fake.URL))
	c := createConv(t, a, map[string]any{"title": "T"})

	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages",
		map[string]any{"content": "hi", "thinking": "turbo"})
	if resp.StatusCode != 400 {
		t.Fatalf("send thinking=turbo: %d %s", resp.StatusCode, dumpBody(resp))
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if errBody.Error.Code != "invalid_thinking_choice" {
		t.Errorf("error code = %q, want invalid_thinking_choice", errBody.Error.Code)
	}
	if n := len(rec.streamBodies()); n != 0 {
		t.Errorf("upstream rounds = %d, want 0", n)
	}

	// The rejected send persisted nothing.
	msgs, _, err := a.db.ListMessages(context.Background(), ownerOf(t, a).ID, c.ID, "", 10)
	if err != nil || len(msgs) != 0 {
		t.Errorf("stored messages after rejection = %d (%v), want 0", len(msgs), err)
	}

	// A valid choice: the request carries the exact Gufo fragment and the
	// conversation remembers the pick.
	sendChatWith(t, a, c.ID, "hi", map[string]any{"thinking": "high"})
	bodies := rec.streamBodies()
	if len(bodies) != 1 {
		t.Fatalf("upstream rounds = %d, want 1", len(bodies))
	}
	const high = `{"chat_template_kwargs":{"enable_thinking":true},"reasoning_effort":"xhigh"}`
	if got := fragmentOf(t, bodies[0]); got != high {
		t.Errorf("request fragment = %s, want %s", got, high)
	}
	var proj struct {
		Thinking string `json:"thinking"`
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj.Thinking != "high" {
		t.Errorf("stored projection = %q, want high", proj.Thinking)
	}

	// Off after High: only the Off fragment survives — a fresh body never
	// inherits the previous turn's effort.
	sendChatWith(t, a, c.ID, "off please", map[string]any{"thinking": "off"})
	bodies = rec.streamBodies()
	if got := fragmentOf(t, bodies[1]); got != `{"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("Off request fragment = %s", got)
	}

	// Back to Default: no thinking field of any kind.
	sendChatWith(t, a, c.ID, "default now", map[string]any{"thinking": "default"})
	bodies = rec.streamBodies()
	if got := fragmentOf(t, bodies[2]); got != "{}" {
		t.Errorf("Default request fragment = %s, want {}", got)
	}
}

// Mid-flight selection changes must not reach the in-flight generation's
// tool continuations: round 1 runs High, the selector moves to Off during
// it, round 2 still speaks High — and only the NEXT generation sends Off.
func TestThinkingSnapshotSurvivesToolRounds(t *testing.T) {
	release, done := gate()
	rec := &recLLM{
		release: release,
		replies: [][]string{
			{toolCallChunk(0, "call-1", "no_such_tool", `{"x":1}`), finishChunk("tool_calls")},
			{answerChunk("after"), finishChunk("stop")},
		},
	}
	fake := newFakeLLM(t, rec.handler)
	cfg := thinkingCfg(t, fake.URL)
	cfg.Tools.Agent.MaxToolRounds = 4

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := newPreparedServer(t, cfg, db, logger, "test", nil, nil)
	s.chat.Tools = registryFor(map[string]string{}) // nothing registered: the call is unknown
	ts := httptest.NewServer(s.Handler())
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("snap@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	defer done()

	c := createConv(t, a, map[string]any{"title": "Snap"})
	if resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"thinking": "high"}); resp.StatusCode != 200 {
		t.Fatalf("patch high: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	sendResp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages",
		map[string]any{"content": "go"})
	// Wait until round 1 is parked at the upstream gate, then move the
	// selector mid-generation.
	waitFor(t, 5*time.Second, func() bool { return len(rec.streamBodies()) >= 1 })
	if resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"thinking": "off"}); resp.StatusCode != 200 {
		t.Fatalf("patch off mid-flight: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	done() // release round 1

	rd := newSSEReader(t, sendResp)
	drainSSE(t, rd)

	bodies := rec.streamBodies()
	if len(bodies) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(bodies))
	}
	const high = `{"chat_template_kwargs":{"enable_thinking":true},"reasoning_effort":"xhigh"}`
	if got := fragmentOf(t, bodies[0]); got != high {
		t.Errorf("round 1 fragment = %s, want High", got)
	}
	if got := fragmentOf(t, bodies[1]); got != high {
		t.Errorf("round 2 fragment = %s, want High (snapshot, not the live selector)", got)
	}

	// The NEXT generation reflects the moved selector.
	sendChatWith(t, a, c.ID, "again", nil)
	bodies = rec.streamBodies()
	if got := fragmentOf(t, bodies[2]); got != `{"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("next generation fragment = %s, want Off", got)
	}
}

// Choices are conversation state under the owner: another user can
// neither see nor touch them.
func TestThinkingPerUserIsolation(t *testing.T) {
	rec := &recLLM{replies: [][]string{{answerChunk("ok"), finishChunk("stop")}}}
	fake := newFakeLLM(t, rec.handler)
	cfg := thinkingCfg(t, fake.URL)
	cfg.LLM.Endpoint = fake.URL

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := newPreparedServer(t, cfg, db, logger, "test", nil, nil)
	ts := httptest.NewServer(s.Handler())

	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("one@b.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register one: %d", resp.StatusCode)
	}
	b := newAuthTestOn(t, ts, db)
	if err := db.CreateInvite(context.Background(), "gk-code-2", time.Now()); err != nil {
		t.Fatal(err)
	}
	if resp := b.do(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"email": "two@b.io", "name": "Second User", "password": "uncommon-password-8",
		"remember_me": false, "invite": "gk-code-2",
	}); resp.StatusCode != 201 {
		t.Fatalf("register two: %d %s", resp.StatusCode, dumpBody(resp))
	}

	c := createConv(t, a, map[string]any{"title": "Mine"})
	if resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"thinking": "medium"}); resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// The stranger cannot read or rewrite it — thinking included.
	if resp := b.do(http.MethodGet, "/api/v1/conversations/"+c.ID, nil); resp.StatusCode != 404 {
		t.Errorf("stranger GET = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := b.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"thinking": "off"}); resp.StatusCode != 404 {
		t.Errorf("stranger PATCH = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// And its own chats still start at Default.
	c2 := createConv(t, b, map[string]any{"title": "Theirs"})
	var proj struct {
		Thinking string `json:"thinking"`
	}
	getJSON(t, b, "/api/v1/conversations/"+c2.ID, &proj)
	if proj.Thinking != "default" {
		t.Errorf("stranger's new chat = %q, want default", proj.Thinking)
	}
	// The owner's choice survived untouched.
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj.Thinking != "medium" {
		t.Errorf("owner's choice = %q, want medium", proj.Thinking)
	}
}
