package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mullsmith/internal/config"
	"mullsmith/internal/store"
)

func TestRecordLimitsAPI(t *testing.T) {
	fake := newFakeLLM(t, scriptReply("answer"))
	cfg := chatCfg(t, fake.URL)
	cfg.AppLimits.MaxConversationsPerUser = 2
	cfg.AppLimits.MaxMessagesPerUser = 3
	a := signedInChat(t, cfg)
	c := createConv(t, a, map[string]any{"title": "Named"}) // plus seeded thread = 2
	r := a.do("POST", "/api/v1/conversations", map[string]any{})
	if r.StatusCode != 409 || apiErrCode(t, r) != "conversation_limit" {
		t.Fatalf("conversation limit=%d", r.StatusCode)
	}
	response := sendChat(t, a, c.ID, "hello")
	newSSEReader(t, response).untilDone()
	response.Body.Close()
	// Three-row allowance cannot admit another two-row send. No partial turn.
	r = a.do("POST", "/api/v1/conversations/"+c.ID+"/messages", map[string]any{"content": "over"})
	if r.StatusCode != 409 || apiErrCode(t, r) != "message_limit" {
		t.Fatalf("message limit=%d", r.StatusCode)
	}
	history, _, err := a.db.ListMessages(context.Background(), ownerOf(t, a).ID, c.ID, "", 100)
	if err != nil || len(history) != 2 {
		t.Fatalf("partial send: %d %v", len(history), err)
	}
	r = a.do("POST", "/api/v1/messages/"+history[1].ID+"/regenerate", map[string]any{})
	if r.StatusCode != 200 {
		t.Fatalf("regenerate=%d %s", r.StatusCode, dumpBody(r))
	}
	newSSEReader(t, r).untilDone()
	r.Body.Close()
	r = a.do("DELETE", "/api/v1/conversations/"+c.ID, nil)
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	c = createConv(t, a, map[string]any{"title": "Freed"})
	response = sendChat(t, a, c.ID, "room again")
	newSSEReader(t, response).untilDone()
	response.Body.Close()
}

func TestMessageLimitDuringToolRoundsExplainsRefusal(t *testing.T) {
	// Unknown-tool feedback takes the third row; the next placeholder is refused
	// before another model request, with an explanation durable across reload.
	script := &toolLLM{replies: [][]string{{toolCallChunk(0, "missing-call", "missing", `{}`), finishChunk("tool_calls")}}}
	fake := newFakeLLM(t, script.handler)
	cfg := chatCfg(t, fake.URL)
	cfg.AppLimits.MaxMessagesPerUser = 3
	a := signedInChat(t, cfg)
	c := createConv(t, a, map[string]any{"title": "Named"})
	r := sendChat(t, a, c.ID, "hello")
	rd := newSSEReader(t, r)
	found := false
	for {
		ev, data := rd.next()
		if ev == "error" {
			if data["code"] != "message_limit" {
				t.Fatalf("error=%v", data)
			}
			found = true
		}
		if ev == "done" {
			if data["status"] != "error" {
				t.Fatalf("done=%v", data)
			}
			break
		}
	}
	r.Body.Close()
	if !found {
		t.Fatal("missing limit explanation")
	}
	items, _, err := a.db.ListMessages(context.Background(), ownerOf(t, a).ID, c.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	note := ""
	for _, m := range items {
		if m.Role == "assistant" {
			note = m.StatusNote
		}
	}
	if !strings.Contains(note, store.MessageLimitNotice) {
		t.Fatalf("durable note=%q", note)
	}
}

func TestMessageLimitReservesToolResultBeforeFileSideEffect(t *testing.T) {
	a, _, cfg := newRealToolServerTuned(t, [][]string{{
		toolCallChunk(0, "write-at-limit", "fs_write",
			`{"path":"work/must-not-exist","content":"side effect"}`),
		finishChunk("tool_calls"),
	}}, func(cfg *config.Config) {
		// The accepted send consumes both available rows. The tool-result
		// reservation must fail before fs_write runs.
		cfg.AppLimits.MaxMessagesPerUser = 2
	})
	c := createConv(t, a, map[string]any{"title": "Tool capacity"})
	r := sendChat(t, a, c.ID, "write")
	rd := newSSEReader(t, r)
	var sawLimit bool
	for {
		ev, data := rd.next()
		if ev == "error" && data["code"] == "message_limit" {
			sawLimit = true
		}
		if ev == "done" {
			if data["status"] != "error" {
				t.Fatalf("done=%v", data)
			}
			break
		}
	}
	r.Body.Close()
	if !sawLimit {
		t.Fatal("missing message-limit terminal error")
	}
	if _, err := os.Stat(filepath.Join(cfg.Scratchpad.Root, meID(a), "work/must-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("tool side effect occurred despite failed result reservation: %v", err)
	}
}
