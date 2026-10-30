package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mullsmith/internal/store"
)

// titleFake answers streaming chat requests with scriptReply-style content
// and non-streaming title quests per titleFn (HTTP status, completion text).
// titleHits counts the quests so tests can assert none were made.
func titleFake(reply []string, titleFn func() (int, string), hits *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			for _, p := range reply {
				sseUp(w, deltaJSON(map[string]any{"content": p}))
			}
			sseUp(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			sseUp(w, "[DONE]")
			return
		}
		// Title quest: must carry the titling system prompt and thinking-off.
		hits.Add(1)
		msgs, _ := body["messages"].([]any)
		okSys, sawUser := false, false
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			s, _ := mm["content"].(string)
			if mm["role"] == "system" && strings.Contains(s, "titles for chat conversations") {
				okSys = true
			}
			if mm["role"] == "user" && s != "" {
				sawUser = true
			}
		}
		if !okSys || !sawUser {
			http.Error(w, "unexpected non-stream call", 500)
			return
		}
		code, text := titleFn()
		if code != 200 {
			http.Error(w, "title unavailable", code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": text}}},
		})
	}
}

// collectUntilDone drains SSE, returning the titles seen in "title" events.
func collectUntilDone(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	rd := newSSEReader(t, resp)
	var titles []map[string]any
	for {
		ev, d := rd.next()
		switch ev {
		case "title":
			titles = append(titles, d)
		case "done":
			return titles
		case "start", "token":
		default:
			t.Fatalf("unexpected SSE event %q", ev)
		}
	}
}

func convTitle(t *testing.T, a *authTest, id string) string {
	t.Helper()
	var c convResp
	getJSON(t, a, "/api/v1/conversations/"+id, &c)
	return c.Title
}

func TestAutoTitleFirstMessage(t *testing.T) {
	var hits atomic.Int64
	fake := newFakeLLM(t, titleFake([]string{"Answ", "er."},
		func() (int, string) { return 200, " “Quantum Entanglement Basics”\nleftovers" }, &hits))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, nil) // unnamed → "New chat"

	resp := sendChat(t, a, c.ID, "explain quantum entanglement like I am five")
	defer resp.Body.Close()
	titles := collectUntilDone(t, resp)

	if len(titles) != 1 {
		t.Fatalf("title events = %v, want exactly one", titles)
	}
	// Quotes stripped, only the first line kept.
	if titles[0]["title"] != "Quantum Entanglement Basics" {
		t.Errorf("title event = %v", titles[0])
	}
	if titles[0]["conversation_id"] != c.ID {
		t.Errorf("conversation_id = %v", titles[0]["conversation_id"])
	}
	if got := convTitle(t, a, c.ID); got != "Quantum Entanglement Basics" {
		t.Errorf("stored title = %q", got)
	}
}

func TestAutoTitleOnlyUntilNamed(t *testing.T) {
	var hits atomic.Int64
	fake := newFakeLLM(t, titleFake([]string{"ok"},
		func() (int, string) { return 200, "Topic Name" }, &hits))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, nil)

	r1 := sendChat(t, a, c.ID, "first question")
	titles := collectUntilDone(t, r1)
	r1.Body.Close()
	if len(titles) != 1 {
		t.Fatalf("first send titles = %v", titles)
	}

	r2 := sendChat(t, a, c.ID, "second question")
	titles2 := collectUntilDone(t, r2)
	r2.Body.Close()
	if len(titles2) != 0 {
		t.Errorf("named conversation retitled: %v", titles2)
	}
	if got := convTitle(t, a, c.ID); got != "Topic Name" {
		t.Errorf("title changed to %q", got)
	}
	if hits.Load() != 1 {
		t.Errorf("title quests = %d, want 1", hits.Load())
	}
}

func TestAutoTitleNeverOverridesUserRename(t *testing.T) {
	// Genuine race: the quest is in flight (server saw "New chat") when the
	// user renames. The guarded UPDATE must skip the row, and no title
	// event may go out.
	ch, release := gate()
	defer release()
	var hits atomic.Int64
	fake := newFakeLLM(t, titleFake([]string{"ok"}, func() (int, string) {
		<-ch // hold the quest open until the test has renamed the row
		return 200, "Machine Choice"
	}, &hits))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, nil)

	resp := sendChat(t, a, c.ID, "hello")
	defer resp.Body.Close()
	// The quest claims the endpoint BEFORE the reply starts (spec §12):
	// wait until it is genuinely in flight, then rename underneath it.
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("expected the title quest to reach the fake upstream before the reply")
	}
	if resp2 := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"title": "My Precise Name"}); resp2.StatusCode != 200 {
		t.Fatalf("rename: %d %s", resp2.StatusCode, dumpBody(resp2))
	}
	release() // the held quest answers onto the renamed row: the guarded write skips it

	titles := collectUntilDone(t, resp)
	if len(titles) != 0 {
		t.Errorf("title event leaked over a rename: %v", titles)
	}
	if got := convTitle(t, a, c.ID); got != "My Precise Name" {
		t.Errorf("user rename clobbered: %q", got)
	}
}

func TestAutoTitleFailureKeepsPlaceholderAndRetries(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var hits atomic.Int64
	fake := newFakeLLM(t, titleFake([]string{"reply"}, func() (int, string) {
		if fail.Load() {
			return 500, ""
		}
		return 200, "Recovered Topic"
	}, &hits))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, nil)

	r1 := sendChat(t, a, c.ID, "what makes bread rise")
	titles := collectUntilDone(t, r1)
	r1.Body.Close()
	if len(titles) != 0 {
		t.Fatalf("failed quest emitted %v", titles)
	}
	if got := convTitle(t, a, c.ID); got != store.DefaultTitle {
		t.Errorf("title = %q, want placeholder", got)
	}

	fail.Store(false)
	r2 := sendChat(t, a, c.ID, "and how does sourdough differ")
	titles2 := collectUntilDone(t, r2)
	r2.Body.Close()
	if len(titles2) != 1 || titles2[0]["title"] != "Recovered Topic" {
		t.Fatalf("retry titles = %v, want Recovered Topic", titles2)
	}
}

func TestRegenerateNeverTitles(t *testing.T) {
	var hits atomic.Int64
	fake := newFakeLLM(t, titleFake([]string{"v1"},
		func() (int, string) { return 200, "Should Not Happen" }, &hits))
	a := signedInChat(t, chatCfg(t, fake.URL))
	c := createConv(t, a, nil)

	r1 := sendChat(t, a, c.ID, "original question")
	collectUntilDone(t, r1)
	r1.Body.Close()
	// The first send titles it; reset the name so a stray title quest on
	// the regenerate path would be observable in the store too.
	if resp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID,
		map[string]any{"title": store.DefaultTitle}); resp.StatusCode != 200 {
		t.Fatalf("rename back: %d", resp.StatusCode)
	}
	before := hits.Load()

	var page msgPage
	getJSON(t, a, "/api/v1/conversations/"+c.ID+"/messages", &page)
	var lastAssistant string
	for _, it := range page.Items {
		if it.Role == "assistant" {
			lastAssistant = it.ID
		}
	}
	if lastAssistant == "" {
		t.Fatal("no assistant message to regenerate")
	}
	r2 := a.do(http.MethodPost, "/api/v1/messages/"+lastAssistant+"/regenerate", nil)
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("regenerate: %d %s", r2.StatusCode, dumpBody(r2))
	}
	collectUntilDone(t, r2)

	if hits.Load() != before {
		t.Errorf("regenerate fired %d title quests, want 0", hits.Load()-before)
	}
}
