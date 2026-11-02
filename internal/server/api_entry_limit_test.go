package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScratchpadEntryLimitBeforeUploadAndAfterDeletion(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.Scratchpad.MaxEntries = 5
	a := newAuthTest(t, cfg)
	signupAs(a, "entries@example.test", "entry-invite")
	for _, name := range []string{"one.txt", "two.txt"} {
		r := a.upload(name, nil)
		r.Body.Close()
		if r.StatusCode != 201 {
			t.Fatalf("upload=%d", r.StatusCode)
		}
	}
	req, payload := uploadRequest(t, a)
	body := &trackedRequestBody{Reader: bytes.NewReader(payload)}
	req.Body = io.NopCloser(body)
	w := httptest.NewRecorder()
	a.ts.Config.Handler.ServeHTTP(w, req)
	if w.Code != 507 || body.reads != 0 {
		t.Fatalf("full scratchpad=%d reads=%d %s", w.Code, body.reads, w.Body.String())
	}
	if code := apiErrCode(t, w.Result()); code != "entry_limit" {
		t.Fatalf("code=%s", code)
	}
	listing := decodedBody(t, a.do(http.MethodGet, "/api/v1/files", nil))
	quota := listing["quota"].(map[string]any)
	if len(listing["items"].([]any)) != 2 || quota["used_bytes"].(float64) != 0 {
		t.Fatalf("at-limit listing=%v", listing)
	}
	r := a.do(http.MethodDelete, "/api/v1/files?path=uploads%2Fone.txt", nil)
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	r = a.upload("new.txt", nil)
	r.Body.Close()
	if r.StatusCode != 201 {
		t.Fatalf("deletion did not free room: %d", r.StatusCode)
	}
}

func TestOverLimitScratchpadStillListsForCleanup(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.Scratchpad.MaxEntries = 4
	a := newAuthTest(t, cfg)
	signupAs(a, "cleanup@example.test", "cleanup-invite")
	user := meID(a)
	// A code-run or configuration change can leave a tree above the current cap.
	seedFile(t, cfg, user, "uploads/one.txt", nil)
	seedFile(t, cfg, user, "uploads/two.txt", nil)
	response := a.do(http.MethodGet, "/api/v1/files", nil)
	if response.StatusCode != 200 {
		t.Fatalf("cleanup listing=%d", response.StatusCode)
	}
	listing := decodedBody(t, response)
	quota := listing["quota"].(map[string]any)
	if len(listing["items"].([]any)) != 2 || quota["entry_limit_reached"] != true || quota["used_bytes"] != nil {
		t.Fatalf("incomplete quota misreported: %v", listing)
	}
	r := a.do(http.MethodDelete, "/api/v1/files?path=uploads%2Fone.txt", nil)
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	listing = decodedBody(t, a.do(http.MethodGet, "/api/v1/files", nil))
	quota = listing["quota"].(map[string]any)
	if quota["entry_limit_reached"] == true || len(listing["items"].([]any)) != 1 || quota["used_bytes"].(float64) != 0 {
		t.Fatalf("cleanup not reflected: %v", listing)
	}
}
