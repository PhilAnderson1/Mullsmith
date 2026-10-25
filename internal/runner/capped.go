package runner

import (
	"bytes"
	"sync"
)

// cappedBuffer retains the first max bytes and counts the overflow instead
// of buffering unbounded output (§9.9 output caps). Write never fails: a
// flooded script should still see its exit code, not EPIPE surprises.
type cappedBuffer struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	max        int
	overflowed bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remain := c.max - c.buf.Len()
	switch {
	case len(p) <= remain:
		c.buf.Write(p)
	case remain > 0:
		c.buf.Write(p[:remain])
		c.overflowed = true
	default:
		c.overflowed = true
	}
	return len(p), nil
}

func (c *cappedBuffer) take() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Bytes()
}
