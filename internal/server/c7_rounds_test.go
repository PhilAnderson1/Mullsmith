package server

// C7 — identified rounds and a shared live/history projection, server half.
//
// The defect this pins was observable without any browser: the loop inserts
// one assistant row per model round but announced only the first, so the
// client accumulated a whole multi-round reply into one bubble and stamped it
// with the FINAL row's id. Reload then showed a different structure —
// different bubble boundaries, cards rebuilt by re-parsing middle-elided
// model-facing text, and terminal reasons that existed only in the live
// stream.
//
// Every test here compares what the STREAM says against what the HISTORY API
// says about the same rows: the client renders one projection of those rows
// for both views, so agreement between these two is the contract.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// c7Page mirrors the history projection these tests compare events against.
type c7Page struct {
	Items []struct {
		ID           string `json:"id"`
		Role         string `json:"role"`
		Content      string `json:"content"`
		Status       string `json:"status"`
		StatusNote   string `json:"status_note"`
		GenerationID string `json:"generation_id"`
		ToolCalls    []struct {
			ID      string `json:"id"`
			Tool    string `json:"tool"`
			Status  string `json:"status"`
			Display string `json:"display"`
		} `json:"tool_calls"`
	} `json:"items"`
}

// displayOf parses one stored card payload; an empty payload is nil.
func displayOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	if raw == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("stored display is not valid JSON (%v): %.120q", err, raw)
	}
	return m
}

// roundsOf reads done.rounds as a list of objects.
func roundsOf(t *testing.T, done map[string]any) []map[string]any {
	t.Helper()
	raw, ok := done["rounds"].([]any)
	if !ok {
		t.Fatalf("done.rounds = %#v, want a list", done["rounds"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("done.rounds entry %#v is not an object", r)
		}
		out = append(out, m)
	}
	return out
}

// lastRoundID names the newest durable outcome; done has no duplicate row id.
func lastRoundID(t *testing.T, done map[string]any) string {
	t.Helper()
	rounds := roundsOf(t, done)
	if len(rounds) == 0 {
		t.Fatal("no durable round in terminal event")
	}
	id, _ := rounds[len(rounds)-1]["message_id"].(string)
	return id
}

// runExchange drives one scripted conversation with `web_search` on auto
// approval and returns the drained stream plus the rows it left behind.
func runExchange(t *testing.T, tool *fakeTool, reply [][]string,
	title, text string,
) (*eventStream, c7Page, *authTest, convResp) {
	t.Helper()
	reg := registryFor(map[string]string{"web_search": "auto"}, tool)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
	a, _ := newToolServer(t, cfg, reg, reply)
	c := createConv(t, a, map[string]any{"title": title})
	resp := sendChat(t, a, c.ID, text)
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))
	var page c7Page
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	return &es, page, a, c
}

func twoRoundExchange(t *testing.T, tool *fakeTool) (*eventStream, c7Page, *authTest, convResp) {
	t.Helper()
	return runExchange(t, tool, [][]string{
		{toolCallChunk(0, "call_1", "web_search", `{"query":"kyoto"}`), finishChunk("tool_calls")},
		{answerChunk("Kyoto is "), answerChunk("lovely."), finishChunk("stop")},
	}, "Rounds", "search kyoto")
}

