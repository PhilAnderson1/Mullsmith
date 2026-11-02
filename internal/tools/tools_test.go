package tools

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- registry ----------------------------------------------------------------

func TestRegistryPolicyFailClosed(t *testing.T) {
	reg := NewRegistry(func(name string) string {
		if name == "web_search" {
			return "auto"
		}
		return "weird-value" // misconfiguration
	})
	if got := reg.Policy("web_search"); got != ApprovalAuto {
		t.Errorf("web_search policy = %q, want auto", got)
	}
	for _, name := range []string{"code_run", "unknown_tool", "weird-value"} {
		if got := reg.Policy(name); got != ApprovalConfirm {
			t.Errorf("Policy(%q) = %q, want confirm (fail closed)", name, got)
		}
	}
	var nilPolicy = NewRegistry(nil)
	if nilPolicy.Policy("anything") != ApprovalConfirm {
		t.Error("nil policy fn must default to confirm")
	}
}

type stubTool struct{ name string }

func (s stubTool) Name() string               { return s.name }
func (s stubTool) Description() string        { return "stub" }
func (s stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Execute(context.Context, Invocation) (Result, error) {
	return Result{Content: "ok"}, nil
}

func TestRegistryOrderAndDedupe(t *testing.T) {
	reg := NewRegistry(nil)
	reg.Register(stubTool{"a"})
	reg.Register(stubTool{"b"})
	reg.Register(stubTool{"a"}) // duplicate loses
	specs := reg.Specs()
	if len(specs) != 2 || specs[0].Function.Name != "a" || specs[1].Function.Name != "b" {
		t.Fatalf("specs = %+v, want [a b] in registration order", specs)
	}
	if _, ok := reg.Get("missing"); ok {
		t.Error("Get(missing) = true")
	}
}

// --- web_search (Brave) --------------------------------------------------------

func TestBraveSearchHappyPath(t *testing.T) {
	var gotPath, gotToken, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Subscription-Token")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"web":{"results":[
			{"title":"Kyoto Guide","url":"https://example.com/kyoto","description":"Autumn <strong>maps</strong>.","age":"2 days ago"},
			{"title":"No URL","url":"","description":"skipped"},
			{"title":"Second","url":"https://example.org/t","description":"Snippets"}
		]}}`))
	}))
	defer srv.Close()

	b := NewBraveSearch("secret-key-9", 8, srv.Client())
	b.endpoint = srv.URL + "/res/v1/web/search"
	res, err := b.Execute(context.Background(), Invocation{Arguments: `{"query":"kyoto autumn"}`})
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "secret-key-9" {
		t.Errorf("X-Subscription-Token = %q", gotToken)
	}
	if gotPath != "/res/v1/web/search" || gotAccept != "application/json" {
		t.Errorf("path=%q accept=%q", gotPath, gotAccept)
	}
	if strings.Contains(res.Content, "secret-key-9") {
		t.Error("API key leaked into model-facing content")
	}
	var payload struct {
		Results []searchResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatalf("content not JSON: %v", err)
	}
	if len(payload.Results) != 2 { // empty-URL row dropped
		t.Fatalf("results = %d, want 2", len(payload.Results))
	}
	if payload.Results[0].Title != "Kyoto Guide" || payload.Results[1].URL != "https://example.org/t" {
		t.Errorf("results = %+v", payload.Results)
	}
	if payload.Results[0].Snippet != "Autumn maps." { // <strong> markup stripped
		t.Errorf("snippet = %q", payload.Results[0].Snippet)
	}
	if !strings.Contains(res.Summary, "kyoto autumn") || !strings.Contains(res.Summary, "2 result") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestBraveSearchLegacyOriginalShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"web":{"original":[
			{"title":"Old form","url":"https://example.net/x","description":"still works"}
		]}}`))
	}))
	defer srv.Close()
	b := NewBraveSearch("k", 8, srv.Client())
	b.endpoint = srv.URL
	res, err := b.Execute(context.Background(), Invocation{Arguments: `{"query":"x"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "https://example.net/x") {
		t.Errorf("legacy original[] not parsed: %s", res.Content)
	}
}

func TestBraveSearchErrors(t *testing.T) {
	b := NewBraveSearch("k", 8, nil)
	if _, err := b.Execute(context.Background(), Invocation{Arguments: `{not json`}); err == nil {
		t.Error("invalid JSON accepted")
	}
	if _, err := b.Execute(context.Background(), Invocation{Arguments: `{"query":"   "}`}); err == nil {
		t.Error("blank query accepted")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	b2 := NewBraveSearch("k", 8, srv.Client())
	b2.endpoint = srv.URL
	if _, err := b2.Execute(context.Background(), Invocation{Arguments: `{"query":"x"}`}); err == nil {
		t.Error("HTTP 429 accepted")
	}
}

func TestBraveSearchKeyNotInURL(t *testing.T) {
	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	b := NewBraveSearch("SUP3R-SECRET", 5, srv.Client())
	b.endpoint = srv.URL
	if _, err := b.Execute(context.Background(), Invocation{Arguments: `{"query":"q"}`}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawQuery, "SUP3R-SECRET") {
		t.Fatal("API key travelled in the URL (§9.6 violation)")
	}
}

// --- web_fetch ----------------------------------------------------------------

func TestWebFetchStripsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>Doc &amp; More</title>
<script>alert('xss')</script><style>body{}</style></head>
<body><h1>Hello</h1><p>First para.</p><p>Second &lt;para&gt;</p><!-- c --></body></html>`))
	}))
	defer srv.Close()

	w := NewWebFetch(NewGuardedClient(5*time.Second), 1<<20)
	// The guarded client refuses loopback, so inject a permissive client
	// for the functional test; the SSRF behaviour is covered in egress_test.
	w.client = srv.Client()
	res, err := w.Execute(context.Background(), Invocation{Arguments: `{"url":"` + srv.URL + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != "Doc & More" {
		t.Errorf("title = %q", payload.Title)
	}
	for _, bad := range []string{"alert", "body{}", "<h1>", "<p>"} {
		if strings.Contains(payload.Text, bad) {
			t.Errorf("text still contains %q: %q", bad, payload.Text)
		}
	}
	for _, good := range []string{"Hello", "First para.", "Second <para>"} {
		if !strings.Contains(payload.Text, good) {
			t.Errorf("text missing %q: %q", good, payload.Text)
		}
	}
}

func TestWebFetchRejectsBadSchemes(t *testing.T) {
	w := NewWebFetch(http.DefaultClient, 1<<20)
	for _, u := range []string{"file:///etc/passwd", "gopher://x/y", "ftp://x/"} {
		if _, err := w.Execute(context.Background(), Invocation{Arguments: `{"url":"` + u + `"}`}); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestWebFetchMaxBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer srv.Close()
	w := NewWebFetch(srv.Client(), 1024)
	res, err := w.Execute(context.Background(), Invocation{Arguments: `{"url":"` + srv.URL + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Truncated || len(payload.Text) > 1100 {
		t.Errorf("truncated=%v len=%d, want truncation at ~1 KiB", payload.Truncated, len(payload.Text))
	}
	if !strings.Contains(res.Summary, "truncated") {
		t.Errorf("summary should flag truncation: %q", res.Summary)
	}
}

func TestWebFetchBinaryRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0xff, 0xfe, 0x00, 0x01, 0x02, 0x03})
	}))
	defer srv.Close()
	w := NewWebFetch(srv.Client(), 1<<20)
	if _, err := w.Execute(context.Background(), Invocation{Arguments: `{"url":"` + srv.URL + `"}`}); err == nil {
		t.Error("binary body accepted as text")
	}
}

