package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/store"
)

// newRealToolServer wires a signed-in session to a server built exactly like
// production (server.New): the registry comes from config, including the fs_*
// tools and files_share backed by the real artifactSink and store.
func newRealToolServer(t *testing.T, replies [][]string) (*authTest, *toolLLM, *store.DB, *config.Config) {
	t.Helper()
	script := &toolLLM{replies: replies}
	fake := newFakeLLM(t, script.handler)
	cfg := chatCfg(t, fake.URL)
	cfg.Scratchpad.MaxFileSize = 1 << 20
	cfg.Scratchpad.UserQuota = 1 << 20
	cfg.Scratchpad.ArtifactTTL = config.Duration(168 * time.Hour)
	cfg.Tools.Agent.MaxToolRounds = 4
	cfg.Tools.Agent.Approval = map[string]string{
		"fs_list": "auto", "fs_read": "auto", "fs_write": "auto",
		"fs_delete": "auto", "files_share": "auto",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := New(cfg, db, logger, "test")
	ts := httptest.NewServer(s.Handler())

	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("fstools@test.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a, script, db, cfg
}

// TestChatFsToolsEndToEnd walks the M5 exit flow minus the HTTP upload leg:
// model writes a report with fs_write, shares it with files_share, and the
// artifact really downloads afterwards.
func TestChatFsToolsEndToEnd(t *testing.T) {
	a, script, db, _ := newRealToolServer(t, [][]string{
		{ // round 1: fs_write
			toolCallChunk(0, "call_w", "fs_write",
				`{"path":"artifacts/report.md","content":"# Report\nvalue=42\n"}`),
			finishChunk("tool_calls"),
		},
		{ // round 2: files_share
			toolCallChunk(0, "call_s", "files_share", `{"path":"artifacts/report.md","note":"here you go"}`),
			finishChunk("tool_calls"),
		},
		{ // round 3: final words
			answerChunk("Report ready."),
			finishChunk("stop"),
		},
	})
	c := createConv(t, a, map[string]any{"title": "Files"})

	resp := sendChat(t, a, c.ID, "write me a report")
	defer resp.Body.Close()
	es := drainSSE(t, newSSEReader(t, resp))

	if n := countEvents(es, "tool_end"); n != 2 {
		t.Fatalf("tool_end events = %d, want 2 (%v)", n, es.events)
	}
	if es.data["tool_end"]["status"] != "ok" {
		t.Fatalf("files_share tool_end = %v", es.data["tool_end"])
	}
	// Download card payload reaches the UI (§10.2).
	endData, _ := es.data["tool_end"]["data"].(map[string]any)
	file, _ := endData["file"].(map[string]any)
	fileID, _ := file["id"].(string)
	if fileID == "" || file["url"] != "/api/v1/files/"+fileID+"/download" {
		t.Fatalf("tool_end card data = %v", endData)
	}
	if es.done["status"] != "complete" {
		t.Errorf("done = %v", es.done)
	}

	// The artifact row exists with a TTL and the bytes come back down.
	ctx := t.Context()
	uid := meID(a)
	row, err := db.GetFileForUser(ctx, uid, fileID)
	if err != nil || row.Kind != store.FileKindArtifact || row.ExpiresAt.IsZero() {
		t.Fatalf("artifact row = %+v, %v", row, err)
	}
	if time.Until(row.ExpiresAt) <= 0 {
		t.Error("artifact must not be expired on share")
	}
	dl := a.do(http.MethodGet, "/api/v1/files/"+fileID+"/download", nil)
	body, _ := io.ReadAll(dl.Body)
	dl.Body.Close()
	if dl.StatusCode != http.StatusOK || string(body) != "# Report\nvalue=42\n" {
		t.Fatalf("artifact download: %d %q", dl.StatusCode, body)
	}
	// Downloads are forced attachments (§9.4).
	if cd := dl.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// The fs_write result was replayed to the model on round 2 (§6.1).
	toolMsgs := payloadRole(t, script.roundBody(1), "tool")
	if len(toolMsgs) != 1 || !strings.Contains(toolMsgs[0]["content"].(string), "bytes_written") {
		t.Errorf("round-2 tool replay = %+v", toolMsgs)
	}
	// files_share result on round 3 carries the file id for the model too
	// (both tool answers of the prior rounds replay).
	toolMsgs = payloadRole(t, script.roundBody(2), "tool")
	if len(toolMsgs) != 2 {
		t.Fatalf("round-3 tool replay = %+v", toolMsgs)
	}
	if !strings.Contains(toJSON(t, toolMsgs), fileID) {
		t.Errorf("round-3 replay missing file id: %+v", toolMsgs)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	return string(b)
}

func countEvents(es eventStream, name string) int {
	n := 0
	for _, e := range es.events {
		if e == name {
			n++
		}
	}
	return n
}

// TestChatAttachmentManifestToModel: an uploaded file attached to a message
// shows up in the upstream prompt as a manifest (never its content), and the
// message API echoes the file for UI chips.
func TestChatAttachmentManifestToModel(t *testing.T) {
	a, script, _, _ := newRealToolServer(t, [][]string{
		{answerChunk("Got your CSV."), finishChunk("stop")},
	})
	up := a.upload("sales.csv", []byte("name,qty\nwidget,3\n"))
	if up.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %v", up.StatusCode, decodedBody(t, up))
	}
	fileID := decodedBody(t, up)["id"].(string)

	c := createConv(t, a, map[string]any{"title": "Att"})
	resp := a.do(http.MethodPost, "/api/v1/conversations/"+c.ID+"/messages", map[string]any{
		"content": "summarize this", "attachment_ids": []string{fileID},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, dumpBody(resp))
	}
	es := drainSSE(t, newSSEReader(t, resp))
	if es.done["status"] != "complete" {
		t.Fatalf("done = %v", es.done)
	}

	users := payloadRole(t, script.roundBody(0), "user")
	if len(users) != 1 {
		t.Fatalf("user messages = %+v", users)
	}
	content, _ := users[0]["content"].(string)
	if !strings.Contains(content, "summarize this") ||
		!strings.Contains(content, "[Attached files") ||
		!strings.Contains(content, "uploads/"+fileID+".csv") ||
		strings.Contains(content, "widget,3") { // content itself must NOT be inlined
		t.Errorf("manifest in prompt = %q", content)
	}

	// Reload path: GET messages exposes the attachment for chips.
	lj := decodedBody(t, a.do(http.MethodGet, "/api/v1/conversations/"+c.ID+"/messages", nil))
	items, _ := lj["items"].([]any)
	var userMsg map[string]any
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["role"] == "user" {
			userMsg = m
		}
	}
	files, _ := userMsg["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("message files = %v", userMsg)
	}
	f0 := files[0].(map[string]any)
	if f0["name"] != "sales.csv" || f0["id"] != fileID ||
		!strings.HasPrefix(f0["download_url"].(string), "/api/v1/files/") {
		t.Errorf("file JSON = %v", f0)
	}
}