// The events name the rows a reload returns: one bubble per stored round,
// cards attached to the round that proposed the call, and one generation id
// across the rows of the turn.
func TestRoundEventsNameTheRowsHistoryReturns(t *testing.T) {
	search := &fakeTool{name: "web_search", content: `{"results":[]}`, summary: "Searched — 3 result(s)"}
	es, page, _, _ := twoRoundExchange(t, search)

	startID, _ := es.data["start"]["message_id"].(string)
	roundID, _ := es.data["round"]["message_id"].(string)
	if startID == "" || roundID == "" || startID == roundID {
		t.Fatalf("start %q / round %q must name two distinct rows", startID, roundID)
	}
	if es.data["tool_start"]["message_id"] != startID || es.data["tool_end"]["message_id"] != startID {
		t.Errorf("tool events named %v/%v, want the proposing round %q",
			es.data["tool_start"]["message_id"], es.data["tool_end"]["message_id"], startID)
	}

	var ids, contents []string
	var genOf []string
	var callsOnFirst = -1
	toolRows := 0
	for _, it := range page.Items {
		switch it.Role {
		case "assistant":
			ids = append(ids, it.ID)
			contents = append(contents, it.Content)
			genOf = append(genOf, it.GenerationID)
			if len(ids) == 1 {
				callsOnFirst = len(it.ToolCalls)
			}
		case "tool":
			toolRows++
		}
	}
	if len(ids) != 2 || toolRows != 0 {
		t.Fatalf("history rows = %+v, want two assistant rounds and no model-only rows", page.Items)
	}
	if ids[0] != startID || ids[1] != roundID {
		t.Errorf("history rows are %q,%q, want the streamed %q,%q", ids[0], ids[1], startID, roundID)
	}
	// Prose lives on ITS round. Live, both rounds' tokens were concatenated
	// into one body; this split is what each bubble now renders.
	if strings.TrimSpace(contents[0]) != "" {
		t.Errorf("proposing round carries prose %q", contents[0])
	}
	if contents[1] != "Kyoto is lovely." {
		t.Errorf("answer round = %q", contents[1])
	}
	if callsOnFirst != 1 {
		t.Errorf("the call is not attached to the round that proposed it: %d call(s)", callsOnFirst)
	}
	if genOf[0] == "" || genOf[1] != genOf[0] {
		t.Errorf("rounds of one generation must share a durable id: %q / %q", genOf[0], genOf[1])
	}

	rounds := roundsOf(t, es.done)
	if len(rounds) != 2 {
		t.Fatalf("done.rounds = %v, want one entry per stored round", rounds)
	}
	for i, want := range []string{startID, roundID} {
		if rounds[i]["message_id"] != want {
			t.Errorf("done.rounds[%d].message_id = %v, want %q", i, rounds[i]["message_id"], want)
		}
		if rounds[i]["status"] != "complete" {
			t.Errorf("done.rounds[%d].status = %v, want complete", i, rounds[i]["status"])
		}
	}
	// The last outcome names the newest durable round.
	if lastRoundID(t, es.done) != roundID {
		t.Errorf("done.message_id = %v, want the newest durable row %q", lastRoundID(t, es.done), roundID)
	}
}

// The card payload is STORED, not re-derived: what tool_end carried live is
// what the history projection hands back, so one projector renders both —
// and eliding the model-facing text cannot eat it.
func TestToolCardPayloadSurvivesReload(t *testing.T) {
	results := []map[string]any{
		{"title": "Kyoto guide", "url": "https://example.org/kyoto", "snippet": strings.Repeat("s", 400)},
		{"title": "Trains", "url": "https://example.org/trains", "snippet": "JR lines"},
	}
	search := &fakeTool{
		name:    "web_search",
		summary: `Searched the web for "kyoto" — 2 result(s)`,
		// Model-facing content deliberately LONGER than the live payload.
		content: `{"results":[` + strings.Repeat(
			`{"url":"https://example.org/x","t":"`+strings.Repeat("x", 500)+`"},`, 40) + `{}]}`,
		data: results,
	}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
	a, _ := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "call_r", "web_search", `{"query":"kyoto"}`), finishChunk("tool_calls")},
		{answerChunk("done"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Payload"})
	resp := sendChat(t, a, c.ID, "search kyoto")
	defer resp.Body.Close()
	var liveData any
	var liveSummary string
	rd := newSSEReader(t, resp)
	for {
		ev, d := rd.next()
		if ev == "tool_end" {
			liveData, _ = d["data"]
			liveSummary, _ = d["summary"].(string)
			break
		}
		if ev == "done" || ev == "error" {
			break
		}
	}
	if liveData == nil {
		t.Fatal("no tool_end payload arrived live")
	}

	var page c7Page
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	var stored map[string]any
	for _, it := range page.Items {
		for _, tc := range it.ToolCalls {
			stored = displayOf(t, tc.Display)
		}
	}
	if stored == nil {
		t.Fatal("no display payload was stored")
	}
	if stored["summary"] != liveSummary {
		t.Errorf("stored summary %v, want the live one %q", stored["summary"], liveSummary)
	}
	got, _ := json.Marshal(stored["data"])
	want, _ := json.Marshal(liveData)
	if string(got) != string(want) {
		t.Errorf("stored card data differs from the live payload:\n got %.200s\nwant %.200s", got, want)
	}
}

// Over the display budget the DATA goes and the SUMMARY stays, with a flag.
// What this replaces is a JSON string cut in half, which the reload path
// could not parse at all — "no card beats a wrong card", and no card is
// exactly what it produced.
func TestOversizedCardPayloadDropsDataNotJSON(t *testing.T) {
	big := make([]map[string]any, 0, 4000)
	for i := 0; i < 4000; i++ {
		big = append(big, map[string]any{"title": fmt.Sprintf("Result %d", i),
			"url": fmt.Sprintf("https://example.org/%d", i)})
	}
	search := &fakeTool{name: "web_search", content: `{"ok":true}`,
		summary: "Searched — 4000 result(s)", data: big}
	es, page, _, _ := twoRoundExchange(t, search)
	if es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}
	var stored map[string]any
	for _, it := range page.Items {
		for _, tc := range it.ToolCalls {
			stored = displayOf(t, tc.Display) // must PARSE: that is the point
		}
	}
	if stored == nil {
		t.Fatal("no display payload stored")
	}
	if stored["truncated"] != true {
		t.Errorf("oversized payload stored without a truncated flag: %.120v", stored)
	}
	if stored["data"] != nil {
		t.Error("over-budget data was kept anyway")
	}
	if stored["summary"] != "Searched — 4000 result(s)" {
		t.Errorf("summary lost when the payload was trimmed: %v", stored["summary"])
	}
}