func TestWebFetchTruncatesAtUTF8Boundary(t *testing.T) {
	for _, suffix := range []string{"é", "€", "😀"} {
		t.Run(suffix, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Write([]byte(strings.Repeat("a", 1023) + suffix))
			}))
			defer srv.Close()
			res, err := NewWebFetch(srv.Client(), 1024).Execute(context.Background(), Invocation{Arguments: `{"url":"` + srv.URL + `"}`})
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Text      string `json:"text"`
				Truncated bool   `json:"truncated"`
			}
			if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Truncated || result.Text != strings.Repeat("a", 1023) {
				t.Fatalf("bad truncation: %+v", result)
			}
		})
	}
}

// --- budget helpers --------------------------------------------------------------

func TestTruncateMiddle(t *testing.T) {
	if got := TruncateMiddle("short", 100); got != "short" {
		t.Errorf("under budget modified: %q", got)
	}
	huge := strings.Repeat("a", 1000) + "NEEDLE" + strings.Repeat("b", 1000)
	got := TruncateMiddle(huge, 200)
	r := []rune(got)
	if len(r) > 200+64 { // head+tail+marker bound; marker adds known text
		t.Fatalf("output too long: %d runes", len(r))
	}
	if !strings.Contains(got, "elided") {
		t.Error("missing elision marker")
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "bbbb") {
		t.Error("head/tail not preserved")
	}
	if strings.Contains(got, "NEEDLE") {
		t.Error("middle survived")
	}
}

func TestElide(t *testing.T) {
	if got := Elide("  a \n b ", 10); got != "a b" {
		t.Errorf("Elide whitespace = %q", got)
	}
	if got := Elide("abcdefghij", 5); got != "abcd…" {
		t.Errorf("Elide cut = %q", got)
	}
}

// TestBraveSearchGzipResponse is the live-endpoint regression: Brave serves
// gzip, and Go only decodes it transparently while Accept-Encoding is
// client-managed. A raw-gzip body must still parse.
func TestBraveSearchGzipResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("client should advertise gzip by default: %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte(`{"web":{"results":[{"title":"Gz","url":"https://example.com/g","description":"works"}]}}`))
		gz.Close()
	}))
	defer srv.Close()
	b := NewBraveSearch("k", 8, srv.Client())
	b.endpoint = srv.URL
	res, err := b.Execute(context.Background(), Invocation{Arguments: `{"query":"x"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "https://example.com/g") {
		t.Errorf("gzip body not decoded: %s", res.Content)
	}
}
