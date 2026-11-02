package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/managed"
)

func downloadArgs(t *testing.T, url, path string) string {
	t.Helper()
	return argsJSON(t, map[string]string{"url": url, "path": path})
}

func assertNoDownloadTemps(t *testing.T, root string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".download-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary downloads remain: %v (%v)", matches, err)
	}
}

func TestWebDownloadBinaryAndNoOverwrite(t *testing.T) {
	root := toolRoot(t)
	data := []byte{0xff, 0xfe, 0x00, 0x01, 0x02}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/file", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	w := NewWebDownload(srv.Client(), 1024, managed.New(1024, 1000))
	res, err := run(t, w, root, downloadArgs(t, srv.URL+"/redirect", "work/file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Path        string `json:"path"`
		Bytes       int64  `json:"bytes"`
		ContentType string `json:"content_type"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Path != "work/file.bin" || payload.Bytes != int64(len(data)) || payload.ContentType != "application/octet-stream" || payload.URL != srv.URL+"/file" {
		t.Fatalf("unexpected result: %s", res.Content)
	}
	// An unrelated existing destination must survive a later download.
	writeDirect(t, root, "work/original.bin", "original")
	if _, err := run(t, w, root, downloadArgs(t, srv.URL+"/file", "work/original.bin")); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite error = %v", err)
	}
	for rel, want := range map[string]string{"work/file.bin": string(data), "work/original.bin": "original"} {
		got, err := os.ReadFile(filepath.Join(root, toolUser, rel))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v", rel, got, err)
		}
	}
	assertNoDownloadTemps(t, root)
}

func TestWebDownloadFailuresLeaveNoFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve http.HandlerFunc
		quota int64
		want  string
	}{
		{"declared size", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1025")
		}, 4096, "size limit"},
		{"streamed size", func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush() // no Content-Length
			_, _ = io.WriteString(w, strings.Repeat("x", 1025))
		}, 4096, "size limit"},
		{"incomplete body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "incomplete")
		}, 4096, "download failed"},
		{"http error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "missing", http.StatusNotFound)
		}, 4096, "HTTP 404"},
		{"partial response", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "partial")
		}, 4096, "HTTP 206"},
		{"quota", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", 100))
		}, 99, "quota"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := toolRoot(t)
			srv := httptest.NewServer(tc.serve)
			defer srv.Close()
			w := NewWebDownload(srv.Client(), 1024, managed.New(tc.quota, 1000))
			if _, err := run(t, w, root, downloadArgs(t, srv.URL, "artifacts/file.bin")); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if entries := listNames(t, filepath.Join(root, toolUser, "artifacts")); len(entries) != 0 {
				t.Fatalf("failed download saved files: %v", entries)
			}
			assertNoDownloadTemps(t, root)
		})
	}
}

func TestWebDownloadExactLimit(t *testing.T) {
	root := toolRoot(t)
	data := strings.Repeat("x", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush() // exercise streaming size detection at the boundary
		_, _ = io.WriteString(w, data)
	}))
	defer srv.Close()
	w := NewWebDownload(srv.Client(), 1024, managed.New(1024, 1000))
	if _, err := run(t, w, root, downloadArgs(t, srv.URL, "work/exact.bin")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, toolUser, "work", "exact.bin"))
	if err != nil || string(got) != data {
		t.Fatalf("boundary download: %d bytes, %v", len(got), err)
	}
	assertNoDownloadTemps(t, root)
}

func TestWebDownloadCancellation(t *testing.T) {
	root := toolRoot(t)
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "some bytes")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	w := NewWebDownload(srv.Client(), 1024, managed.New(4096, 1000))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := w.Execute(ctx, inv(root, downloadArgs(t, srv.URL, "work/file.bin")))
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download did not stop promptly")
	}
	if entries := listNames(t, filepath.Join(root, toolUser, "work")); len(entries) != 0 {
		t.Fatalf("cancelled download saved files: %v", entries)
	}
	assertNoDownloadTemps(t, root)
}

func TestWebDownloadPathBoundaries(t *testing.T) {
	root := toolRoot(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, toolUser, "work", "escape")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data")
	}))
	defer srv.Close()
	w := NewWebDownload(srv.Client(), 1024, managed.New(4096, 1000))
	for _, path := range []string{"", "file.bin", "uploads/file.bin", "../file.bin", "/tmp/file.bin", "work", "work/escape/file.bin"} {
		if _, err := run(t, w, root, downloadArgs(t, srv.URL, path)); err == nil {
			t.Errorf("unsafe path accepted: %q", path)
		}
	}
	if entries := listNames(t, outside); len(entries) != 0 {
		t.Fatalf("escaped scratchpad: %v", entries)
	}
	assertNoDownloadTemps(t, root)
}

func TestWebDownloadRegistrationAndGuard(t *testing.T) {
	cfg := config.Defaults()
	cfg.Tools.WebDownload.Enabled = true
	// Code execution and its networking remain disabled throughout this test.
	reg := BuildFor(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, managed.New(4096, 1000))
	tool, ok := reg.Get("web_download")
	if !ok || reg.Policy("web_download") != ApprovalAuto {
		t.Fatal("download not registered with default policy")
	}
	root := toolRoot(t)
	for _, url := range []string{"http://127.0.0.1/file", "http://10.1.2.3/file", "http://169.254.169.254/latest/meta-data/", "http://[::1]/file", "file:///etc/passwd", "ftp://example.com/file"} {
		_, err := run(t, tool, root, downloadArgs(t, url, "work/file.bin"))
		if err == nil {
			t.Fatalf("unsafe URL accepted: %s", url)
		}
		if strings.HasPrefix(url, "http:") && !strings.Contains(err.Error(), "private or reserved") {
			t.Fatalf("guard did not reject %s: %v", url, err)
		}
	}
	cfg.Tools.WebDownload.Enabled = false
	reg = BuildFor(cfg, slog.Default(), nil, nil, nil)
	if _, ok := reg.Get("web_download"); ok {
		t.Fatal("disabled tool registered")
	}
}

func TestWebToolsRejectSpecialAndOwnAddresses(t *testing.T) {
	root := toolRoot(t)
	client := NewGuardedClient(time.Second)
	guard := &GuardedTransport{
		LookupHost: fakeLookup("93.184.216.34"),
		InterfaceAddrs: func() ([]net.Addr, error) {
			return []net.Addr{&net.IPAddr{IP: net.ParseIP("93.184.216.34")}}, nil
		},
	}
	client.Transport.(*http.Transport).DialContext = guard.DialContext
	for _, tool := range []Tool{
		NewWebFetch(client, 1024),
		NewWebDownload(client, 1024, managed.New(4096, 1000)),
	} {
		for _, url := range []string{
			"http://0.1.2.3/file", "http://192.0.2.1/file", "http://[64:ff9b::a00:1]/file",
			"http://[2001:db8::1]/file", "http://[2606:4700::1111%25eth0]/file",
			"http://93.184.216.34/file", "http://own.example/file",
		} {
			if _, err := run(t, tool, root, downloadArgs(t, url, "work/file.bin")); err == nil || !strings.Contains(err.Error(), "private or reserved") {
				t.Fatalf("%s accepted %s or failed outside the guard: %v", tool.Name(), url, err)
			}
		}
	}
	assertNoDownloadTemps(t, root)
}

func TestWebDownloadRedirectToPrivate(t *testing.T) {
	root := toolRoot(t)
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("download reached private server")
	}))
	defer private.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/secret", http.StatusFound)
	}))
	defer front.Close()
	client := NewGuardedClient(5 * time.Second)
	var hops int
	// Only the test's initial redirector is reachable. Subsequent dials use
	// the real guard, just as every dial does in production.
	client.Transport = &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		hops++
		if hops == 1 {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return (&GuardedTransport{}).DialContext(ctx, network, addr)
	}}
	w := NewWebDownload(client, 1024, managed.New(4096, 1000))
	if _, err := run(t, w, root, downloadArgs(t, front.URL, "work/file.bin")); err == nil || !strings.Contains(err.Error(), "private or reserved") {
		t.Fatalf("redirect error = %v", err)
	}
	if hops != 2 {
		t.Fatalf("redirect was not checked: %d dials", hops)
	}
	assertNoDownloadTemps(t, root)
}
