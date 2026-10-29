package mail

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/config"
)

// --- fake SMTP server ------------------------------------------------------

type received struct {
	raw       string // exact DATA octets (stuffed form)
	from      string
	to        string
	subject   string
	bodyAfter string // body after dot-unstuffing
}

type fakeServer struct {
	ln              net.Listener
	advertiseSTARTL bool
	implicitTLS     bool
	cert            tls.Certificate
	authUser        string // "" = AUTH not offered/expected
	authPass        string

	mu       sync.Mutex
	lines    []string // every line the server read (for absence assertions)
	messages []received
}

func newFakeServer(t *testing.T, s *fakeServer) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	go s.serve(t)
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (f *fakeServer) port() int {
	return f.ln.Addr().(*net.TCPAddr).Port
}

func (f *fakeServer) logLine(l string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, l)
}

func (f *fakeServer) saw(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (f *fakeServer) msgs() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.messages...)
}

func (f *fakeServer) serve(t *testing.T) {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(t, conn)
	}
}

func (f *fakeServer) handle(t *testing.T, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	w := func(format string, args ...any) {
		fmt.Fprintf(conn, format+"\r\n", args...)
	}
	if f.implicitTLS {
		tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
	}
	w("220 fake ESMTP")
	r := bufio.NewReader(conn)
	const promptUser, promptPass = "VXNlcm5hbWU6", "UGFzc3dvcmQ6"
	await := "" // "user" | "pass" during AUTH LOGIN
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.logLine(line)
		up := strings.ToUpper(line)
		switch {
		case await == "user":
			// This line is the base64 username; now ask for the password.
			await = "pass"
			w("334 %s", promptPass)
			continue
		case await == "pass":
			await = ""
			got, _ := base64.StdEncoding.DecodeString(line)
			if string(got) == f.authPass {
				w("235 2.7.0 Authentication successful")
			} else {
				w("535 5.7.8 Authentication failed")
			}
			continue
		}
		switch {
		case strings.HasPrefix(up, "EHLO"):
			w("250-fake")
			if f.advertiseSTARTL && !f.implicitTLS {
				w("250-STARTTLS")
			}
			if f.authUser != "" {
				w("250 AUTH LOGIN")
			} else {
				w("250 END")
			}
		case strings.HasPrefix(up, "HELO"):
			w("250 fake")
		case strings.HasPrefix(up, "STARTTLS"):
			if !f.advertiseSTARTL {
				w("504 no STARTTLS here")
				continue
			}
			w("220 Ready to start TLS")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			r = bufio.NewReader(conn)
			w = func(format string, args ...any) {
				fmt.Fprintf(conn, format+"\r\n", args...)
			}
		case strings.HasPrefix(up, "AUTH LOGIN"):
			if f.authUser == "" {
				w("504 no auth")
				continue
			}
			await = "user"
			w("334 %s", promptUser) // ask for the username
		case strings.HasPrefix(up, "MAIL FROM"):
			await = "" // an envelope before auth completes: reset the dance
			w("250 OK")
		case strings.HasPrefix(up, "RCPT TO"):
			w("250 OK")
		case strings.HasPrefix(up, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var (
				raw    strings.Builder
				unstuf strings.Builder
				header = true
				inMsg  received
			)
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				raw.WriteString(l)
				clean := strings.TrimSuffix(strings.TrimSuffix(l, "\r\n"), "\n")
				if header && clean == "" {
					header = false
					continue
				}
				if strings.HasPrefix(clean, ".") {
					clean = clean[1:]
				}
				if header {
					if v, ok := strings.CutPrefix(clean, "From: "); ok {
						inMsg.from = v
					}
					if v, ok := strings.CutPrefix(clean, "To: "); ok {
						inMsg.to = v
					}
					if v, ok := strings.CutPrefix(clean, "Subject: "); ok {
						inMsg.subject = v
					}
					continue // headers never reach the body
				}
				unstuf.WriteString(clean + "\n")
			}
			inMsg.raw = raw.String()
			inMsg.bodyAfter = unstuf.String()
			f.mu.Lock()
			f.messages = append(f.messages, inMsg)
			f.mu.Unlock()
			w("250 2.6.0 Queued")
		case strings.HasPrefix(up, "QUIT"):
			w("221 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

// --- test certificate -------------------------------------------------------

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-smtp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, nil)) }

