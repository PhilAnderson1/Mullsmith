package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyContextReachesAccessLog(t *testing.T) {
	cfg := testCfg()
	cfg.Server.TrustedProxies = []string{"127.0.0.1/32"}
	var logs bytes.Buffer
	s := newPreparedServer(t, cfg, testStore(t), slog.New(slog.NewJSONHandler(&logs, nil)), "test", nil, nil)
	logs.Reset()
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	req.Header.Set("X-Forwarded-Proto", "https")
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	var record struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.IP != "203.0.113.42" {
		t.Fatalf("logged proxy instead of client: %+v", record)
	}
}

func TestTrustedProxyRequiresOneProtocolValue(t *testing.T) {
	cfg := testCfg()
	cfg.Server.TrustedProxies = []string{"127.0.0.1/32"}
	s := newPreparedServer(t, cfg, testStore(t), discardLogger(), "test", nil, nil)
	for _, tc := range []struct {
		name   string
		values []string
		peer   string
		want   int
	}{
		{"missing", nil, "127.0.0.1:1", 400},
		{"https", []string{"https"}, "127.0.0.1:1", 200},
		{"http", []string{"http"}, "127.0.0.1:1", 200},
		{"prefix", []string{"http, https"}, "127.0.0.1:1", 400},
		{"suffix", []string{"https, http"}, "127.0.0.1:1", 400},
		{"repeated", []string{"https", "https"}, "127.0.0.1:1", 400},
		{"unknown", []string{"ftp"}, "127.0.0.1:1", 400},
		{"empty", []string{""}, "127.0.0.1:1", 400},
		{"untrusted ignored", []string{"ftp, https"}, "192.0.2.1:1", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/healthz", nil)
			req.RemoteAddr = tc.peer
			for _, value := range tc.values {
				req.Header.Add("X-Forwarded-Proto", value)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
		})
	}
}

func TestFinalStatusTracksWritesAndInformationalResponses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(http.ResponseWriter)
		want  int
	}{
		{"implicit", func(w http.ResponseWriter) { io.WriteString(w, "ok") }, 200},
		{"informational", func(w http.ResponseWriter) { w.WriteHeader(103); io.WriteString(w, "ok") }, 200},
		{"duplicate", func(w http.ResponseWriter) { w.WriteHeader(201); w.WriteHeader(500) }, 201},
		{"flush", func(w http.ResponseWriter) { w.(http.Flusher).Flush(); w.WriteHeader(500) }, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type snapshot struct {
				status    int
				committed bool
			}
			snapshots := make(chan snapshot, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tracked := &statusWriter{ResponseWriter: w, status: 200}
				defer func() { snapshots <- snapshot{tracked.status, tracked.committed} }()
				tc.write(tracked)
			}))
			defer ts.Close()
			resp, err := http.Get(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			got := <-snapshots
			if got.status != tc.want || resp.StatusCode != tc.want || !got.committed {
				t.Fatalf("status=%d wire=%d committed=%v", got.status, resp.StatusCode, got.committed)
			}
		})
	}
}

func TestPanicAfterFlushAbortsResponse(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	done := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, "partial payload")
		w.(http.Flusher).Flush()
		panic("private failure")
	})
	chain := logRequests(recoverPanics(h, logger), logger)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); chain.ServeHTTP(w, r) }))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	<-done
	if err == nil || string(body) != "partial payload" {
		t.Fatalf("failed response ended cleanly or appended data: %q %v", body, err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("committed status changed: %d", resp.StatusCode)
	}
	// The network error follows the deferred access log, before net/http closes.
	if !strings.Contains(logs.String(), `"aborted":true`) || !strings.Contains(logs.String(), `"status":200`) {
		t.Fatalf("missing abort outcome: %s", logs.String())
	}
}

func TestPanicBeforeCommitClearsStaleBodyHeaders(t *testing.T) {
	h := recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000")
		w.Header().Set("Content-Encoding", "gzip")
		panic("private failure")
	}), discardLogger())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || rec.Header().Get("Content-Length") != "" || rec.Header().Get("Content-Encoding") != "" || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("invalid error response: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestIntentionalAbortIsRethrown(t *testing.T) {
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("abort converted to a response: %v", got)
		}
	}()
	h := recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }), discardLogger())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestRouterDerivesMethodsAndPreservesRedirects(t *testing.T) {
	s := newPreparedServer(t, testCfg(), testStore(t), discardLogger(), "test", nil, nil)
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"DELETE", "/", 405, "GET, HEAD"},
		{"GET", "/api/v1/auth/login", 405, "POST"},
		{"POST", "/healthz", 405, "GET, HEAD"},
		{"GET", "/api/v1/conversations/abc/messages", 401, ""},
		{"GET", "/missing", 404, ""},
		{"GET", "/missing//path", 307, ""},
		{"HEAD", "/", 200, ""},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status || rec.Header().Get("Allow") != tc.allow {
				t.Fatalf("status=%d allow=%q body=%s", rec.Code, rec.Header().Get("Allow"), rec.Body)
			}
			if tc.status == 404 || tc.status == 405 {
				if !json.Valid(rec.Body.Bytes()) {
					t.Fatalf("not a JSON error: %s", rec.Body)
				}
			}
			if tc.status == 307 && (rec.Header().Get("Location") != "/missing/path" || strings.Contains(rec.Body.String(), `"not_found"`)) {
				t.Fatalf("canonical redirect replaced: %v %s", rec.Header(), rec.Body)
			}
		})
	}
}
