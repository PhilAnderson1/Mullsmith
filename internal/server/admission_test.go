package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestCapacityRejectsWithoutQueueAndReleasesSlot(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	h := limitRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			entered <- struct{}{}
			<-release
		}
		io.WriteString(w, "ok")
	}), 1)
	ts := httptest.NewServer(h)
	defer ts.Close()
	defer unblock()
	first := make(chan error, 1)
	go func() {
		resp, err := http.Get(ts.URL + "/stream")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter")
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "1" || !strings.Contains(string(body), "server_busy") {
		t.Fatalf("not refused immediately: %d %s", resp.StatusCode, body)
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request slot was not released: %d", resp.StatusCode)
	}
}

func TestConnectionCapacityAppliesBeforeHeadersAndRecovers(t *testing.T) {
	certFile, keyFile := testTLSFiles(t)
	for _, secure := range []bool{false, true} {
		name := "HTTP"
		if secure {
			name = "HTTPS"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Server.MaxConnections = 1
			s := newPreparedServer(t, cfg, testStore(t), discardLogger(), "test", nil, nil)
			states := make(chan http.ConnState, 10)
			s.httpSrv.ConnState = func(_ net.Conn, state http.ConnState) { states <- state }
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if secure {
					done <- s.ServeTLS(listener, certFile, keyFile)
				} else {
					done <- s.Serve(listener)
				}
			}()
			t.Cleanup(func() {
				s.httpSrv.Close()
				if err := <-done; !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("serve: %v", err)
				}
			})
			first, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			select {
			case state := <-states:
				if state != http.StateNew {
					t.Fatal(state)
				}
			case <-time.After(time.Second):
				t.Fatal("connection was not admitted")
			}
			io.WriteString(first, "G") // no complete request or TLS handshake required to hold a slot
			second, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			second.SetReadDeadline(time.Now().Add(time.Second))
			_, err = second.Read(make([]byte, 1))
			if err == nil {
				t.Fatal("excess connection stayed open")
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("excess connection queued instead of closing")
			}
			first.Close()
			for closed := false; !closed; {
				select {
				case state := <-states:
					closed = state == http.StateClosed
				case <-time.After(time.Second):
					t.Fatal("closed connection retained its slot")
				}
			}
			var third net.Conn
			if secure {
				third, err = tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- ephemeral test certificate
			} else {
				third, err = net.Dial("tcp", listener.Addr().String())
			}
			if err != nil {
				t.Fatal(err)
			}
			defer third.Close()
			third.SetDeadline(time.Now().Add(time.Second))
			io.WriteString(third, "GET /healthz HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(third), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("connection slot not released: %d", resp.StatusCode)
			}
		})
	}
}

func testTLSFiles(t *testing.T) (string, string) {
	t.Helper()
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := seed.TLS.Certificates[0]
	seed.Close()
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestHeaderBudgetRejectsOversizedHeaders(t *testing.T) {
	s := newPreparedServer(t, testCfg(), testStore(t), discardLogger(), "test", nil, nil)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config = s.httpSrv
	ts.Start()
	defer ts.Close()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", ts.URL+"/healthz", nil)
	req.Header.Set("X-Flood", strings.Repeat("x", 40<<10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized headers accepted: %d", resp.StatusCode)
	}
}
