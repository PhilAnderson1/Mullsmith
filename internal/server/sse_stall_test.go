package server

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- audit finding 10: streaming writes must have an effective stall timeout

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Pre-fix, NewResponseController stopped at statusWriter (no Unwrap):
// SetWriteDeadline died with ErrNotSupported and rc.Flush returned nil no
// matter what the socket did.
func TestStatusWriterPassesControllerThrough(t *testing.T) {
	var deadlineErr error
	handler := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlineErr = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		w.Write([]byte("ok"))
	}), discardLogger())
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if deadlineErr != nil {
		t.Errorf("SetWriteDeadline through statusWriter: %v, want nil (Unwrap missing?)", deadlineErr)
	}
}

type flushErrWriter struct {
	http.ResponseWriter
	err error
}

func (f *flushErrWriter) Flush()            {}
func (f *flushErrWriter) FlushError() error { return f.err }

func TestStatusWriterSurfacesFlushError(t *testing.T) {
	sentinel := errors.New("client vanished")
	sw := &statusWriter{ResponseWriter: &flushErrWriter{
		ResponseWriter: httptest.NewRecorder(), err: sentinel}, status: 200}
	if err := http.NewResponseController(sw).Flush(); !errors.Is(err, sentinel) {
		t.Errorf("rc.Flush = %v, want the inner FlushError to surface", err)
	}
}

// The regression proof: a client that never reads must make writes FAIL
// within a bounded time. Pre-fix the write blocks in a syscall forever and
// this test hits the suite timeout — that hang is exactly the bug.
func TestSSEStalledClientTimesOut(t *testing.T) {
	defer func(old time.Duration) { sseWriteTimeout = old }(sseWriteTimeout)
	sseWriteTimeout = 250 * time.Millisecond

	done := make(chan error, 1)
	handler := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := newSSE(w)
		if err != nil {
			done <- err
			return
		}
		payload := strings.Repeat("x", 64<<10)
		var wErr error
		for i := 0; i < 8192 && wErr == nil; i++ { // upper bound, never reached on a stall
			wErr = conn.Event("token", payload)
		}
		conn.Close()
		done <- wErr
	}), discardLogger())
	srv := httptest.NewServer(handler)
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
			t.Fatal("8192 frames to a stalled client all succeeded — buffers absurdly large?")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stalled SSE write never timed out — a stalled client would hold a generation slot forever")
	}
}

// The deadline must ROLL (armed per frame), not sit absolute from connection
// start: frames slower than the per-frame timeout are normal for a healthy
// stream (heartbeats are 15 s apart in production).
func TestSSEDeadlineRollsPerFrame(t *testing.T) {
	defer func(old time.Duration) { sseWriteTimeout = old }(sseWriteTimeout)
	sseWriteTimeout = 200 * time.Millisecond

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
				time.Sleep(600 * time.Millisecond) // 3× the per-frame deadline, idle
			}
		}
		conn.Close()
		done <- wErr
	}), discardLogger())
	srv := httptest.NewServer(handler)
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
		t.Errorf("healthy client errored: %v — deadline is behaving as absolute, not rolling", wErr)
	}
	if n := strings.Count(string(body), "event: token"); n != 3 {
		t.Errorf("body carried %d token events, want 3", n)
	}
}
