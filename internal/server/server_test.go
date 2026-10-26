package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"gabkeep/internal/config"
	"gabkeep/internal/store"
)

func testCfg() *config.Config {
	return &config.Config{
		Server: config.Server{
			Listen:    "127.0.0.1:0",
			PublicURL: "http://127.0.0.1:0",
		},
		LLM:  config.LLM{Models: []string{"m1"}},
		Auth: config.Auth{Registration: "invite-only"},
		// config.Load applies these defaults; struct-literal test configs
		// must set them too or bodyLimit rejects every POST (max=0).
		Limits: config.Limits{MaxBodyBytes: 1 << 20, MaxMessageChars: 32000, MaxAttachments: 8},
	}
}

func testStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	ts, _ := newTestServerWithDB(t, cfg)
	return ts
}

func newTestServerWithDB(t *testing.T, cfg *config.Config) (*httptest.Server, *store.DB) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	return httptest.NewServer(New(cfg, db, logger, "test", nil).Handler()), db
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
	// The SPA loads as an external ES module and carries no inline scripts
	// or handlers — CSP is script-src 'self' (spec §9.8). The URL may carry
	// the ?v= build stamp (TestIndexAssetURLsAreVersioned).
	html := string(body)
	if !strings.Contains(html, `type="module" src="/assets/app/app.js`) {
		t.Error("index missing module entrypoint")
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, " onclick=") {
		t.Error("index contains inline script/handler violating CSP")
	}
}

func TestUIAssetsServed(t *testing.T) {
	ts := newTestServer(t, testCfg())
	defer ts.Close()
	for _, tc := range []struct{ path, marker string }{
		{"/assets/styles.css", "--thread-max"},
		{"/assets/app/api.js", "X-Gabkeep-CSRF"},
		{"/assets/app/app.js", "api.listConversations"},
		{"/assets/app/sse.js", "event:"},
		{"/assets/app/markdown.js", "renderMarkdown"},
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: status %d", tc.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/") {
			t.Errorf("%s: Content-Type = %q", tc.path, ct)
		}
		if !strings.Contains(string(body), tc.marker) {
			t.Errorf("%s: marker %q missing", tc.path, tc.marker)
		}
	}
	// Path traversal must not escape the embed (spec §9.2).
	resp, err := http.Get(ts.URL + "/assets/../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Error("traversal served a file")
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
	resolve := func(nets []netip.Prefix, xff ...string) string {
		cfg := testCfg()
		cfg.TrustedNets = nets
		var got string
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
		})
		h = securityHeaders(cfg)(h)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		for _, v := range xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	// No trusted proxies: XFF is ignored entirely (spoofing, spec §9.10).
	if got := resolve(nil, "9.9.9.9"); got != "127.0.0.1" {
		t.Errorf("untrusted peer: ClientIP = %q, want 127.0.0.1", got)
	}

	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	// Trusted proxy passes a public client (untrusted) — resolves to it.
	if got := resolve(trusted, "203.0.113.7, 127.0.0.1"); got != "203.0.113.7" {
		t.Errorf("trusted proxy: ClientIP = %q, want 203.0.113.7", got)
	}

	// Audit finding 2: an appending proxy puts the REAL client rightmost,
	// after the attacker's forged prefix. Right-to-left must answer with
	// the real client, never the forged one.
	if got := resolve(trusted, "1.2.3.4, 198.51.100.9"); got != "198.51.100.9" {
		t.Errorf("spoofed prefix: ClientIP = %q, want 198.51.100.9 (the appended real client)", got)
	}

	// Interior trusted hops are skipped over, right to left.
	nets2 := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8")}
	if got := resolve(nets2, "203.0.113.9, 10.0.0.5, 127.0.0.1"); got != "203.0.113.9" {
		t.Errorf("trusted interior hop: ClientIP = %q, want 203.0.113.9", got)
	}

	// Every hop trusted ⇒ only the peer is a defensible answer.
	if got := resolve(nets2, "127.0.0.1, 10.0.0.1"); got != "127.0.0.1" {
		t.Errorf("all-trusted chain: ClientIP = %q, want peer 127.0.0.1", got)
	}

	// Junk at the observed (right) end: the chain was not completed by the
	// proxy, so nothing left of the junk is trustworthy — peer wins even
	// though 203.0.113.9 parses.
	if got := resolve(trusted, "203.0.113.9, evil"); got != "127.0.0.1" {
		t.Errorf("malformed right end: ClientIP = %q, want peer 127.0.0.1", got)
	}

	// Junk left of the real client is harmless: the walk resolves before
	// reaching it.
	if got := resolve(trusted, "garbage, nonsense, 203.0.113.5"); got != "203.0.113.5" {
		t.Errorf("junk left of client: ClientIP = %q, want 203.0.113.5", got)
	}

	// Over-long chains are junk by definition ⇒ peer.
	long := strings.Repeat("1.1.1.1, ", maxXFFHops) + "203.0.113.2"
	if got := resolve(trusted, long); got != "127.0.0.1" {
		t.Errorf("over-long chain: ClientIP = %q, want peer 127.0.0.1", got)
	}

	// Repeated header lines are joined in wire order; the proxy appended
	// the real client to the last line, so the forged first line loses.
	if got := resolve(trusted, "9.9.9.9", "198.51.100.7"); got != "198.51.100.7" {
		t.Errorf("two header lines: ClientIP = %q, want 198.51.100.7", got)
	}

	// IPv4-mapped IPv6 is reported as plain IPv4.
	if got := resolve(trusted, "::ffff:203.0.113.7"); got != "203.0.113.7" {
		t.Errorf("mapped v6: ClientIP = %q, want 203.0.113.7", got)
	}

	// No header at all ⇒ peer.
	if got := resolve(trusted); got != "127.0.0.1" {
		t.Errorf("no XFF: ClientIP = %q, want peer 127.0.0.1", got)
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

// The app shell is no-store while assets cache for an hour, so a deploy
// without URL versioning leaves browsers running the PREVIOUS app.js until
// expiry — production proved it: a Files-page fix was invisible to a
// fresh page load for an hour after rollout. Every build (version change)
// must therefore rotate the asset URLs.
func TestIndexAssetURLsAreVersioned(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(testCfg(), testStore(t), logger, "abc123", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`/assets/app/app.js?v=abc123"`,
		`/assets/styles.css?v=abc123"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served index lacks %q", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index Content-Type = %q", ct)
	}
	// The stamp rides ldflags (operator input): anything that could break
	// out of the URL attribute is stripped.
	v := string(versionedIndex(`1.0 "evil`))
	if strings.Contains(v, `%22`) || strings.Contains(v, `"evil`) ||
		!strings.Contains(v, `app.js?v=1.0evil"`) {
		t.Errorf("version not URL-safe: %q", v)
	}
	// No version → shell served unmodified (dev builds, tests).
	if raw := string(versionedIndex("")); !strings.Contains(raw, `src="/assets/app/app.js"`) {
		t.Error("empty version altered the shell")
	}
}
