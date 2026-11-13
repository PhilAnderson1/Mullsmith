package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWebFetchContentTypesAndPreservation(t *testing.T) {
	// Indentation, tabs, blank lines, and spaces within values must survive.
	body := " \n\tfirst:  value\n    second: value\n\n\n"
	for _, ct := range []string{
		"text/plain", "text/markdown", "text/csv", "text/x-python",
		"application/json", "Application/JSON; charset=utf-8",
		"application/problem+json", "application/xml", "application/rss+xml",
		"application/atom+xml", "application/yaml", "application/x-yaml",
		"application/javascript", "application/x-www-form-urlencoded",
		"", "application/octet-stream",
	} {
		t.Run(ct, func(t *testing.T) {
			result, err := fetchContent(t, ct, body)
			if err != nil {
				t.Fatal(err)
			}
			if result != body {
				t.Fatalf("content altered: %q", result)
			}
		})
	}
}

func TestWebFetchRejectsBinaryContent(t *testing.T) {
	for _, tc := range []struct{ name, ct, body string }{
		{"pdf", "application/pdf", "%PDF-1.7"},
		{"image", "image/png", "ascii only"},
		{"svg", "image/svg+xml", "<svg></svg>"},
		{"archive", "application/zip", "PK"},
		{"audio", "audio/mpeg", "ascii"},
		{"video", "video/mp4", "ascii"},
		{"nul", "application/octet-stream", "abc\x00def"},
		{"control", "", "abc\x01def"},
		{"escape", "text/plain", "\x1b[31mhello"},
		{"invalid UTF8", "application/json", "\xff"},
		{"invalid HTML UTF8", "text/html", "<p>\xff</p>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetchContent(t, tc.ct, tc.body)
			if err == nil || !strings.Contains(err.Error(), "web_download") {
				t.Fatalf("expected download guidance, got %v", err)
			}
		})
	}
}

func TestWebFetchHTMLDetection(t *testing.T) {
	body := "<html><body><p>Hello</p></body></html>"
	for _, ct := range []string{"text/html; charset=utf-8", "application/xhtml+xml", "", "application/octet-stream"} {
		t.Run(ct, func(t *testing.T) {
			text, err := fetchContent(t, ct, body)
			if err != nil || text != "Hello" {
				t.Fatalf("text=%q err=%v", text, err)
			}
		})
	}
	// Literal HTML in declared plain text must stay intact.
	text, err := fetchContent(t, "text/plain", body)
	if err != nil || text != body {
		t.Fatalf("plain text altered: %q %v", text, err)
	}
	text, err = fetchContent(t, "text/plain", "")
	if err != nil || text != "" {
		t.Fatalf("empty text altered: %q %v", text, err)
	}
}

func fetchContent(t *testing.T, ct, body string) (string, error) {
	t.Helper()
	client := &http.Client{Transport: egressRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}},
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	res, err := NewWebFetch(client, 64<<10).Execute(context.Background(), Invocation{Arguments: `{"url":"https://example.com/data"}`})
	if err != nil {
		return "", err
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Text, nil
}
