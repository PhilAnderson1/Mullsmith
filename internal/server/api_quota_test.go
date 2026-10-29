package server

import (
	"bytes"
	"io/fs"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
)

// --- audit finding 5: uploads must not share a stale quota picture ---------

func quotaCfg(t *testing.T, quota int64) *config.Config {
	t.Helper()
	cfg := authCfg(t)
	cfg.Scratchpad.MaxFileSize = 1 << 20
	cfg.Scratchpad.UserQuota = config.Size(quota)
	return cfg
}

// uploadRaw posts a multipart upload without touching the shared authTest
// jar, so N requests can fly concurrently without -race flagging the jar
// map. Snapshot the jar before the fan-out.
func uploadRaw(t *testing.T, url string, cookies map[string]string, csrf string, content []byte) (int, http.Header) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	part, err := mw.CreateFormFile("file", "blob.bin")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(content)
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, url+"/api/v1/files", &b)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(auth.CSRFHeaderName, csrf)
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// The audit's repro: quota was decided from a size read ONCE at request
// start, so N concurrent uploads all saw the same used figure and all
// stored — 1,400 bytes landed under a 1,024-byte quota. The atomic commit
// re-reads usage per rename under a mutex, so exactly floor(quota/size)
// files can win no matter the scheduling, and the disk agrees.
func TestConcurrentUploadsCannotBreachQuota(t *testing.T) {
	cfg := quotaCfg(t, 1024)
	a, _ := pwTestApp(t, cfg)
	signupAs(a, "quota@test.io", "gk-code-1")

	const n, size = 5, 400 // 1024-byte quota fits exactly two
	cookies := make(map[string]string, len(a.cookies))
	for k, v := range a.cookies {
		cookies[k] = v
	}
	csrf := a.csrf

	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := bytes.Repeat([]byte{byte('a' + i)}, size)
			codes[i], _ = uploadRaw(t, a.ts.URL, cookies, csrf, body)
		}(i)
	}
	close(start)
	wg.Wait()

	var stored, rejected, other int
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			stored++
		case http.StatusInsufficientStorage:
			rejected++
		default:
			other++
		}
	}
	if stored != 2 || rejected != 3 || other != 0 {
		t.Errorf("codes: stored=%d rejected=%d other=%d, want 2/3/0 (codes %v)", stored, rejected, other, codes)
	}

	// The filesystem must agree: nothing beyond the quota landed.
	var total int64
	filepath.WalkDir(cfg.Scratchpad.Root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if total > 1024 {
		t.Errorf("%d bytes on disk under a 1024-byte quota", total)
	}
}

// The global upload gate saturates to 429 server_busy — same contract as
// the Argon2 gate — and recovers the moment a slot frees.
func TestUploadGateSaturation429(t *testing.T) {
	a, s := pwTestApp(t, quotaCfg(t, 1<<20))
	signupAs(a, "ugate@test.io", "gk-code-1")

	s.uploadSem = make(chan struct{}, 1)
	s.uploadSem <- struct{}{} // hold the only slot

	resp := a.upload("x.bin", []byte("hello"))
	if resp.StatusCode != http.StatusTooManyRequests || apiErrCode(t, resp) != "server_busy" {
		t.Fatalf("held gate: %d, want 429 server_busy", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	<-s.uploadSem
	resp = a.upload("x.bin", []byte("hello"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("after release: %d, want 201", resp.StatusCode)
	}
}
