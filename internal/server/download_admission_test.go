package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/config"
)

func downloadRequest(a *authTest, method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range a.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	return r
}

// The first write waits for release/cancellation, representing network
// backpressure without filling kernel buffers or depending on transfer speed.
type pausedDownloadResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	ctx     context.Context
	failure error
	once    sync.Once
}

func (w *pausedDownloadResponse) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.release:
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
	if w.failure != nil {
		return 0, w.failure
	}
	return w.ResponseRecorder.Write(p)
}

func holdDownload(t *testing.T, s *Server, a *authTest, ctx context.Context, failure error) func() {
	t.Helper()
	w := &pausedDownloadResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{}), ctx: ctx, failure: failure}
	r := downloadRequest(a, http.MethodGet, "/api/v1/files/download?path=uploads%2Ftest.txt").WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.Handler().ServeHTTP(w, r) }()
	var once sync.Once
	finish := func() {
		once.Do(func() { close(w.release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("download handler did not release")
		}
	}
	t.Cleanup(finish)
	select {
	case <-w.entered:
	case <-done:
		t.Fatalf("download did not reach write: %d %s", w.Code, w.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("download never entered")
	}
	return finish
}

func downloadFixture(t *testing.T, limit int) (*authTest, *Server, *config.Config) {
	t.Helper()
	cfg := scratchCfg(t)
	cfg.AppLimits.ConcurrentDownloadsPerUser = limit
	a, s := pwTestApp(t, cfg)
	signupAs(a, "downloads@example.test", "download-invite")
	seedFile(t, cfg, meID(a), "uploads/test.txt", []byte("owner bytes"))
	return a, s, cfg
}

func TestDownloadLimitIsPerUserAcrossSessions(t *testing.T) {
	a, s, cfg := downloadFixture(t, 2)
	sameOwner := newAuthTestOn(t, a.ts, a.db)
	r := sameOwner.do(http.MethodPost, "/api/v1/auth/login", map[string]any{"email": "downloads@example.test", "password": "passw0rd-long-enough"})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	other := newAuthTestOn(t, a.ts, a.db)
	signupAs(other, "other-downloads@example.test", "other-download-invite")
	seedFile(t, cfg, meID(other), "uploads/test.txt", []byte("other bytes"))
	first := holdDownload(t, s, a, context.Background(), nil)
	second := holdDownload(t, s, sameOwner, context.Background(), nil)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, downloadRequest(a, method, "/api/v1/files/download?path=uploads%2Ftest.txt"))
		if w.Code != 429 || w.Header().Get("Retry-After") == "" {
			t.Fatalf("excess %s=%d %s", method, w.Code, w.Body.String())
		}
		if method == http.MethodGet && !strings.Contains(w.Body.String(), "too_many_downloads") {
			t.Fatalf("wrong refusal: %s", w.Body.String())
		}
	}
	for _, tc := range []struct {
		client *authTest
		path   string
	}{{other, "/api/v1/files/download?path=uploads%2Ftest.txt"}, {other, "/api/v1/auth/me"}, {a, "/api/v1/auth/me"}, {other, "/healthz"}} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, downloadRequest(tc.client, http.MethodGet, tc.path))
		if w.Code != 200 {
			t.Fatalf("unrelated request %s=%d", tc.path, w.Code)
		}
		if strings.Contains(tc.path, "/download") && w.Body.String() != "other bytes" {
			t.Fatal("cross-user file exposure")
		}
	}
	first()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest(a, http.MethodGet, "/api/v1/files/download?path=uploads%2Ftest.txt"))
	if w.Code != 200 || w.Body.String() != "owner bytes" {
		t.Fatalf("released slot unavailable: %d %s", w.Code, w.Body.String())
	}
	second()
	assertNoTransferSlots(t, s.downloads)
}

func TestDownloadSlotsReleaseAfterErrorsAndCancellation(t *testing.T) {
	a, s, _ := downloadFixture(t, 1)
	for _, path := range []string{"", "?path=../outside", "?path=uploads%2Fmissing.txt", "?path=uploads"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, downloadRequest(a, http.MethodGet, "/api/v1/files/download"+path))
		if w.Code != 404 {
			t.Fatalf("invalid download=%d", w.Code)
		}
		assertNoTransferSlots(t, s.downloads)
	}
	for _, failure := range []error{io.ErrClosedPipe, os.ErrDeadlineExceeded} {
		finish := holdDownload(t, s, a, context.Background(), failure)
		finish()
		assertNoTransferSlots(t, s.downloads)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finish := holdDownload(t, s, a, ctx, nil)
	cancel()
	finish()
	assertNoTransferSlots(t, s.downloads)
	// HEAD follows the same admission/release path without a response body.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest(a, http.MethodHead, "/api/v1/files/download?path=uploads%2Ftest.txt"))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	assertNoTransferSlots(t, s.downloads)
}