func clientCfg(s *fakeServer, auth bool) config.SMTP {
	return config.SMTP{Host: "127.0.0.1", Port: s.port(), AuthenticatedSMTP: auth, From: "mullsmith@test"}
}

// --- tests -------------------------------------------------------------------

func TestAuthRefusedWithoutSTARTTLS(t *testing.T) {
	s := newFakeServer(t, &fakeServer{authUser: "gk", authPass: "hunter2"})
	c := &smtpClient{cfg: clientCfg(s, true)}
	c.cfg.Username, c.cfg.Password = "gk", "hunter2"
	err := c.Deliver("user@dest.io", "s", "body")
	if err == nil || !strings.Contains(err.Error(), "in the clear") {
		t.Fatalf("expected cleartext refusal, got %v", err)
	}
	// The proof: the server never saw AUTH, a username, or the password
	// (raw or base64-encoded).
	if s.saw("AUTH") || s.saw("hunter2") || s.saw(base64.StdEncoding.EncodeToString([]byte("hunter2"))) {
		t.Error("credentials reached a cleartext server")
	}
}

func TestAuthenticatedSendOverSTARTTLS(t *testing.T) {
	cert, pool := selfSignedCert(t)
	s := newFakeServer(t, &fakeServer{advertiseSTARTL: true, cert: cert, authUser: "gk", authPass: "pw"})
	c := &smtpClient{cfg: clientCfg(s, true), tlsConfig: &tls.Config{RootCAs: pool}}
	c.cfg.Username, c.cfg.Password = "gk", "pw"
	body := "line one\n.dot stuffed\nlast\r\nmiddle\n"
	if err := c.Deliver("user@dest.io", "Hello there", body); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	msgs := s.msgs()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.to != "user@dest.io" || m.subject != "Hello there" || m.from != "mullsmith@test" {
		t.Errorf("envelope/headers = %+v", m)
	}
	if !strings.Contains(m.raw, "\r\n..dot stuffed\r\n") {
		t.Errorf("dot-stuffing missing from wire form: %q", m.raw)
	}
	want := "line one\n.dot stuffed\nlast\nmiddle\n"
	if m.bodyAfter != want {
		t.Errorf("decoded body = %q, want %q", m.bodyAfter, want)
	}
	for _, h := range []string{"Date: ", "Message-ID: ", "Content-Type: text/plain"} {
		if !strings.Contains(m.raw, h) {
			t.Errorf("header %q missing", h)
		}
	}
}

func TestImplicitTLSSend(t *testing.T) {
	cert, pool := selfSignedCert(t)
	s := newFakeServer(t, &fakeServer{implicitTLS: true, cert: cert, authUser: "gk", authPass: "pw"})
	c := &smtpClient{cfg: clientCfg(s, true), implicit: true, tlsConfig: &tls.Config{RootCAs: pool}}
	c.cfg.Username, c.cfg.Password = "gk", "pw"
	if err := c.Deliver("u@d.io", "s", "b"); err != nil {
		t.Fatalf("implicit-TLS Deliver: %v", err)
	}
	if len(s.msgs()) != 1 {
		t.Fatalf("want 1 message, got %d", len(s.msgs()))
	}
}

func TestUnauthenticatedPlainSendAllowed(t *testing.T) {
	s := newFakeServer(t, &fakeServer{}) // no TLS, no AUTH — the LAN relay
	c := &smtpClient{cfg: clientCfg(s, false)}
	if err := c.Deliver("u@d.io", "relay me", "body"); err != nil {
		t.Fatalf("unauthenticated relay send: %v", err)
	}
	if len(s.msgs()) != 1 || s.msgs()[0].subject != "relay me" {
		t.Fatalf("message not received: %+v", s.msgs())
	}
}

func TestInjectionRejected(t *testing.T) {
	s := newFakeServer(t, &fakeServer{})
	c := &smtpClient{cfg: clientCfg(s, false)}
	if err := c.Deliver("u@d.io\r\nRCPT TO:<evil@x.io>", "s", "b"); err == nil {
		t.Error("CRLF-injecting recipient accepted")
	}
	c2 := &smtpClient{cfg: config.SMTP{Host: "127.0.0.1", Port: s.port(), From: "a@b\r\nX: y"}}
	if err := c2.Deliver("u@d.io", "s", "b"); err == nil {
		t.Error("CRLF-injecting From accepted")
	}
}