// A denial has no tool-result row and never reaches FinishToolCall, so its
// card text was live-only: a reload showed a bare "denied" label where the
// stream said why. The verdict is now part of the stored payload.
func TestDeniedCallCardIsDurable(t *testing.T) {
	risky := &fakeTool{name: "code_run", content: "never"}
	ac := startApprovalFlow(t, risky)
	defer ac.resp.Body.Close()
	if r := ac.a.do(http.MethodPost, "/api/v1/generations/"+ac.genID+"/approvals",
		map[string]any{"approval_id": ac.approvalID, "approve": false}); r.StatusCode != 204 {
		t.Fatalf("deny: %d %s", r.StatusCode, dumpBody(r))
	}
	var liveSummary string
	for {
		ev, d := ac.rd.next()
		if ev == "tool_end" && d["status"] == "denied" {
			liveSummary, _ = d["summary"].(string)
		}
		if ev == "done" || ev == "error" {
			break
		}
	}
	if liveSummary == "" {
		t.Fatal("live denial carried no card summary")
	}
	var page c7Page
	getJSON(t, ac.a, "/api/v1/conversations/"+ac.conv.ID+"/messages", &page)
	var stored map[string]any
	found := false
	for _, it := range page.Items {
		for _, tc := range it.ToolCalls {
			if tc.Status == "denied" {
				stored, found = displayOf(t, tc.Display), true
			}
		}
	}
	if !found {
		t.Fatalf("no denied call row in history: %+v", page.Items)
	}
	if stored["summary"] != liveSummary {
		t.Errorf("stored denial %v, want the live %q", stored["summary"], liveSummary)
	}
}

