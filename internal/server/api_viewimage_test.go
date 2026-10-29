package server

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gabkeep/internal/config"
)

// view_image (llm.vision): the model asks to see an uploads/ picture,
// the loop injects a synthetic user turn carrying it, and the turn is
// persisted — so later requests rebuild the same picture from history.
// Without vision the tool is never registered, and the model's attempt
// is a plain unknown-tool answer, exactly like any other missing tool.

func imageDataPart(t *testing.T, msg map[string]any) map[string]any {
	t.Helper()
	parts, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content = %T, want a content-parts array", msg["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %+v, want text + one picture", parts)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("part 1 = %+v, want an image_url part", parts[1])
	}
	iu, _ := img["image_url"].(map[string]any)
	url, _ := iu["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("image_url = %q, want a png data URL", url)
	}
	return iu
}

// toolNames lists the function names offered in a round's "tools" field.
func toolNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["tools"].([]any)
	var out []string
	for _, s := range raw {
		m, _ := s.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		out = append(out, name)
	}
	return out
}

func TestChatViewImageAttachesPictureTurn(t *testing.T) {
	a, script, _ := newRealToolServerTuned(t, [][]string{
		{toolCallChunk(0, "call_v", "view_image", `{"path":"uploads/photo.png"}`),
			finishChunk("tool_calls")},
		{answerChunk("A red bicycle."), finishChunk("stop")},
		{answerChunk("Still a red bicycle."), finishChunk("stop")},
	}, func(cfg *config.Config) { cfg.LLM.Vision = true })

	raw := pngBytes(24)
	upPath := uploadOK(t, a, "photo.png", raw)
	if upPath != "uploads/photo.png" {
		t.Fatalf("upload path = %q; the scripted tool call assumes uploads/photo.png", upPath)
	}

	c := createConv(t, a, map[string]any{"title": "Bicycle"})
	resp := sendChat(t, a, c.ID, "look at the photo I uploaded")
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))
	if es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}
	if te := es.data["tool_end"]; te["status"] != "ok" {
		t.Fatalf("view_image tool_end = %v", te)
	}
	if !slices.Contains(toolNames(t, script.roundBody(0)), "view_image") {
		t.Errorf("round 1 request does not offer view_image")
	}

	// Round 2 carries the injected turn: a second user message whose
	// parts are the note + manifest text and the picture itself —
	// byte-identical to what a fresh upload would send.
	users := payloadRole(t, script.roundBody(1), "user")
	if len(users) != 2 {
		t.Fatalf("user messages in round 2 = %d, want 2", len(users))
	}
	parts, _ := users[1]["content"].([]any)
	txt, _ := parts[0].(map[string]any)
	text, _ := txt["text"].(string)
	if !strings.Contains(text, "view_image attached") ||
		!strings.Contains(text, "uploads/photo.png") {
		t.Errorf("injected note text = %q", text)
	}
	iu := imageDataPart(t, users[1])
	decoded, err := base64.StdEncoding.DecodeString(
		strings.TrimPrefix(iu["url"].(string), "data:image/png;base64,"))
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Errorf("injected bytes = %x (err %v), want %x", decoded, err, raw)
	}

	// A fresh send replays the picture from the store: persistence, not
	// a one-off in the live loop.
	resp2 := sendChat(t, a, c.ID, "and again — what colour?")
	defer resp2.Body.Close()
	if es2 := drainSSE(t, newSSEReader(t, resp2)); es2.done["status"] != "complete" {
		t.Fatalf("second done = %v", es2.done)
	}
	users2 := payloadRole(t, script.roundBody(2), "user")
	if len(users2) != 3 {
		t.Fatalf("user messages on the next send = %d, want 3", len(users2))
	}
	imageDataPart(t, users2[1]) // still the picture, same slot

	if n := payloadRole(t, script.roundBody(2), "tool"); len(n) != 1 {
		t.Errorf("replayed tool rows = %d, want 1", len(n))
	}

	// The API projects the row's synthetic flag so the browser hides it
	// without sniffing content; genuine rows carry no flag at all.
	pageResp := a.do(http.MethodGet, "/api/v1/conversations/"+c.ID+"/messages", nil)
	page := decodedBody(t, pageResp)
	raws, _ := page["items"].([]any)
	seenFlagged, seenGenuine := 0, 0
	for _, it := range raws {
		m, _ := it.(map[string]any)
		s, isUser := m["synthetic"]
		switch {
		case strings.HasPrefix(m["content"].(string), viewTurnMarker):
			if !isUser || s != true {
				t.Errorf("view turn projected without synthetic=true: %v", m)
			}
			seenFlagged++
		case m["role"] == "user":
			if isUser {
				t.Errorf("genuine user message carries a synthetic flag: %v", m)
			}
			seenGenuine++
		}
	}
	if seenFlagged != 1 || seenGenuine != 2 { // both typed messages of the test
		t.Errorf("projected rows: flagged = %d, genuine = %d, want 1 and 2", seenFlagged, seenGenuine)
	}
}

func TestChatViewImageAbsentWithoutVision(t *testing.T) {
	a, script, _ := newRealToolServer(t, [][]string{
		{toolCallChunk(0, "call_v", "view_image", `{"path":"uploads/photo.png"}`),
			finishChunk("tool_calls")},
		{answerChunk("No pictures for this endpoint."), finishChunk("stop")},
	})
	c := createConv(t, a, map[string]any{"title": "Blind"})
	resp := sendChat(t, a, c.ID, "look at a photo")
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))
	if es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}
	if slices.Contains(toolNames(t, script.roundBody(0)), "view_image") {
		t.Errorf("vision off, yet the request offers view_image")
	}
	tools := payloadRole(t, script.roundBody(1), "tool")
	if len(tools) != 1 || !strings.Contains(tools[0]["content"].(string), "unknown tool") {
		t.Fatalf("tool answer = %+v, want unknown tool", tools)
	}
	// No synthetic picture turn ever entered the history.
	if users := payloadRole(t, script.roundBody(1), "user"); len(users) != 1 {
		t.Errorf("user messages = %d, want only the original", len(users))
	}
}