// --- outbox -----------------------------------------------------------------

type countingDeliverer struct {
	mu        sync.Mutex
	attempt   int
	failFor   int // first N Deliver calls fail overall
	delivered chan struct{}
}

func (d *countingDeliverer) Deliver(string, string, string) error {
	d.mu.Lock()
	d.attempt++
	n := d.attempt
	d.mu.Unlock()
	if n <= d.failFor {
		return fmt.Errorf("synthetic failure %d", n)
	}
	select {
	case d.delivered <- struct{}{}:
	default:
	}
	return nil
}

func (d *countingDeliverer) attempts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempt
}

func TestOutboxRetriesThenDrops(t *testing.T) {
	del := &countingDeliverer{failFor: 2, delivered: make(chan struct{}, 1)}
	o := NewOutbox(del, testLogger())
	o.backoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}

	o.Send("a@b.io", "retry me", "x") // fails twice, succeeds on 3rd attempt
	select {
	case <-del.delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("retry never succeeded")
	}

	// Now a message that exhausts all attempts: 1 + len(backoff) tries,
	// then dropped without wedging the worker. The successor after it must
	// still be delivered.
	del.mu.Lock()
	del.failFor = del.attempt + len(o.backoff) + 1 // doomed burns exactly the window
	del.mu.Unlock()
	o.Send("c@d.io", "doomed", "x")
	o.Send("e@f.io", "after doomed", "x") // proves the worker survives the drop
	select {
	case <-del.delivered: // "after doomed" went out
	case <-time.After(5 * time.Second):
		t.Fatal("worker wedged after a dropped message")
	}
	// 3 attempts for the first message + 3 for the doomed one + 1 success.
	if got := del.attempts(); got != 7 {
		t.Errorf("attempts = %d, want 7 (3 + 3 + 1)", got)
	}
}

func TestMessageBuilders(t *testing.T) {
	subj, body := ConfirmationMail("https://gk.example:3210", "TOK123")
	if !strings.Contains(body, "https://gk.example:3210/#confirm?t=TOK123") {
		t.Errorf("confirm link missing: %s", body)
	}
	if !strings.Contains(body, "24 hours") || !strings.Contains(body, "deleted") {
		t.Errorf("confirm mail must state the 24h confirm-or-lose deadline: %s", body)
	}
	if subj == "" {
		t.Error("empty subject")
	}
	subj, body = ResetMail("https://gk.example:3210", "TOK9")
	if !strings.Contains(body, "https://gk.example:3210/#reset?t=TOK9") ||
		!strings.Contains(body, "30 minutes") {
		t.Errorf("reset mail malformed: %s / %s", subj, body)
	}
	if subj, body = ConfirmationMailChange("https://gk.example:3210", "TOK7"); !strings.Contains(body, "https://gk.example:3210/#confirm?t=TOK7") {
		t.Errorf("change-confirm link missing: %s", body)
	}
	if !strings.Contains(subj, "address change") ||
		!strings.Contains(body, "activate your Mullsmith email address change") {
		t.Errorf("change-confirm must read as an address change, not an activation: %q / %s", subj, body)
	}
	if strings.Contains(body, "activate your mullsmith account") || strings.Contains(body, "deleted") {
		t.Errorf("change-confirm must carry no sign-up/deletion wording: %s", body)
	}
	// Sign-up wording must stay distinct from the change wording.
	if s1, _ := ConfirmationMail("https://g", "T"); s1 == subj {
		t.Error("sign-up and address-change confirm mails share a subject")
	}
	subj, body = AddressChangedMail()
	if !strings.Contains(subj, "was requested") || !strings.Contains(body, "Nothing has changed yet") {
		t.Errorf("old-mailbox notice must be request-time, not accomplished fact: %q / %s", subj, body)
	}
	if subj, _ = AddressChangedMail(); subj == "" {
		t.Error("address-change notice needs a subject")
	}
}
