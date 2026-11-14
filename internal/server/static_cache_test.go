package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticResponsesCacheOnlySuccess(t *testing.T) {
	for _, version := range []string{"", "build123"} {
		s := newPreparedServer(t, testCfg(), testStore(t), discardLogger(), version, nil, nil, "abc1234")
		for _, tc := range []struct {
			method, path, byteRange string
			status                  int
			policy                  string
		}{
			{"GET", "/assets/app/app.js", "", 200, "public, max-age=3600"},
			{"HEAD", "/assets/styles.css", "", 200, "public, max-age=3600"},
			{"GET", "/assets/styles.css", "bytes=0-9", 206, "public, max-age=3600"},
			{"GET", "/assets/styles.css", "bytes=999999999-", 416, "no-store"},
			{"GET", s.assets.url("/assets/app/app.js"), "", 200, "public, max-age=2592000, immutable"},
			{"HEAD", s.assets.url("/assets/styles.css"), "", 200, "public, max-age=2592000, immutable"},
			{"GET", s.assets.url("/assets/styles.css"), "bytes=0-9", 206, "public, max-age=2592000, immutable"},
			{"GET", s.assets.url("/assets/styles.css"), "bytes=999999999-", 416, "no-store"},
			{"GET", "/assets/app/app.js?v=old-commit", "", 404, "no-store"},
			{"GET", "/assets/styles.css?v=", "", 404, "no-store"},
			{"GET", s.assets.url("/assets/styles.css") + "&v=other", "", 404, "no-store"},
			{"GET", "/assets/vendor/katex-0.19.1/katex.mjs", "", 200, "public, max-age=2592000, immutable"},
			{"GET", "/assets/vendor/katex-0.19.1/katex.min.css", "", 200, "public, max-age=2592000, immutable"},
			{"GET", "/assets/vendor/katex-0.19.1/fonts/KaTeX_Main-Regular.woff2", "", 200, "public, max-age=2592000, immutable"},
			{"GET", "/assets/vendor/katex-0.19.1/missing.js", "", 404, "no-store"},
			{"GET", "/assets/manifest-v4.webmanifest", "", 200, "no-cache"},
			{"GET", "/assets/missing.webmanifest", "", 404, "no-store"},
			{"GET", "/assets/missing.js", "", 404, "no-store"},
			{"GET", "/assets/app/", "", 404, "no-store"},
			{"GET", "/assets", "", 307, "no-store"},
			{"GET", "/api/v1/auth/me", "", 401, "no-store"},
		} {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Range", tc.byteRange)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != tc.policy {
				t.Errorf("version=%q %s %s range=%q: status=%d cache=%q", version, tc.method, tc.path, tc.byteRange, w.Code, w.Header().Get("Cache-Control"))
			}
		}
	}
}

func TestAssetOverloadIsNotCacheable(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := securityHeaders(testCfg())(limitRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}), 1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	}()
	defer func() { close(release); <-done }()
	<-entered
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app/app.js", nil))
	if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("overload status=%d headers=%v", w.Code, w.Header())
	}
}
