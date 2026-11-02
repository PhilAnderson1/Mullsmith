package server

// The recorded transcript defines the streaming/history contract. This runs the real
// chat handler against the scripted upstream for a two-round tool exchange
// and pins what the wire and the history API agree on: the events'
// message_ids, the done.rounds entries and the assistant rows a reload
// returns are the SAME ids, in the same order. The JS suite replays the
// recorded frames through the real client loop and asserts the same
// equality, so "live view == reloaded history" is checked in Node, on
// recorded reality, not hand-written frames.
//
// Regenerate after an intentional wire change with:
//
//	UPDATE_TRANSCRIPT=1 go test ./internal/server -run TestTranscriptFixture
//
// and review the diff — that diff is the compatibility statement.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const transcriptFixtureRel = "testdata/two-round-transcript.json"

// transcriptFixture is the recorded exchange: raw SSE bytes, the event-name
// sequence, and the exact /messages page the same conversation returns.
type transcriptFixture struct {
	Events     []string           `json:"events"`
	Frames     string             `json:"frames"`
	Messages   messageHistoryPage `json:"messages"`
	Generation string             `json:"generation_id"`
	Rounds     []map[string]any   `json:"rounds"`
}

func transcriptFixturePath() string { return filepath.Join(".", transcriptFixtureRel) }

// recordTranscriptExchange drives the exchange and captures everything verbatim.
func recordTranscriptExchange(t *testing.T) transcriptFixture {
	t.Helper()
	search := &fakeTool{name: "web_search", content: `{"results":[]}`, summary: "Searched — 3 result(s)"}
	reg := registryFor(map[string]string{"web_search": "auto"}, search)
	cfg := toolChatCfg(t, 4, map[string]string{"web_search": "auto"})
	a, _ := newToolServer(t, cfg, reg, [][]string{
		{toolCallChunk(0, "call_f1", "web_search", `{"query":"kyoto"}`), finishChunk("tool_calls")},
		{answerChunk("Kyoto "), answerChunk("in autumn."), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Fixture"})
	resp := sendChat(t, a, c.ID, "search kyoto")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Parse the recorded bytes for the assertions (ids, rounds), keeping
	// the bytes themselves for the JS replay.
	var evs []string
	var done map[string]any
	var gen string
	for _, frame := range strings.Split(string(raw), "\n\n") {
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		if name == "" || name == "title" {
			continue
		}
		evs = append(evs, name)
		var d map[string]any
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			t.Fatalf("fixture frame %.80q: %v", frame, err)
		}
		if name == "start" {
			gen, _ = d["generation_id"].(string)
		}
		if name == "done" {
			done = d
		}
	}
	if gen == "" || done == nil {
		t.Fatal("recorded transcript has no start/done")
	}
	rounds := roundsOf(t, done)
	var page messageHistoryPage
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	return transcriptFixture{Events: evs, Frames: string(raw), Messages: page, Generation: gen, Rounds: rounds}
}

func TestTranscriptFixture(t *testing.T) {
	got := recordTranscriptExchange(t)
	if os.Getenv("UPDATE_TRANSCRIPT") == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(transcriptFixturePath()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(transcriptFixturePath(), b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("transcript fixture rewritten")
	}
	want, err := os.ReadFile(transcriptFixturePath())
	if err != nil {
		t.Fatalf("fixture missing: regenerate with UPDATE_TRANSCRIPT=1 go test ./internal/server -run TestTranscriptFixture (%v)", err)
	}
	var fx transcriptFixture
	if err := json.Unmarshal(want, &fx); err != nil {
		t.Fatal(err)
	}

	// The event SEQUENCE is the wire contract: a new or renamed event
	// must be a deliberate act (regenerate + review), never a side effect.
	if strings.Join(fx.Events, ",") != strings.Join(got.Events, ",") {
		t.Errorf("event sequence changed:\n fixture %v\n current  %v", fx.Events, got.Events)
	}

	// The fixture must be self-consistent, because the JS replay leans on
	// it: every round row named by start/round/done.rounds exists in the
	// recorded page with the same generation id.
	byID := map[string]string{} // id -> generation_id of assistant rows
	for _, m := range fx.Messages.Items {
		if m.Role == "assistant" {
			byID[m.ID] = m.GenerationID
		}
	}
	var startID, roundID string
	for _, frame := range strings.Split(fx.Frames, "\n\n") {
		if strings.Contains(frame, "event: start") {
			startID = transcriptFrameField(t, frame, "message_id")
		}
		if strings.Contains(frame, "event: round") {
			roundID = transcriptFrameField(t, frame, "message_id")
		}
	}
	if startID == "" || roundID == "" || startID == roundID {
		t.Fatalf("fixture events must name two rows, got %q / %q", startID, roundID)
	}
	if byID[startID] != fx.Generation || byID[roundID] != fx.Generation {
		t.Errorf("fixture rows %v do not carry the recorded generation %q", byID, fx.Generation)
	}
	if len(fx.Rounds) != 2 || fx.Rounds[0]["message_id"] != startID || fx.Rounds[1]["message_id"] != roundID {
		t.Errorf("fixture done.rounds %v must name the two rows in order", fx.Rounds)
	}
}

// transcriptFrameField reads one top-level string field out of a frame's data line.
func transcriptFrameField(t *testing.T, frame, field string) string {
	t.Helper()
	_, data, _ := strings.Cut(frame, "data: ")
	var d map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &d); err != nil {
		t.Fatalf("frame %.80q: %v", frame, err)
	}
	s, _ := d[field].(string)
	return s
}
