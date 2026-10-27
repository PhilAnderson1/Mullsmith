package server

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// Vision (llm.vision): with the flag on, raster uploads attached to a
// message ride to the endpoint as OpenAI image_url content parts (data:
// URLs); with it off — or when an attachment is oversized, non-raster by
// content sniffing, or past the per-message picture cap — the pipeline
// stays exactly the pre-vision manifest text, so a text-only endpoint is
// never shown a shape it cannot parse.

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

func TestChatVisionEmbedsImageAsDataURL(t *testing.T) {
	a, script, cfg := newRealToolServer(t, [][]string{
		{answerChunk("A red bicycle."), finishChunk("stop")},
	})
	cfg.LLM.Vision = true

	raw := pngBytes(24)
	upPath := uploadOK(t, a, "photo.png", raw)

	c := createConv(t, a, map[string]any{"title": "Vision"})
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "what is in this picture?", "attachment_paths": []string{upPath},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, dumpBody(resp))
	}
	if es := drainSSE(t, newSSEReader(t, resp)); es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}

	users := payloadRole(t, script.roundBody(0), "user")
	if len(users) != 1 {
		t.Fatalf("user messages = %+v", users)
	}
	parts, ok := users[0]["content"].([]any)
	if !ok {
		t.Fatalf("vision content = %T, want a content-parts array", users[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %+v, want text + one picture", parts)
	}
	text, _ := parts[0].(map[string]any)
	txt, _ := text["text"].(string)
	if text["type"] != "text" || !strings.Contains(txt, "what is in this picture?") {
		t.Fatalf("part 0 = %+v, want the question text", parts[0])
	}
	if !strings.Contains(txt, "uploads/photo.png") || !strings.Contains(txt, "inline=yes") {
		t.Errorf("manifest must still name the file and mark it embedded: %q", txt)
	}
	img, _ := parts[1].(map[string]any)
	iu, _ := img["image_url"].(map[string]any)
	url, _ := iu["url"].(string)
	if img["type"] != "image_url" || !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("part 1 = %+v, want an image_url data URL", parts[1])
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/png;base64,"))
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Errorf("embedded bytes = %x (err %v), want the uploaded %x", decoded, err, raw)
	}
}

func TestChatVisionOffKeepsPlainStringContent(t *testing.T) {
	a, script, _ := newRealToolServer(t, [][]string{
		{answerChunk("I see no picture."), finishChunk("stop")},
	})
	// cfg.LLM.Vision stays at its default false: text-only endpoint.

	uploadOK(t, a, "photo.png", pngBytes(24))
	c := createConv(t, a, map[string]any{"title": "NoVision"})
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "what is in this picture?", "attachment_paths": []string{"uploads/photo.png"},
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
	if strings.Contains(content, "data:image") || strings.Contains(content, "inline=yes") {
		t.Errorf("nothing may be embedded with vision off: %q", content)
	}
}

func TestChatVisionRefusesOversizedAndNonRaster(t *testing.T) {
	a, script, cfg := newRealToolServer(t, [][]string{
		{answerChunk("Noted."), finishChunk("stop")},
	})
	cfg.LLM.Vision = true
	cfg.LLM.VisionMaxImageBytes = 64

	oversized := uploadOK(t, a, "big.png", pngBytes(100)) // 108 B > 64 cap
	texty := uploadOK(t, a, "notes.png",                  // .png name, text bytes
		[]byte("this is really plain text wearing a picture name"))

	c := createConv(t, a, map[string]any{"title": "Degrade"})
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "two files", "attachment_paths": []string{oversized, texty},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, dumpBody(resp))
	}
	drainSSE(t, newSSEReader(t, resp))

	body := toJSON(t, script.roundBody(0))
	if strings.Contains(body, "data:image") {
		t.Errorf("oversized/sniffed-non-image bytes must never be embedded: %s", body)
	}
	if strings.Contains(body, "inline=yes") {
		t.Errorf("refused images must not be annotated as embedded: %s", body)
	}
	// Both files still appear in the manifest — the fs_read pointer is the
	// graceful degradation path, unchanged from the pre-vision pipeline.
	if !strings.Contains(body, "uploads/big.png") || !strings.Contains(body, "uploads/notes.png") {
		t.Errorf("manifest lost a degraded attachment: %s", body)
	}
}

func TestChatVisionCapsPicturesPerMessage(t *testing.T) {
	a, script, cfg := newRealToolServer(t, [][]string{
		{answerChunk("Two pics."), finishChunk("stop")},
	})
	cfg.LLM.Vision = true
	cfg.LLM.VisionMaxImages = 1

	p1 := uploadOK(t, a, "a1.png", pngBytes(8))
	p2 := uploadOK(t, a, "b2.png", pngBytes(8))

	c := createConv(t, a, map[string]any{"title": "Cap"})
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "both?", "attachment_paths": []string{p2, p1},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, dumpBody(resp))
	}
	drainSSE(t, newSSEReader(t, resp))

	users := payloadRole(t, script.roundBody(0), "user")
	parts, ok := users[0]["content"].([]any)
	if !ok || len(parts) != 2 { // text + exactly ONE picture, despite two uploads
		t.Fatalf("parts = %+v, want text + 1 picture at cap 1", users[0]["content"])
	}
	txt, _ := parts[0].(map[string]any)["text"].(string)
	if got := strings.Count(txt, "inline=yes"); got != 1 {
		t.Errorf("inline=yes count = %d, want 1: %q", got, txt)
	}
}

func TestChatVisionReplaysPicturesInHistory(t *testing.T) {
	a, script, cfg := newRealToolServer(t, [][]string{
		{answerChunk("A red bicycle."), finishChunk("stop")},
		{answerChunk("Still red."), finishChunk("stop")},
	})
	cfg.LLM.Vision = true

	uploadOK(t, a, "photo.png", pngBytes(24))
	c := createConv(t, a, map[string]any{"title": "Replay"})
	r1 := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "what is in this picture?", "attachment_paths": []string{"uploads/photo.png"},
	})
	if r1.StatusCode != 200 {
		t.Fatalf("chat 1: %d %s", r1.StatusCode, dumpBody(r1))
	}
	drainSSE(t, newSSEReader(t, r1))

	r2 := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages",
		map[string]any{"content": "what color was it?"})
	if r2.StatusCode != 200 {
		t.Fatalf("chat 2: %d %s", r2.StatusCode, dumpBody(r2))
	}
	drainSSE(t, newSSEReader(t, r2))

	// Round 2 replays history: the picture turn must still carry its image
	// part (otherwise the follow-up question would be unanswerable), while
	// the new text-only turn stays a plain string.
	users := payloadRole(t, script.roundBody(1), "user")
	if len(users) != 2 {
		t.Fatalf("history user turns = %+v", users)
	}
	parts, ok := users[0]["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("replayed picture turn = %T %v", users[0]["content"], users[0]["content"])
	}
	if toJSON(t, parts[1]) == "" || !strings.Contains(toJSON(t, parts[1]), "data:image/png;base64,") {
		t.Errorf("replayed picture lost its data URL: %+v", parts[1])
	}
	if _, ok := users[1]["content"].(string); !ok {
		t.Errorf("text-only follow-up must stay a plain string, got %T", users[1]["content"])
	}
}
