package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hold the operation's SetDeadline until cancellation has closed the socket.
// This forces the ordering that used to overwrite the watcher's expired deadline.
type cancellationConn struct {
	net.Conn
	cancel context.CancelFunc
	first  atomic.Bool
	closed chan struct{}
	once   sync.Once
}

func (c *cancellationConn) SetDeadline(d time.Time) error {
	if c.first.CompareAndSwap(false, true) {
		c.cancel()
		select {
		case <-c.closed:
		case <-time.After(time.Second):
			return errors.New("cancellation failed to close the connection")
		}
	}
	return c.Conn.SetDeadline(d)
}
func (c *cancellationConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}
func TestCancellationCannotBeUndoneByDeadlineReset(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn := &cancellationConn{Conn: a, cancel: cancel, closed: make(chan struct{})}
	stop := interruptOnCancel(ctx, conn)
	defer stop()
	client := smtpClient{conn: conn, r: bufio.NewReader(conn)}
	if _, _, err := client.reply(ctx); err == nil {
		t.Fatal("read succeeded after cancellation")
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("cancellation did not permanently close the socket")
	}
}

func TestTLSHandshakeTimesOutWithoutParentDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	// A cancellable parent with no deadline models the outbox worker.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := smtpClient{conn: a}
	client.cfg.Host = "example.org"
	done := make(chan error, 1)
	go func() { done <- client.upgradeTLS(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("handshake returned %v", err)
		}
	case <-time.After(dialTimeout + 3*time.Second):
		cancel()
		<-done
		t.Fatal("TLS handshake had no timeout")
	}
}
