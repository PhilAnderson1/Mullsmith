package server

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mullsmith/web"
)

func TestIndexAssetURLsUseCommitStamps(t *testing.T) {
	for _, tc := range []struct{ commit, stamp string }{
		{"abc1234", "abc1234"},
		{"c0c8c0fad759cc541237fda9bbb70d5749aa0afb", "c0c8c0f"},
		{"dev", ""}, {"none", ""}, {"", ""},
		{`abc1234"evil`, ""},
	} {
		t.Run(tc.commit, func(t *testing.T) {
			s := newPreparedServer(t, testCfg(), testStore(t), discardLogger(), "same-version", nil, nil, tc.commit)
			if s.assets.stamp != tc.stamp {
				t.Fatalf("stamp = %q, want %q", s.assets.stamp, tc.stamp)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("GET /: status=%d headers=%v", rec.Code, rec.Header())
			}
			for _, path := range []string{"/assets/app/app.js", "/assets/styles.css"} {
				url := path
				policy := "public, max-age=3600"
				if tc.stamp != "" {
					url += "?v=" + tc.stamp
					policy = "public, max-age=2592000, immutable"
				}
				if !strings.Contains(rec.Body.String(), `"`+url+`"`) {
					t.Errorf("shell lacks %q", url)
				}
				asset := httptest.NewRecorder()
				s.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, url, nil))
				if asset.Code != http.StatusOK || asset.Header().Get("Cache-Control") != policy {
					t.Errorf("GET %s: status=%d headers=%v", url, asset.Code, asset.Header())
				}
			}
		})
	}
}

func TestCommitStampedModuleImports(t *testing.T) {
	s := newPreparedServer(t, testCfg(), testStore(t), discardLogger(), "same-version", nil, nil, "abc1234")
	paths, err := fs.Glob(web.FS, "assets/app/*.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(s.assets.modules) {
		t.Fatal("not all application modules are covered")
	}
	for _, path := range paths {
		raw, err := web.FS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.assets.url("/"+path), nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=2592000, immutable" ||
			!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
			t.Fatalf("GET %s: status=%d headers=%v", path, rec.Code, rec.Header())
		}
		for _, dependency := range paths {
			url := "/" + dependency
			if !bytes.Contains(raw, []byte(`"`+url+`"`)) {
				continue
			}
			if !strings.Contains(rec.Body.String(), `"`+s.assets.url(url)+`"`) ||
				strings.Contains(rec.Body.String(), `"`+url+`"`) {
				t.Errorf("%s has an unstamped import of %s", path, dependency)
			}
		}
	}
}
