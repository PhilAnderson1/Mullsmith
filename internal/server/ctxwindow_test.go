package server

import (
	"context"
	"io"
	"log/slog"
	"mullsmith/internal/chat"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/store"
)

// newWindowTest builds one Server with a canned windowProbe — installed
// after construction, so nothing is probed until a test refreshes — plus
// a signed-in session for conversation reads. entries override the model
// list when non-nil.
func newWindowTest(t *testing.T, probe func(context.Context) (map[string]int, bool), entries ...config.LLMModel) (*authTest, *Server, *store.DB) {
	t.Helper()
	cfg := chatCfg(t, "http://127.0.0.1:1/v1") // endpoint unused: the probe is canned
	if len(entries) > 0 {
		cfg.LLM.Models = entries
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := newPreparedServer(t, cfg, db, logger, "test", nil, nil)
	s.windowProbe = probe
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1") // register() replays exactly this code
	if resp := a.register("gate@b.io", "uncommon-password-9", false); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a, s, db
}

func TestContextWindowPinnedBeatsDiscovery(t *testing.T) {
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		t.Error("an all-pinned config must never probe the endpoint")
		return map[string]int{"m1": 9999}, true
	}, config.LLMModel{ID: "m1", Default: true, ContextSize: 5000})
	s.refreshContextWindows()
	if got := s.contextWindow("m1"); got != 5000 {
		t.Errorf("contextWindow(m1) = %d, want the pinned 5000", got)
	}
}

func TestContextWindowAutoDiscoveryPerModel(t *testing.T) {
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		return map[string]int{"m1": 9999, "m2": 4000}, true
	},
		config.LLMModel{ID: "m1", Default: true},
		config.LLMModel{ID: "m2"},
	)
	if got := s.contextWindow("m1"); got != 0 {
		t.Fatalf("fresh auto window = %d, want unknown", got)
	}
	s.refreshContextWindows() // unknown yet: this one waits
	if got := s.contextWindow("m1"); got != 9999 {
		t.Errorf("m1 window = %d, want 9999 after discovery", got)
	}
	if got := s.contextWindow("m2"); got != 4000 {
		t.Errorf("m2 window = %d, want 4000 after discovery", got)
	}
	// Listing spelling differs in case: the cache keys lower-cased.
	if got := s.contextWindow("M1"); got != 9999 {
		t.Errorf("case-insensitive lookup = %d, want 9999", got)
	}
}

func TestContextWindowUnknownIsNull(t *testing.T) {
	// The dumb-proxy case: nothing reported → unlimited, gate open.
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) { return nil, false })
	if got := s.contextWindow("m1"); got != 0 {
		t.Errorf("window = %d, want 0 while unresolved", got)
	}
	if adm, err := s.chat.AdmitSend(context.Background(), &store.Conversation{ID: "whatever", Model: "m1"}, "", "plenty of content", nil, nil); err != nil || adm.Full {
		t.Error("gate must be open while the window is unknown")
	}
}

func TestContextWindowRefreshIsAsyncWhenKnown(t *testing.T) {
	calls := make(chan struct{}, 8)
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		calls <- struct{}{}
		return map[string]int{"m1": 8000}, true
	})
	s.refreshContextWindows() // unknown yet: this one waits
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh never probed")
	}
	if got := s.contextWindow("m1"); got != 8000 {
		t.Fatalf("window = %d, want 8000", got)
	}
	start := time.Now()
	s.refreshContextWindows() // known now: must not block on the probe
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("refresh with a cached value blocked for %v", time.Since(start))
	}
}

