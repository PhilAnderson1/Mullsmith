package server

import (
	"fmt"
	"io/fs"
	"strings"
)

// The short commit ID versions the complete module graph and stylesheet.
// Commit source changes before rebuilding to rotate browser cache keys.
type assetBundle struct {
	stamp   string
	modules map[string][]byte
	index   []byte
}

func newAssetBundle(source fs.FS, commit string) (*assetBundle, error) {
	paths, err := fs.Glob(source, "assets/app/*.js")
	if err != nil {
		return nil, fmt.Errorf("load application module graph: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("load application module graph: no modules found")
	}
	bundle := &assetBundle{modules: make(map[string][]byte, len(paths))}
	if len(commit) >= 7 && len(commit) <= 40 && strings.Trim(commit, "0123456789abcdef") == "" {
		bundle.stamp = commit[:7]
	}
	for _, path := range paths {
		raw, err := fs.ReadFile(source, path)
		if err != nil {
			return nil, fmt.Errorf("load asset %s: %w", path, err)
		}
		body := string(raw)
		for _, dependency := range paths {
			url := "/" + dependency
			body = strings.ReplaceAll(body, `"`+url+`"`, `"`+bundle.url(url)+`"`)
		}
		bundle.modules["/"+path] = []byte(body)
	}
	index, err := fs.ReadFile(source, "index.html")
	if err != nil {
		return nil, fmt.Errorf("load application shell: %w", err)
	}
	html := string(index)
	for _, path := range []string{"/assets/app/app.js", "/assets/styles.css"} {
		html = strings.ReplaceAll(html, `"`+path+`"`, `"`+bundle.url(path)+`"`)
	}
	bundle.index = []byte(html)
	return bundle, nil
}

func (b *assetBundle) url(path string) string {
	if b.stamp == "" {
		return path
	}
	return path + "?v=" + b.stamp
}
