package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/auth"
)

func uploadRequest(t *testing.T, a *authTest) (*http.Request, []byte) {
	t.Helper()
	var payload bytes.Buffer
	mw := multipart.NewWriter(&payload)
	part, err := mw.CreateFormFile("file", "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "upload bytes"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/files", bytes.NewReader(payload.Bytes()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set(auth.CSRFHeaderName, a.csrf)
	for k, v := range a.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	return r, payload.Bytes()
}

type pausedUploadBody struct {
	io.Reader
	entered chan struct{}
	release chan struct{}
	ctx     context.Context
	once    sync.Once
}

func (b *pausedUploadBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	return b.Reader.Read(p)
}

func holdUpload(t *testing.T, s *Server, a *authTest, ctx context.Context) func() {
	t.Helper()
	r, payload := uploadRequest(t, a)
	r = r.WithContext(ctx)
	body := &pausedUploadBody{Reader: bytes.NewReader(payload), entered: make(chan struct{}), release: make(chan struct{}), ctx: ctx}
	r.Body = io.NopCloser(body)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.Handler().ServeHTTP(w, r) }()
	var once sync.Once
	finish := func() {
		once.Do(func() { close(body.release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("upload did not release its slots")
		}
	}
	t.Cleanup(finish)
	select {
	case <-body.entered:
	case <-done:
		t.Fatalf("upload not admitted: %d %s", w.Code, w.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("upload never entered")
	}
	return finish
}

func refuseUpload(t *testing.T, s *Server, a *authTest, code string) {
	t.Helper()
	r, payload := uploadRequest(t, a)
	body := &trackedRequestBody{Reader: bytes.NewReader(payload)}
	r.Body = io.NopCloser(body)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"`+code+`"`) || w.Header().Get("Retry-After") == "" || body.reads != 0 {
		t.Fatalf("upload refusal=%d reads=%d body=%s", w.Code, body.reads, w.Body.String())
	}
}

func TestUploadUserLimitPreservesGlobalCapacity(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.AppLimits.ConcurrentUploads = 8
	a, s := pwTestApp(t, cfg)
	clients := []*authTest{a}
	signupAs(a, "u0@uploads.test", "upload-0")
	for i := 1; i < 5; i++ {
		b := newAuthTestOn(t, a.ts, a.db)
		signupAs(b, fmt.Sprintf("u%d@uploads.test", i), fmt.Sprintf("upload-%d", i))
		clients = append(clients, b)
	}
	sameOwner := newAuthTestOn(t, a.ts, a.db)
	login := sameOwner.do(http.MethodPost, "/api/v1/auth/login", map[string]any{"email": "u0@uploads.test", "password": "passw0rd-long-enough"})
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatal(login.StatusCode)
	}
	finishes := []func(){holdUpload(t, s, a, context.Background()), holdUpload(t, s, sameOwner, context.Background())}
	refuseUpload(t, s, a, "too_many_uploads")
	if len(s.uploadSem) != 2 {
		t.Fatalf("per-user refusal spent global capacity: %d", len(s.uploadSem))
	}
	// Three other accounts can still each use their own two slots.
	for _, b := range clients[1:4] {
		finishes = append(finishes, holdUpload(t, s, b, context.Background()), holdUpload(t, s, b, context.Background()))
	}
	if len(s.uploadSem) != 8 {
		t.Fatalf("global slots=%d, want 8", len(s.uploadSem))
	}
	refuseUpload(t, s, clients[4], "server_busy")
	// A global refusal must refund the per-user slot it acquired first.
	s.uploads.mu.Lock()
	activeUsers := len(s.uploads.active)
	s.uploads.mu.Unlock()
	if activeUsers != 4 {
		t.Fatalf("global refusal retained user: %d active", activeUsers)
	}
	finishes[0]()
	r, _ := uploadRequest(t, clients[4])
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("freed global capacity unavailable: %d %s", w.Code, w.Body.String())
	}
	for _, finish := range finishes {
		finish()
	}
	assertNoTransferSlots(t, s.uploads)
	if len(s.uploadSem) != 0 {
		t.Fatalf("global slots leaked: %d", len(s.uploadSem))
	}
}

func TestUploadLimitsDisableIndependently(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		global, perUser, held int
		code                  string
	}{{"global disabled", -1, 1, 1, "too_many_uploads"}, {"user disabled", 3, -1, 3, "server_busy"}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := scratchCfg(t)
			cfg.AppLimits.ConcurrentUploads = tc.global
			cfg.AppLimits.ConcurrentUploadsPerUser = tc.perUser
			a, s := pwTestApp(t, cfg)
			signupAs(a, "disable@uploads.test", "disable-invite")
			var finishes []func()
			for i := 0; i < tc.held; i++ {
				finishes = append(finishes, holdUpload(t, s, a, context.Background()))
			}
			refuseUpload(t, s, a, tc.code)
			for _, finish := range finishes {
				finish()
			}
			assertNoTransferSlots(t, s.uploads)
			if len(s.uploadSem) != 0 {
				t.Fatal("global capacity leaked")
			}
		})
	}
}

func TestUploadSlotsReleaseOnMalformedBodyAndCancellation(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.AppLimits.ConcurrentUploads = 2
	cfg.AppLimits.ConcurrentUploadsPerUser = 1
	a, s := pwTestApp(t, cfg)
	signupAs(a, "errors@uploads.test", "errors-invite")
	r, _ := uploadRequest(t, a)
	r.Body = io.NopCloser(strings.NewReader("malformed multipart"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("malformed upload=%d", w.Code)
	}
	assertNoTransferSlots(t, s.uploads)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finish := holdUpload(t, s, a, ctx)
	cancel()
	finish()
	assertNoTransferSlots(t, s.uploads)
	if len(s.uploadSem) != 0 {
		t.Fatal("global capacity leaked")
	}
	r, _ = uploadRequest(t, a)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("retry=%d %s", w.Code, w.Body.String())
	}
	assertNoTransferSlots(t, s.uploads)
}