// The failure that had nowhere to live: the last round saved cleanly and
// THEN the loop died. `done` explained it; a reload used to show a plain
// complete answer with no trace of a stopped generation.
func TestStrandedTerminalReasonSurvivesReload(t *testing.T) {
	search := &fakeTool{name: "web_search", content: `{}`, summary: "ran"}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
	a, _ := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "call_q", "web_search", `{"query":"y"}`), finishChunk("tool_calls")},
		{answerChunk("never streamed"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Stranded"})
	// The tool-round placeholder insert fails AFTER the proposing round was
	// finalised `complete` — the exact shape of a failure with no row.
	if err := a.db.ExecForTest(`CREATE TRIGGER no_second_assistant BEFORE INSERT ON messages
		WHEN NEW.role='assistant' AND
			(SELECT COUNT(*) FROM messages
			 WHERE conversation_id = NEW.conversation_id AND role='assistant') >= 1
		BEGIN SELECT RAISE(ABORT, 'injected placeholder failure'); END`); err != nil {
		t.Fatal(err)
	}
	resp := sendChat(t, a, c.ID, "search y")
	defer resp.Body.Close()
	// drainSSE stops at the `error` frame (the storage failure is announced
	// there), and `done` follows it describing what IS durable — read it too.
	rd := newSSEReader(t, resp)
	var done map[string]any
	for {
		ev, d := rd.next()
		if ev == "done" || ev == "error" {
			if ev == "error" {
				continue
			}
			done = d
			break
		}
	}
	if done["status"] != "error" {
		t.Fatalf("done = %v, want the failure announced", done)
	}
	rounds := roundsOf(t, done)
	if len(rounds) != 1 {
		t.Fatalf("done.rounds = %v, want the one durable round", rounds)
	}
	note, _ := rounds[0]["note"].(string)
	if note == "" {
		t.Fatalf("the stranded round carries no note: %v", rounds[0])
	}
	var page c7Page
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	var lastNote string
	for _, it := range page.Items {
		if it.Role == "assistant" {
			lastNote = it.StatusNote
		}
	}
	if lastNote != note {
		t.Errorf("reload status_note = %q, want the note the stream carried %q", lastNote, note)
	}
}

// Regenerate retires the TURN. Retiring only the row the button named left
// the retired attempt's earlier rounds and tool answers in history — which
// historyToLLM replays, so "answer that again" started from the previous
// attempt's own tool work and its own earlier prose.
func TestRegenerateRetiresWholeTurn(t *testing.T) {
	search := &fakeTool{name: "web_search",
		content: `{"results":[{"url":"https://secret.example/old"}],"body":"` + strings.Repeat("o", 60) + `"}`,
		summary: "Searched"}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
	a, script := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "call_t", "web_search", `{"query":"kyoto"}`), finishChunk("tool_calls")},
		{answerChunk("first answer"), finishChunk("stop")},
		{answerChunk("fresh answer"), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Retire"})
	resp := sendChat(t, a, c.ID, "search kyoto")
	es := drainSSE(t, newSSEReader(t, resp))
	resp.Body.Close()
	if es.done["status"] != "complete" {
		t.Fatalf("first exchange: %v", es.done)
	}
	lastID := lastRoundID(t, es.done)

	rresp := a.do(http.MethodPost, "/api/v1/messages/"+lastID+"/regenerate", map[string]any{})
	if rresp.StatusCode != 200 {
		t.Fatalf("regenerate: %d %s", rresp.StatusCode, dumpBody(rresp))
	}
	retry := drainSSE(t, newSSEReader(t, rresp))
	rresp.Body.Close()
	if retry.done["status"] != "complete" {
		t.Fatalf("regenerate stream: %v", retry.done)
	}

	// One turn, one answer: the retired round rows AND the tool answer that
	// fed them are gone.
	var page c7Page
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	var assistant, toolRows int
	var content string
	for _, it := range page.Items {
		switch it.Role {
		case "assistant":
			assistant++
			content = it.Content
		case "tool":
			toolRows++
		}
	}
	if assistant != 1 || toolRows != 0 {
		t.Fatalf("after regenerate: %d assistant rows and %d tool rows, want 1 and 0 (%+v)",
			assistant, toolRows, page.Items)
	}
	if content != "fresh answer" {
		t.Errorf("surviving answer = %q", content)
	}

	// And the retry's own prompt quotes neither the retired prose nor the
	// tool result the retired attempt produced.
	if script.roundCount() != 3 {
		t.Fatalf("upstream rounds = %d, want 3", script.roundCount())
	}
	body, _ := json.Marshal(script.roundBody(2))
	sent := string(body)
	if strings.Contains(sent, "first answer") {
		t.Error("the retry prompt still quotes the retired answer's prose")
	}
	if strings.Contains(sent, "secret.example") {
		t.Error("the retry prompt still replays the retired attempt's tool result")
	}
	if !strings.Contains(sent, "search kyoto") {
		t.Error("the retry prompt lost the user's own turn")
	}
}
