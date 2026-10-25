package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// sseConn writes Server-Sent Events frames (spec §6.1 step 4). All writes go
// through one mutex so the heartbeat cannot interleave with token frames.
type sseConn struct {
	w  http.ResponseWriter
	rc *http.ResponseController
	mu sync.Mutex
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

func (c *sseConn) Event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := append([]byte("event: "+name+"\ndata: "), b...)
	frame = append(frame, '\n', '\n')
	if _, err := c.w.Write(frame); err != nil {
		return err
	}
	return c.rc.Flush()
}

// Comment writes an SSE comment line (": ping") — keeps proxies warm and
// reveals dead clients through a write error.
func (c *sseConn) Comment(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write([]byte(": " + text + "\n\n")); err != nil {
		return err
	}
	return c.rc.Flush()
}
