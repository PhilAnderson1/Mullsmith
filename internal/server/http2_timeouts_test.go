package server

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual server policy, shortening only its timeout durations.
func startHTTP2TestServer(t *testing.T, h http.Handler, configure func(*http.Server)) *httptest.Server {
	t.Helper()
	s := newPreparedServer(t, testCfg(), testStore(t), discardLogger(), "test", nil, nil)
	s.httpSrv.Handler = h
	s.httpSrv.ReadHeaderTimeout = time.Second
	s.httpSrv.ReadTimeout = time.Second
	s.httpSrv.WriteTimeout = 100 * time.Millisecond
	s.httpSrv.IdleTimeout = time.Minute
	s.httpSrv.HTTP2.SendPingTimeout = 100 * time.Millisecond
	s.httpSrv.HTTP2.PingTimeout = 200 * time.Millisecond
	s.httpSrv.HTTP2.WriteByteTimeout = 40 * time.Millisecond
	if configure != nil {
		configure(s.httpSrv)
	}
	ts := httptest.NewUnstartedServer(h)
	ts.Config = s.httpSrv
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func writeHTTP2Frame(t *testing.T, c net.Conn, kind, flags byte, stream uint32, payload []byte) {
	t.Helper()
	header := make([]byte, 9)
	header[0], header[1], header[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	header[3], header[4] = kind, flags
	binary.BigEndian.PutUint32(header[5:], stream)
	if _, err := c.Write(append(header, payload...)); err != nil {
		t.Fatal(err)
	}
}

func dialHTTP2TestServer(t *testing.T, ts *httptest.Server) *tls.Conn {
	t.Helper()
	c, err := tls.Dial("tcp", ts.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}) // #nosec G402 -- ephemeral test certificate
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if c.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("HTTP/2 was not negotiated")
	}
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	writeHTTP2Frame(t, c, 4, 0, 0, nil) // client SETTINGS
	writeHTTP2Frame(t, c, 4, 1, 0, nil) // acknowledge server SETTINGS
	return c
}

// HPACK indexed GET, https, /, then literal :authority = localhost.
var http2TestHeaders = []byte{0x82, 0x87, 0x84, 0x01, 0x09, 'l', 'o', 'c', 'a', 'l', 'h', 'o', 's', 't'}

func TestHTTP2IncompleteHeadersAreDisconnected(t *testing.T) {
	var called atomic.Bool
	ts := startHTTP2TestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }), nil)
	c := dialHTTP2TestServer(t, ts)
	// END_STREAM without END_HEADERS: Go's header decoder awaits CONTINUATION.
	writeHTTP2Frame(t, c, 1, 1, 1, http2TestHeaders)
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := io.Copy(io.Discard, c)
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("incomplete headers survived the connection-health budget")
	}
	if called.Load() {
		t.Fatal("incomplete request reached handler")
	}
}

func TestHTTP2StalledSocketWriterIsReleased(t *testing.T) {
	entered, done := make(chan struct{}), make(chan struct{})
	ts := startHTTP2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		close(entered)
		payload := make([]byte, 64<<10)
		for i := 0; i < 512; i++ {
			if _, err := w.Write(payload); err != nil {
				return
			}
		}
	}), func(s *http.Server) { s.HTTP2.SendPingTimeout = 0 }) // isolate write progress from ping handling
	c := dialHTTP2TestServer(t, ts)
	if err := c.NetConn().(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	credit := []byte{0x3f, 0xff, 0xff, 0xff}
	writeHTTP2Frame(t, c, 8, 0, 0, credit) // enough connection flow credit to reach socket backpressure
	writeHTTP2Frame(t, c, 1, 5, 1, http2TestHeaders)
	writeHTTP2Frame(t, c, 8, 0, 1, credit)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		c.Close()
		<-done
		t.Fatal("stalled socket writer ignored its progress timeout")
	}
	// The allowance includes Go TLS's five-second close-notify timeout.
}

func TestHTTP2HealthySSESurvivesProgressBudgets(t *testing.T) {
	ts := startHTTP2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := newSSE(w)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 12; i++ {
			if err := conn.Event("token", map[string]string{"text": "ok"}); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}), nil)
	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.ProtoMajor != 2 || strings.Count(string(body), "event: token") != 12 {
		t.Fatalf("healthy stream interrupted: proto=%s err=%v body=%s", resp.Proto, err, body)
	}
}
