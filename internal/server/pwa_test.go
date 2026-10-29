package server

import (
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gabkeep/web"
)

// The PWA manifest is what turns "Add to home screen" into a standalone
// install (§10.2): if the link rots, the type is wrong, or an icon goes
// missing, browsers silently offer nothing.

func TestManifestServedAsJSONWithUsableIcons(t *testing.T) {
	cfg := chatCfg(t, "http://127.0.0.1:1/v1") // endpoint unused: nothing chats here
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, testStore(t), logger, "test", nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/assets/manifest-v4.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("Content-Type = %q, want application/manifest+json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache (stale manifests keep serving old icons)", cc)
	}

	var m struct {
		Name     string `json:"name"`
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
			Type  string `json:"type"`
		} `json:"icons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.Name == "" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" {
		t.Errorf("manifest fields wrong: %+v", m)
	}
	biggest := 0
	for _, ic := range m.Icons {
		src, _, _ := strings.Cut(ic.Src, "?") // icons are cache-busted by query
		b, err := web.FS.ReadFile(strings.TrimPrefix(src, "/"))
		if err != nil {
			t.Errorf("manifest icon %q not embedded: %v", ic.Src, err)
			continue
		}
		if ic.Type == "image/png" {
			cfgImg, err := png.DecodeConfig(strings.NewReader(string(b)))
			if err != nil {
				t.Errorf("icon %q does not decode: %v", ic.Src, err)
				continue
			}
			if cfgImg.Width != cfgImg.Height {
				t.Errorf("icon %q is not square: %dx%d", ic.Src, cfgImg.Width, cfgImg.Height)
			}
			if cfgImg.Width > biggest {
				biggest = cfgImg.Width
			}
		}
	}
	// Chrome wants at least one raster ≥ 192 px (or an SVG with sizes "any").
	if biggest < 192 {
		t.Errorf("no PNG icon ≥ 192 px declared (largest %d)", biggest)
	}
}

func TestIndexLinksManifestAndAppleTags(t *testing.T) {
	b, err := web.FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(b)
	for _, want := range []string{
		`rel="manifest" href="/assets/manifest-v4.webmanifest"`,
		`rel="apple-touch-icon" href="/assets/gk-apple-full-v4.png"`,
		`name="theme-color"`,
		`media="(prefers-color-scheme: dark)"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html lost %s", want)
		}
	}
}
