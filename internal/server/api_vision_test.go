package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"mullsmith/internal/config"
)

// Uploads carry pointers, never pictures (§7.2): whatever llm.vision
// says, an attachment rides to the endpoint as a manifest line naming
// the file; the bytes cross the wire only when the model asks for them
// with view_image (api_viewimage_test.go). These tests pin the
// negative: nothing embeds, not now, not on replay, not after the
// viewed bytes have aged away.

// pngBytes returns bytes whose SNIFFED type is image/png (the 8-byte
// signature is all http.DetectContentType keys on) of the requested size.
func pngBytes(body int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	return append(b, bytes.Repeat([]byte{0x2a}, body)...)
}

func uploadOK(t *testing.T, a *authTest, name string, content []byte) string {
	t.Helper()
	resp := a.upload(name, content)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d %v", name, resp.StatusCode, decodedBody(t, resp))
	}
	return decodedBody(t, resp)["path"].(string)
}

func TestChatUploadStaysManifestOnlyWithVisionOn(t *testing.T) {
	a, script, cfg := newRealToolServer(t, [][]string{
		{answerChunk("I will open it with a tool."), finishChunk("stop")},
	})
	cfg.LLM.Vision = true

	upPath := uploadOK(t, a, "photo.png", pngBytes(24))
	c := createConv(t, a, map[string]any{"title": "Pointer"})
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "what is in this picture?", "attachment_paths": []string{upPath},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, dumpBody(resp))
	}
	if es := drainSSE(t, newSSEReader(t, resp)); es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}

	body := toJSON(t, script.roundBody(0))
	users := payloadRole(t, script.roundBody(0), "user")
	content, ok := users[0]["content"].(string)
	if !ok {
		t.Fatalf("attached content = %T, want a plain string — uploads never embed", users[0]["content"])
	}
	if !strings.Contains(content, "[Attached files") || !strings.Contains(content, "uploads/photo.png") {
		t.Errorf("manifest missing: %q", content)
	}
	if !strings.Contains(content, "view_image") {
		t.Errorf("manifest must point at view_image as the way to see a picture: %q", content)
	}
	if strings.Contains(body, "data:image") || strings.Contains(body, "inline=yes") {
		t.Errorf("nothing may embed since view_image owns the picture path: %s", body)
	}
}

func TestChatUploadStaysManifestOnlyOnReplay(t *testing.T) {
	a, script, cfg := newRealToolServer(t, [][]string{
		{answerChunk("Noted."), finishChunk("stop")},
		{answerChunk("Still just the pointer."), finishChunk("stop")},
	})
	cfg.LLM.Vision = true

	upPath := uploadOK(t, a, "photo.png", pngBytes(24))
	c := createConv(t, a, map[string]any{"title": "Replay"})
	r1 := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "look", "attachment_paths": []string{upPath},
	})
	if r1.StatusCode != 200 {
		t.Fatalf("chat 1: %d %s", r1.StatusCode, dumpBody(r1))
	}
	drainSSE(t, newSSEReader(t, r1))

	r2 := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages",
		map[string]any{"content": "and now?"})
	if r2.StatusCode != 200 {
		t.Fatalf("chat 2: %d %s", r2.StatusCode, dumpBody(r2))
	}
	drainSSE(t, newSSEReader(t, r2))

	body2 := toJSON(t, script.roundBody(1))
	if strings.Contains(body2, "data:image") || strings.Contains(body2, "inline=yes") {
		t.Errorf("history replay must not start embedding: %s", body2)
	}
	users := payloadRole(t, script.roundBody(1), "user")
	if len(users) < 1 {
		t.Fatal("history lost the attached user message")
	}
	first, ok := users[0]["content"].(string)
	if !ok || !strings.Contains(first, "uploads/photo.png") {
		t.Errorf("replayed manifest broken: %T %v", users[0]["content"], first)
	}
}

func TestChatVisionOffKeepsPlainStringContent(t *testing.T) {
	a, script, _ := newRealToolServer(t, [][]string{
		{answerChunk("I see no picture."), finishChunk("stop")},
	})
	// cfg.LLM.Vision stays at its default false: text-only endpoint.

	upPath := uploadOK(t, a, "photo.png", pngBytes(24))
	c := createConv(t, a, map[string]any{"title": "NoVision"})
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "what is in this picture?", "attachment_paths": []string{upPath},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, dumpBody(resp))
	}
	drainSSE(t, newSSEReader(t, resp))

	users := payloadRole(t, script.roundBody(0), "user")
	content, ok := users[0]["content"].(string)
	if !ok {
		t.Fatalf("vision off must keep plain string content, got %T", users[0]["content"])
	}
	if !strings.Contains(content, "[Attached files") || !strings.Contains(content, "uploads/photo.png") {
		t.Errorf("manifest missing: %q", content)
	}
	if strings.Contains(content, "view_image") {
		t.Errorf("with vision off the manifest must not advertise a tool that is not offered: %q", content)
	}
	if strings.Contains(content, "data:image") || strings.Contains(content, "inline=yes") {
		t.Errorf("nothing may be embedded with vision off: %q", content)
	}
}

// The marker rule ages gracefully: a viewed picture whose bytes vanish
// afterwards (TTL sweep, fs_delete) replays as manifest-only plain
// text — never a half-built parts array.
func TestChatViewTurnReplayDegradesWhenBytesGone(t *testing.T) {
	a, script, _ := newRealToolServerTuned(t, [][]string{
		{toolCallChunk(0, "call_v", "view_image", `{"path":"uploads/photo.png"}`),
			finishChunk("tool_calls")},
		{answerChunk("Lovely."), finishChunk("stop")},
		{answerChunk("Gone, but the pointer stays."), finishChunk("stop")},
	}, func(cfg *config.Config) { cfg.LLM.Vision = true })

	upPath := uploadOK(t, a, "photo.png", pngBytes(24))
	if upPath != "uploads/photo.png" {
		t.Fatalf("unexpected upload path %q", upPath)
	}
	c := createConv(t, a, map[string]any{"title": "Faded"})
	r1 := sendChat(t, a, c.ID, "see it")
	if es := drainSSE(t, newSSEReader(t, r1)); es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}
	r1.Body.Close()
	if users := payloadRole(t, script.roundBody(1), "user"); len(users) != 2 {
		t.Fatalf("view turn missing in round 2: %+v", users)
	}

	// Delete the file, as a TTL sweep would, then ask again.
	del := a.do(http.MethodDelete, "/api/v1/files?path=uploads%2Fphoto.png", nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", del.StatusCode)
	}
	r2 := sendChat(t, a, c.ID, "still see it?")
	defer r2.Body.Close()
	if es2 := drainSSE(t, newSSEReader(t, r2)); es2.done["status"] != "complete" {
		t.Fatalf("second done = %v", es2.done)
	}

	body := toJSON(t, script.roundBody(2))
	if strings.Contains(body, "data:image") || strings.Contains(body, "inline=yes") {
		t.Errorf("a viewed picture whose bytes vanished must replay as plain text: %s", body)
	}
	users := payloadRole(t, script.roundBody(2), "user")
	if len(users) != 3 {
		t.Fatalf("user messages = %d, want 3", len(users))
	}
	if _, ok := users[1]["content"].(string); !ok {
		t.Errorf("view turn content = %T, want plain string after the bytes vanished", users[1]["content"])
	}
}
