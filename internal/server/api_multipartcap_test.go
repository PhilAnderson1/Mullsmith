package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"gabkeep/internal/auth"
	"gabkeep/internal/config"
)

// --- audit finding 6: the whole upload request must carry one budget -------

// multipartBody builds a request with ignored file-style fields first (to
// exercise the drain path) and optionally the real file part last.
func multipartBody(t *testing.T, nJunk int, junkSize int, fileName string, fileContent []byte) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for i := 0; i < nJunk; i++ {
		part, err := mw.CreateFormFile("junk", "junk.bin")
		if err != nil {
			t.Fatal(err)
		}
		part.Write(bytes.Repeat([]byte("x"), junkSize))
	}
	if fileName != "" {
		part, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(fileContent)
	}
	mw.Close()
	return b.Bytes(), mw.FormDataContentType()
}

func postBody(t *testing.T, a *authTest, body []byte, contentType string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/files", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(auth.CSRFHeaderName, a.csrf)
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Error code only when there IS an envelope — 2xx bodies are metadata.
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	_ = json.Unmarshal(b, &env)
	return resp.StatusCode, env.Error.Code
}

func capCfg(t *testing.T, maxFile int64) *config.Config {
	t.Helper()
	cfg := quotaCfg(t, 1<<20)
	cfg.Scratchpad.MaxFileSize = config.Size(maxFile)
	return cfg
}

// The audit's repro: an ignored multipart field was drained and discarded
// without any byte cap — 1 MiB accepted with both limits at 1 KiB. The
// request now shares one budget (max_file_size + framing allowance), so
// the oversized junk part is refused mid-stream with 413.
func TestOversizedIgnoredFieldRejected(t *testing.T) {
	a, _ := pwTestApp(t, capCfg(t, 1024))
	signupAs(a, "mcap1@test.io", "gk-code-1")

	body, ct := multipartBody(t, 1, 128<<10, "ok.txt", []byte("hi"))
	code, errCode := postBody(t, a, body, ct)
	if code != http.StatusRequestEntityTooLarge || errCode != "upload_too_large" {
		t.Fatalf("128 KiB junk field: %d %s, want 413 upload_too_large", code, errCode)
	}
}

// With no file part anywhere, the old loop drained the entire body to EOF
// before answering 400. The budget must cut that off with 413 instead.
func TestBodyWithoutFilePartNotDrainedToEOF(t *testing.T) {
	a, _ := pwTestApp(t, capCfg(t, 1024))
	signupAs(a, "mcap2@test.io", "gk-code-1")

	body, ct := multipartBody(t, 1, 128<<10, "", nil)
	code, errCode := postBody(t, a, body, ct)
	if code != http.StatusRequestEntityTooLarge || errCode != "upload_too_large" {
		t.Fatalf("junk-only body: %d %s, want 413 upload_too_large", code, errCode)
	}
}

// Legitimate traffic must not feel the cap: one small ignored field plus a
// small file still uploads.
func TestUploadWithSmallExtraFieldsStillWorks(t *testing.T) {
	a, _ := pwTestApp(t, capCfg(t, 1024))
	signupAs(a, "mcap3@test.io", "gk-code-1")

	body, ct := multipartBody(t, 1, 16, "ok.txt", []byte("hi"))
	code, errCode := postBody(t, a, body, ct)
	if code != http.StatusCreated {
		t.Fatalf("legit upload: %d %s, want 201", code, errCode)
	}
}

// A file of exactly max_file_size must still fit — the allowance covers
// framing, and the copy cap is max+1 only to distinguish "exactly fits".
func TestFileExactlyAtMaxUploads(t *testing.T) {
	a, _ := pwTestApp(t, capCfg(t, 1024))
	signupAs(a, "mcap4@test.io", "gk-code-1")

	body, ct := multipartBody(t, 0, 0, "exact.bin", bytes.Repeat([]byte("z"), 1024))
	code, errCode := postBody(t, a, body, ct)
	if code != http.StatusCreated {
		t.Fatalf("exact-size file: %d %s, want 201", code, errCode)
	}
}

// The byte budget alone would still admit ~10^6 tiny parts of iteration
// waste; the part counter refuses at uploadMaxParts.
func TestTooManyPartsRejected(t *testing.T) {
	a, _ := pwTestApp(t, capCfg(t, 1<<20))
	signupAs(a, "mcap5@test.io", "gk-code-1")

	body, ct := multipartBody(t, 65, 4, "ok.txt", []byte("hi"))
	code, errCode := postBody(t, a, body, ct)
	if code != http.StatusBadRequest || errCode != "too_many_parts" {
		t.Fatalf("65 junk parts: %d %s, want 400 too_many_parts", code, errCode)
	}
}
