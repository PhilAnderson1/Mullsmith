package server

import (
	"net"
	"net/http"
	"sync"
)

const maxHeaderBytes = 32 << 10

// Overload is refused without queuing; streams retain their slot until return.
func limitRequests(next http.Handler, max int) http.Handler {
	if max < 0 {
		return next
	}
	slots := make(chan struct{}, max)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			writeAPIError(w, r, http.StatusServiceUnavailable, "server_busy", "too many concurrent requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type connectionListener struct {
	net.Listener
	slots chan struct{}
}
type admittedConnection struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *admittedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
func limitConnections(listener net.Listener, max int) net.Listener {
	if max < 0 {
		return listener
	}
	return &connectionListener{Listener: listener, slots: make(chan struct{}, max)}
}
func (l *connectionListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &admittedConnection{Conn: conn, release: func() { <-l.slots }}, nil
		default:
			// No HTTP/TLS has been parsed yet, so rejection is a TCP close.
			_ = conn.Close()
		}
	}
}
