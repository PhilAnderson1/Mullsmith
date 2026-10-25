package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"gabkeep/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		Server: config.Server{
			Listen:    "127.0.0.1:0",
			PublicURL: "http://127.0.0.1:0",
		},
		LLM:  config.LLM{Models: []string{"m1"}},
		Auth: config.Auth{Registration: "invite-only"},
	}
}

func newTestServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httptest.NewServer(New(cfg, logger, "test").Handler())
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, testCfg())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	// health output must carry no version/db info (spec §5.4)
	if strings.Contains(string(body), "gabkeep") {
		t.Error("health body leaks app name")
	}
}

func TestSecurityHeadersAlwaysPresent(t *testing.T) {
	ts := newTestServer(t, testCfg())
	defer ts.Close()

	for _, path := range []string{"/", "/healthz", "/api/v1/meta", "/nope/missing", "/assets/"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s: CSP missing directives: %q", path, csp)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: nosniff missing", path)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", path, got)
		}
		if got := resp.Header.Get("X-Request-Id"); len(got) != 16 {
			t.Errorf("%s: X-Request-Id = %q", path, got)
		}
		if strings.Contains(path, "/api/") || path == "/healthz" || path == "/" {
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
			}
		}
	}
}

func TestTLSConfigAddsHSTSAndUpgrade(t *testing.T) {
	cfg := testCfg()
	cfg.Server.TLS = config.TLS{Enabled: true, CertFile: "c", KeyFile: "k"}
	cfg.Server.HSTSMaxAge = 31536000
	ts := newTestServer(t, cfg)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=31536000") {
		t.Errorf("HSTS = %q", got)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "upgrade-insecure-requests") {
		t.Error("CSP missing upgrade-insecure-requests under TLS")
	}
}

func TestMetaStub(t *testing.T) {
	ts := newTestServer(t, testCfg())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var meta map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta["app"] != "gabkeep" || meta["registration"] != "invite-only" {
		t.Errorf("meta = %v", meta)
	}
	if _, leaked := meta["api_key"]; leaked {
		t.Error("meta must not expose api_key")
	}
}

func TestIndexServed(t *testing.T) {
	ts := newTestServer(t, testCfg())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Gabkeep") {
		t.Errorf("index: status %d, body missing brand", resp.StatusCode)
	}
}

func TestNoDirectoryListing(t *testing.T) {
	ts := newTestServer(t, testCfg())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/assets/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /assets/ status = %d, want 404", resp.StatusCode)
	}
	var env map[string]map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("404 body not JSON envelope: %v", err)
	}
	if env["error"]["code"] != "not_found" {
		t.Errorf("error envelope = %v", env["error"])
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, testCfg())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/healthz", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestClientIPTrustedProxies(t *testing.T) {
	// Helper to run ClientIP behind the securityHeaders middleware (which
	// injects trusted nets into the context).
	resolve := func(nets []netip.Prefix, xff string) string {
		cfg := testCfg()
		cfg.TrustedNets = nets
		var got string
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
		})
		h = securityHeaders(cfg)(h)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	// No trusted proxies: XFF is ignored entirely (spoofing, spec §9.10).
	if got := resolve(nil, "9.9.9.9"); got != "127.0.0.1" {
		t.Errorf("untrusted peer: ClientIP = %q, want 127.0.0.1", got)
	}

	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	// Trusted proxy passes a public client: leftmost untrusted hop wins.
	if got := resolve(trusted, "203.0.113.7, 127.0.0.1"); got != "203.0.113.7" {
		t.Errorf("trusted proxy: ClientIP = %q, want 203.0.113.7", got)
	}
}

func TestRecoverPanics(t *testing.T) {
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h = recoverPanics(h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic detail leaked to client")
	}
}

func TestBodyLimit(t *testing.T) {
	var readErr error
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	})
	h = bodyLimit(h, 8)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789abcdef"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !IsBodyTooLarge(readErr) {
		t.Errorf("expected MaxBytesError, got %v", readErr)
	}
}
