package server

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- astra2 finding 1: ordinary responses carry a finite write deadline;
// the surfaces with legitimately long writes self-manage theirs ---

// The exhaustion proof inverted: with the default ON, a client that never
// reads must make writes FAIL within the cap. With WriteTimeout off (the
// pre-fix default) the write blocks in a syscall forever — the pinned
// handler+connection is the vulnerability, and this test would hit the
// suite timeout.
func TestStalledClientTimesOutOnOrdinaryResponse(t *testing.T) {
	done := make(chan error, 1)
	handler := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := bytes.Repeat([]byte("x"), 64<<10)
		var wErr error
		for i := 0; i < 8192 && wErr == nil; i++ { // upper bound, never reached on a stall
			_, wErr = w.Write(payload)
		}
		done <- wErr
	}), discardLogger())
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.WriteTimeout = 250 * time.Millisecond
	srv.Start()
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if err := tc.(*net.TCPConn).SetReadBuffer(1); err != nil { // tiny window → backpressure arrives soon
		t.Logf("SetReadBuffer: %v (test still valid, just slower)", err)
	}
	io.WriteString(tc, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	// ...and never read the response.

	select {
	case wErr := <-done:
		if wErr == nil {
			t.Fatal("512 MB to a stalled client all succeeded — buffers absurdly large?")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stalled ordinary response never timed out — a stalled reader would pin the handler forever")
	}
}

// The reason the cap may be ON by default: SSE lifts the global deadline
// the moment the stream starts and arms its own per-frame deadline, so
// generations survive a server-wide cap comfortably smaller than they do.
func TestSSESurvivesGlobalWriteTimeout(t *testing.T) {
	done := make(chan error, 1)
	handler := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := newSSE(w)
		if err != nil {
			done <- err
			return
		}
		var wErr error
		for i := 0; i < 3 && wErr == nil; i++ {
			wErr = conn.Event("token", map[string]string{"t": "hi"})
			if i < 2 {
				time.Sleep(300 * time.Millisecond) // total stream outlives the global cap
			}
		}
		conn.Close()
		done <- wErr
	}), discardLogger())
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if wErr := <-done; wErr != nil {
		t.Errorf("SSE stream died under the global write timeout: %v", wErr)
	}
	if n := strings.Count(string(body), "event: token"); n != 3 {
		t.Errorf("body carried %d token events, want 3", n)
	}
}

// Downloads: the per-chunk deadline ROLLS — total transfer time may exceed
// both the global cap and the per-chunk bound, as long as every individual
// chunk handoff is prompt. A pre-armed deadline that merely went stale must
// not fire between writes, and the global absolute cap must not bite, or
// honest multi-megabyte downloads would silently truncate.
func TestDownloadDeadlineRollsPerChunk(t *testing.T) {
	defer func(old time.Duration) { downloadWriteTimeout = old }(downloadWriteTimeout)
	downloadWriteTimeout = 300 * time.Millisecond

	done := make(chan error, 1)
	handler := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		chunk := bytes.Repeat([]byte("y"), 32<<10)
		var wErr error
		for i := 0; i < 6 && wErr == nil; i++ {
			_, wErr = io.Copy(deadlineWriter{rc: rc, w: w}, bytes.NewReader(chunk))
			if i < 5 {
				time.Sleep(500 * time.Millisecond) // idle: past BOTH deadlines
			}
		}
		_ = rc.SetWriteDeadline(time.Time{})
		done <- wErr
	}), discardLogger())
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if wErr := <-done; wErr != nil {
		t.Errorf("rolling deadline errored a healthy transfer: %v — behaving as absolute, not rolling", wErr)
	}
	if len(body) != 6*32<<10 {
		t.Errorf("body = %d bytes, want %d", len(body), 6*32<<10)
	}
}
