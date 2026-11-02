package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilePaginationOrdersAllFilesAndBindsFilter(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "paged-files@x.io", "page-invite")
	uid := meID(a)
	stamp := time.Unix(1700000000, 123456789)
	for _, kind := range fileDirs {
		for i := 0; i < 171; i++ {
			rel := fmt.Sprintf("%s/a%03d.txt", kind, i)
			seedFile(t, cfg, uid, rel, []byte("x"))
			if err := os.Chtimes(filepath.Join(cfg.Scratchpad.Root, uid, rel), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedFile(t, cfg, uid, "work/z-newest.txt", []byte("new"))
	seedFile(t, cfg, uid, "work/.hidden", []byte("hidden"))
	for _, kind := range []string{"", "work", "uploads", "artifacts"} {
		seen := map[string]bool{}
		cursor := ""
		previous := ""
		pages := 0
		for {
			q := url.Values{"limit": {"37"}, "kind": {kind}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			resp := a.do(http.MethodGet, "/api/v1/files?"+q.Encode(), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("page: %d %v", resp.StatusCode, decodedBody(t, resp))
			}
			body := decodedBody(t, resp)
			items := body["items"].([]any)
			pages++
			for i, raw := range items {
				entry := raw.(map[string]any)
				rel := entry["path"].(string)
				if seen[rel] || (kind != "" && !strings.HasPrefix(rel, kind+"/")) {
					t.Fatalf("duplicate/wrong filter: %s", rel)
				}
				if pages == 1 && i == 0 && (kind == "" || kind == "work") {
					if rel != "work/z-newest.txt" {
						t.Fatalf("newest omitted: %s", rel)
					}
				} else if rel != "work/z-newest.txt" {
					if previous != "" && rel <= previous {
						t.Fatalf("unstable tie order: %s then %s", previous, rel)
					}
					previous = rel
				}
				seen[rel] = true
			}
			cursor = body["next_cursor"].(string)
			if cursor == "" {
				break
			}
			if pages == 1 {
				other := "work"
				if kind == "work" {
					other = "uploads"
				}
				bad := a.do(http.MethodGet, "/api/v1/files?kind="+other+"&cursor="+url.QueryEscape(cursor), nil)
				bad.Body.Close()
				if bad.StatusCode != http.StatusBadRequest {
					t.Fatal("cursor accepted for another tab")
				}
			}
			if pages > 20 {
				t.Fatal("pagination did not terminate")
			}
		}
		want := 171
		if kind == "work" {
			want = 172
		}
		if kind == "" {
			want = 514
		}
		if len(seen) != want {
			t.Fatalf("%s listed %d, want %d", kind, len(seen), want)
		}
	}
}

func TestFilePageCursorSurvivesDeletionAndBudgetFailsExplicitly(t *testing.T) {
	cfg := scratchCfg(t)
	a := newAuthTest(t, cfg)
	signupAs(a, "file-mutations@x.io", "mutation-invite")
	uid := meID(a)
	for i := 0; i < 10; i++ {
		seedFile(t, cfg, uid, fmt.Sprintf("work/%02d.txt", i), []byte("x"))
	}
	ctx := context.Background()
	page, more, err := scanFilePage(ctx, cfg.Scratchpad.Root, uid, "work", nil, 3, 100)
	if err != nil || !more || len(page) != 3 {
		t.Fatalf("first page: %v %v %v", page, more, err)
	}
	last := page[2]
	cursor := &fileCursor{Modified: last.info.ModTime().UnixNano(), Path: last.rel, Kind: "work"}
	if err := os.Remove(filepath.Join(cfg.Scratchpad.Root, uid, last.rel)); err != nil {
		t.Fatal(err)
	}
	seedFile(t, cfg, uid, "work/newer.txt", []byte("new"))
	next, _, err := scanFilePage(ctx, cfg.Scratchpad.Root, uid, "work", cursor, 20, 100)
	if err != nil || len(next) != 7 {
		t.Fatalf("cursor after deletion: %d %v", len(next), err)
	}
	if _, _, err := scanFilePage(ctx, cfg.Scratchpad.Root, uid, "work", nil, 3, 2); err == nil {
		t.Fatal("over-budget scan returned a partial success")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := scanFilePage(cancelled, cfg.Scratchpad.Root, uid, "work", nil, 3, 100); err == nil {
		t.Fatal("cancelled scan returned success")
	}
}