// A conversation's payload carries the window of ITS model — the meter's
// denominator travels with the conversation, not with meta.
func TestConversationCarriesItsModelsWindow(t *testing.T) {
	a, _, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		return map[string]int{"m1": 9999}, true
	},
		config.LLMModel{ID: "m1", Default: true},
		config.LLMModel{ID: "m2", ContextSize: 6000},
	)
	c := createConv(t, a, map[string]any{"title": "win"})
	var proj map[string]any
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj["max_context_tokens"] != float64(9999) {
		t.Fatalf("default-model conversation window = %v, want discovered 9999", proj["max_context_tokens"])
	}
	patchResp := a.do(http.MethodPatch, "/api/v1/conversations/"+c.ID, map[string]any{"model": "m2"})
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch model: %d", patchResp.StatusCode)
	}
	getJSON(t, a, "/api/v1/conversations/"+c.ID, &proj)
	if proj["max_context_tokens"] != float64(6000) {
		t.Errorf("m2 conversation window = %v, want the pinned 6000", proj["max_context_tokens"])
	}
}

func TestConversationFullGate(t *testing.T) {
	// The gate now measures the ACTUAL built request (finding 7): system
	// prompt, tool guidance, tool schemas, manifests, history and the new
	// turn. The window is set relative to the measured empty-request
	// baseline, so these assertions pin the gate's arithmetic rather than
	// today's exact prompt size.
	a, s, db := newWindowTest(t,
		func(context.Context) (map[string]int, bool) { return nil, false },
		config.LLMModel{ID: "m1", Default: true})
	u, err := db.UserByEmail(context.Background(), "gate@b.io")
	if err != nil || u == nil {
		t.Fatalf("owner lookup: %v", err)
	}
	c := createConv(t, a, map[string]any{"title": "gate"})
	conv, err := db.ConversationForUser(context.Background(), u.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseMsgs, baseSpecs := s.chat.BuildRequest(conv, u.ID, nil)
	empty := chat.RequestTokens(baseMsgs, baseSpecs)
	if empty <= 0 {
		t.Fatal("baseline request estimated zero tokens")
	}
	// Window = baseline + 300 tokens of conversational room ON TOP of the
	// output reserve — the overflow line is the window minus the reserve,
	// so the reserve must be bought back for the room to be what it says.
	room := 300
	s.discoveredWindows.Swap("m1", int(empty)+room+s.cfg.LLM.MaxTokensResponse)

	seed := func(content string) {
		t.Helper()
		m := &store.Message{ConversationID: c.ID, Role: "assistant", Content: content, Status: "complete"}
		if err := db.AddMessage(context.Background(), u.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	// ~100 tokens of history against 300 tokens of room: gate open.
	seed(strings.Repeat("y", 400))
	if adm, err := s.chat.AdmitSend(context.Background(), conv, u.ID, "hi", nil, nil); err != nil || adm.Full {
		t.Errorf("gate closed with room to spare (err=%v adm=%+v)", err, adm)
	} else if len(adm.History) != 2 {
		// The gate's history is the prompt the send WOULD build: one
		// seeded row plus the incoming turn, decided from ONE read.
		t.Errorf("gate history = %d rows, want 2 (seeded + incoming)", len(adm.History))
	}
	// Cross the line with one big turn (~750 tokens): the gate answers
	// before the send pays for it.
	seed(strings.Repeat("x", 3000))
	if adm, err := s.chat.AdmitSend(context.Background(), conv, u.ID, "hi", nil, nil); err != nil || !adm.Full || adm.Reason != "context_full" {
		t.Errorf("gate open with history past the window (adm=%+v err=%v)", adm, err)
	}
	// A conversation the store cannot read never reads as full.
	ghost := &store.Conversation{ID: "00NOTAREALID00", Model: "m1"}
	if adm, err := s.chat.AdmitSend(context.Background(), ghost, u.ID, "hi", nil, nil); err != nil || adm.Full {
		t.Error("unknown conversation must not read as full")
	}
}

// Review2 finding 1: the history row cap is a REPLAY limit, not a token
// limit, so it must bind even while the window is still unknown — and it
// must count the turn about to land, refusing at the point the NEXT
// ContextHistory read would start failing with ErrHistoryLimit (len > cap),
// never one row early.
func TestHistoryCapBindsWithoutWindow(t *testing.T) {
	restore := store.SetContextHistoryCap(3)
	defer restore()
	// Probe returns nothing discoverable: the window stays unknown, the
	// token gate stays open — only the row cap can refuse.
	a, s, db := newWindowTest(t,
		func(context.Context) (map[string]int, bool) { return nil, false },
		config.LLMModel{ID: "m1", Default: true})
	if _, known := s.overflowLine("m1"); known {
		t.Fatal("window must stay unknown for this test")
	}
	u, err := db.UserByEmail(context.Background(), "gate@b.io")
	if err != nil || u == nil {
		t.Fatalf("owner lookup: %v", err)
	}
	c := createConv(t, a, map[string]any{"title": "cap"})
	conv, err := db.ConversationForUser(context.Background(), u.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(role, content string) string {
		t.Helper()
		m := &store.Message{ConversationID: c.ID, Role: role, Content: content, Status: "complete"}
		if err := db.AddMessage(context.Background(), u.ID, m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}

	// Two stored rows + the incoming turn = cap exactly: the LAST send
	// the cap allows — refusing here would be the early-refusal bug.
	seed("user", "one")
	last := seed("assistant", "two")
	if adm, err := s.chat.AdmitSend(context.Background(), conv, u.ID, "third", nil, nil); err != nil || adm.Full {
		t.Errorf("send landing exactly ON the cap refused (adm=%+v err=%v)", adm, err)
	} else if len(adm.History) != 3 {
		t.Errorf("gate history = %d rows, want 3", len(adm.History))
	}
	// A third stored row brings history TO the cap: readable, but the
	// incoming turn would make cap+1 — the next read would fail
	// ErrHistoryLimit, so the send is refused BEFORE anything persists.
	seed("user", "three")
	if adm, err := s.chat.AdmitSend(context.Background(), conv, u.ID, "fourth", nil, nil); err != nil || !adm.Full || adm.Reason != "history_limit" {
		t.Errorf("past-cap send accepted (adm=%+v err=%v)", adm, err)
	}
	// Regenerate excludes the retired reply: with the excluded row the
	// replayable history is back at the cap, so the same conversation
	// accepts a replacement — the cap counts what would remain, not
	// what is stored.
	if adm, err := s.chat.AdmitSend(context.Background(), conv, u.ID, "", nil, []string{last}); err != nil || adm.Full {
		t.Errorf("regenerate at the cap refused (adm=%+v err=%v)", adm, err)
	}
}

// Review2 finding 6: a DISCOVERED window smaller than the configured
// output reserve used to make the reserve silently vanish from the gate
// while requests still demanded it. The line now floors at zero, so every
// send is refused with context_full — an honest "start a new chat" (really:
// re-provision this model) instead of an overstatement of what fits.
// All three relationships the audit names: smaller, equal, larger.
func TestReserveAgainstDiscoveredWindow(t *testing.T) {
	_, s, _ := newWindowTest(t,
		func(context.Context) (map[string]int, bool) { return nil, false },
		config.LLMModel{ID: "m1", Default: true})
	reserve := int64(s.cfg.LLM.MaxTokensResponse)
	if reserve <= 100 {
		t.Fatalf("test needs a meaningful reserve, got %d", reserve)
	}
	conv := &store.Conversation{ID: "whatever", Model: "m1"}

	// Smaller than the reserve: line floors at 0, every send refused.
	s.discoveredWindows.Swap("m1", 100)
	line, known := s.overflowLine("m1")
	if !known || line != 0 {
		t.Fatalf("window %d < reserve %d: line = %d (known=%v), want 0/true", 100, reserve, line, known)
	}
	if adm, err := s.chat.AdmitSend(context.Background(), conv, "", "hi", nil, nil); err != nil || !adm.Full || adm.Reason != "context_full" {
		t.Errorf("send against reserve-sized window = (adm=%+v err=%v), want refusal", adm, err)
	}

	// Equal to the reserve: nothing is left for the prompt — refused too.
	s.discoveredWindows.Swap("m1", int(reserve))
	if line, _ := s.overflowLine("m1"); line != 0 {
		t.Fatalf("window == reserve: line = %d, want 0", line)
	}
	if adm, err := s.chat.AdmitSend(context.Background(), conv, "", "hi", nil, nil); err != nil || !adm.Full || adm.Reason != "context_full" {
		t.Errorf("window == reserve = (adm=%+v err=%v), want refusal", adm, err)
	}

	// Larger than the reserve: the normal arithmetic — window minus
	// reserve, and the emptiest conversation fits.
	s.discoveredWindows.Swap("m1", int(reserve)+50_000)
	line, _ = s.overflowLine("m1")
	if line != 50_000 {
		t.Fatalf("window > reserve: line = %d, want 50000", line)
	}
	if adm, err := s.chat.AdmitSend(context.Background(), conv, "", "hi", nil, nil); err != nil || adm.Full {
		t.Errorf("empty request against a 50k line refused (adm=%+v err=%v)", adm, err)
	}
}

// C1's typed contract: a failed history read is an ERROR, never an open
// gate with a nil history the generation would re-read (or build a partial
// prompt from). Refusals carry a verdict; failures carry an error.
func TestAdmitSendReadFailureIsTyped(t *testing.T) {
	_, s, db := newWindowTest(t,
		func(context.Context) (map[string]int, bool) { return nil, false },
		config.LLMModel{ID: "m1", Default: true})
	db.Close() // every store read fails from here
	adm, err := s.chat.AdmitSend(context.Background(),
		&store.Conversation{ID: "c-absent", Model: "m1"}, "u1", "hi", nil, nil)
	if err == nil || adm != nil {
		t.Fatalf("admission over a failed read = (%+v, %v), want (nil, error)", adm, err)
	}
}

// R5: overlapping refresh intent shares ONE GET /models. Opening ten chats
// against an unprobed endpoint used to launch ten probes; now the first
// starts the flight and the rest join it.
func TestContextWindowProbesCoalesce(t *testing.T) {
	gate, open := gate()
	started := make(chan struct{}, 2)
	var probes int32
	_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
		atomic.AddInt32(&probes, 1)
		started <- struct{}{}
		<-gate // slow endpoint: every other refresh lands mid-flight
		return map[string]int{"m1": 9999}, true
	}, config.LLMModel{ID: "m1", Default: true})

	first := s.ensureProbe()
	<-started
	for i := 1; i < 10; i++ {
		if joined := s.ensureProbe(); joined != first {
			t.Fatalf("caller %d started a separate probe instead of joining", i)
		}
	}
	open()
	<-first
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Errorf("probes = %d, want the ten refreshes to share one", n)
	}
	if got := s.contextWindow("m1"); got != 9999 {
		t.Errorf("contextWindow(m1) = %d, want 9999 after the shared probe", got)
	}

	// The marker resets with the flight: a later refresh probes again.
	second := s.ensureProbe()
	<-started
	<-second
	if n := atomic.LoadInt32(&probes); n != 2 {
		t.Errorf("probes after completion = %d, want a fresh probe (marker reset)", n)
	}
}

func TestStartContextWindowProbe(t *testing.T) {
	t.Run("pinned models do not probe", func(t *testing.T) {
		called := make(chan struct{}, 1)
		_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
			called <- struct{}{}
			return nil, false
		}, config.LLMModel{ID: "pinned", Default: true, ContextSize: 8192})
		s.StartContextWindowProbe()
		select {
		case <-called:
			t.Fatal("boot probe ran for an all-pinned configuration")
		default:
		}
	})

	t.Run("auto model is warmed in background", func(t *testing.T) {
		probed := make(chan struct{})
		_, s, _ := newWindowTest(t, func(context.Context) (map[string]int, bool) {
			close(probed)
			return map[string]int{"auto": 12288}, true
		}, config.LLMModel{ID: "auto", Default: true})
		s.StartContextWindowProbe()
		select {
		case <-probed:
		case <-time.After(2 * time.Second):
			t.Fatal("boot context-window probe did not run")
		}
		deadline := time.Now().Add(2 * time.Second)
		for s.contextWindow("auto") != 12288 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := s.contextWindow("auto"); got != 12288 {
			t.Fatalf("boot-probed window = %d, want 12288", got)
		}
	})
}
