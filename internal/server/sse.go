package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// sseConn writes Server-Sent Events frames (spec §6.1 step 4). All writes go
// through one mutex so the heartbeat and the auto-titler cannot interleave
// with token frames.
type sseConn struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	mu     sync.Mutex
	closed bool
}

// newSSE sets streaming headers, lifts the server-wide write deadline for
// this connection and emits the initial 200.
func newSSE(w http.ResponseWriter) (*sseConn, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (spec §9.10)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // generations outlive the write timeout
	w.WriteHeader(http.StatusOK)
	c := &sseConn{w: w, rc: rc}
	if err := rc.Flush(); err != nil {
		return nil, err
	}
	return c, nil
}

// sseWriteTimeout bounds ONE frame's write+flush. A client that stops
// reading (sleeping laptop, dropped mobile link: TCP zero window, no FIN)
// otherwise blocks the write in a syscall forever — no ctx or tool timeout
// interrupts that, and the stalled stream holds a generation slot until
// restart (audit finding 10). 10 s to hand a few KB to a drained socket
// buffer means the client is gone; the heartbeat interval (15 s) stays the
// outer rhythm. Var so tests can shrink it.
var sseWriteTimeout = 10 * time.Second

// writeFrame writes one frame under a rolling deadline: armed per write,
// cleared right after, so idle time between frames costs nothing and the
// deadline can never expire "while nobody is writing". Fd-level control
// reaches the connection through statusWriter's Unwrap.
func (c *sseConn) writeFrame(frame []byte) error {
	if err := c.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) { // unwrappable wrappers must not kill every stream
		return err
	}
	_, err := c.w.Write(frame)
	if err == nil {
		err = c.rc.Flush() // the real socket error usually lives here, not in Write
	}
	_ = c.rc.SetWriteDeadline(time.Time{})
	return err
}

func (c *sseConn) Event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return http.ErrHandlerTimeout
	}
	frame := append([]byte("event: "+name+"\ndata: "), b...)
	frame = append(frame, '\n', '\n')
	return c.writeFrame(frame)
}

// Close marks the connection finished so late background writers (the
// auto-titler) fail fast instead of writing into a recycled ResponseWriter.
// The handler calls it just before returning, after the done event.
func (c *sseConn) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

// Comment writes an SSE comment line (": ping") — keeps proxies warm and
// reveals dead clients through a write error. The rolling frame deadline
// guarantees the reveal even when the client merely stalls: pre-fix the
// heartbeat itself blocked on the write while holding c.mu, so nothing
// could cancel the generation (audit finding 10).
func (c *sseConn) Comment(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return http.ErrHandlerTimeout
	}
	return c.writeFrame([]byte(": " + text + "\n\n"))
}
