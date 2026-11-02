// Package mail sends mullsmith's transactional messages (confirmation and
// password-reset links, address-change notices) over SMTP.
//
// No dependency: Go's net/smtp cannot do AUTH-over-STARTTLS cleanly, and a
// ~150-line conversation is the whole feature. HTTP handlers never wait on
// the network — they hand jobs to an Outbox whose worker retries and logs
// (plan §4).
package mail

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"mullsmith/internal/config"
)

const (
	dialTimeout   = 10 * time.Second
	deadlineEvery = 30 * time.Second // refreshed before every read/write
)

// Deliverer hands one fully-composed message to a transport. The SMTP
// implementation below; server tests substitute a recording fake.
// Deliver must honour ctx: it bounds the dial, the TLS handshake and every
// read/write, so shutdown can cut an ACTIVE delivery, not just a retry
// wait (review3 issue 5) — a delivery already parked on the socket is
// interrupted by cancellation alone, not left to age out its deadline.
type Deliverer interface {
	Deliver(ctx context.Context, to, subject, body string) error
}

// opDeadline is the deadline for one wire operation: the per-op 30 s,
// cut short by the delivery context's own deadline when that lands first.
// A done ctx is an error before the connection is even touched.
func opDeadline(ctx context.Context) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	dl := time.Now().Add(deadlineEvery)
	if x, ok := ctx.Deadline(); ok && x.Before(dl) {
		dl = x
	}
	return dl, nil
}

// smtpClient speaks just enough SMTP: EHLO, optional STARTTLS, optional
// AUTH LOGIN, one message, QUIT.
type smtpClient struct {
	cfg       config.SMTP
	implicit  bool // 465-style: TLS from the first byte
	tlsConfig *tls.Config
	conn      net.Conn
	r         *bufio.Reader
	ext       map[string]bool
}

// NewSMTP returns the production Deliverer for the configured server.
func NewSMTP(cfg config.SMTP) Deliverer {
	return &smtpClient{cfg: cfg, implicit: cfg.Port == 465}
}

func (c *smtpClient) Deliver(ctx context.Context, to, subject, body string) error {
	if err := safeAddr(to); err != nil {
		return err
	}
	if err := safeAddr(c.cfg.From); err != nil {
		return fmt.Errorf("email.smtp.from: %w", err)
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.Port)))
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	defer conn.Close()
	c.conn = conn
	c.r = bufio.NewReader(conn)
	// opDeadline can only bound operations that START after the context is
	// done; a read or write ALREADY parked would otherwise wait out the
	// per-op 30 s (reproduced against this client and a silent server,
	// review follow-up to issue 5). The watcher closes the raw socket the
	// instant ctx is cancelled, interrupting the TLS record layer above it.
	unarm := interruptOnCancel(ctx, conn)
	defer unarm()
	err = c.converse(ctx, to, subject, body)
	if err != nil && ctx.Err() != nil {
		// The interrupted socket reports a network error; shutdown cancelled
		// this delivery, and the caller (the outbox retry policy included)
		// should read the failure as the cancellation it is.
		return ctx.Err()
	}
	return err
}

// interruptOnCancel closes conn as soon as ctx is done. Unlike expiring a
// deadline, closure cannot be undone by a concurrent operation arming its
// normal deadline. The returned func retires the watcher.
func interruptOnCancel(ctx context.Context, conn net.Conn) func() {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	return func() { close(stop) }
}

// converse runs the SMTP conversation on the dialed, watched connection.
func (c *smtpClient) converse(ctx context.Context, to, subject, body string) error {
	// Implicit TLS (465): the handshake precedes the 220 greeting, so the
	// connection is upgraded before anything is read.
	secure := false
	if c.implicit {
		if err := c.upgradeTLS(ctx); err != nil {
			return err
		}
		secure = true
	}
	if _, _, err := c.reply(ctx); err != nil { // greeting
		return fmt.Errorf("smtp greeting: %w", err)
	}
	if err := c.ehlo(ctx); err != nil {
		return err
	}
	if !secure && c.ext["STARTTLS"] {
		if code, txt, err := c.cmd(ctx, "STARTTLS"); err != nil {
			return err
		} else if code != 220 {
			return fmt.Errorf("STARTTLS refused: %s", txt)
		}
		if err := c.upgradeTLS(ctx); err != nil {
			return err
		}
		secure = true
		if err := c.ehlo(ctx); err != nil { // capabilities change post-TLS
			return err
		}
	}
	// Fail-closed doctrine: credentials never cross a connection that is
	// not TLS, whether or not the server offered STARTTLS.
	if c.cfg.AuthenticatedSMTP && !secure {
		return errors.New("smtp: server does not offer STARTTLS — refusing to send credentials in the clear")
	}
	if c.cfg.AuthenticatedSMTP {
		if err := c.authLogin(ctx); err != nil {
			return err
		}
	}
	if code, txt, err := c.cmd(ctx, "MAIL FROM:<"+c.cfg.From+">"); err != nil {
		return err
	} else if code != 250 {
		return fmt.Errorf("MAIL FROM: %s", txt)
	}
	if code, txt, err := c.cmd(ctx, "RCPT TO:<"+to+">"); err != nil {
		return err
	} else if code != 250 && code != 251 {
		return fmt.Errorf("RCPT TO: %s", txt)
	}
	if code, txt, err := c.cmd(ctx, "DATA"); err != nil {
		return err
	} else if code != 354 {
		return fmt.Errorf("DATA: %s", txt)
	}
	if err := c.writeMessage(ctx, to, subject, body); err != nil {
		return err
	}
	if code, txt, err := c.reply(ctx); err != nil {
		return err
	} else if code != 250 {
		return fmt.Errorf("message rejected: %s", txt)
	}
	// Polite goodbye; nobody waits on its failure.
	_, _, _ = c.cmd(ctx, "QUIT")
	return nil
}

func (c *smtpClient) ehlo(ctx context.Context) error {
	code, txt, err := c.cmd(ctx, "EHLO mullsmith")
	if err != nil {
		return err
	}
	if code == 250 {
		c.ext = parseExtensions(txt)
		return nil
	}
	// Servers without EHLO get the (unauthenticated-era only) legacy HELO;
	// no STARTTLS/AUTH can exist, which the fail-closed checks above catch.
	code, txt, err = c.cmd(ctx, "HELO mullsmith")
	if err != nil {
		return err
	}
	if code != 250 {
		return fmt.Errorf("HELO: %s", txt)
	}
	c.ext = map[string]bool{}
	return nil
}

func (c *smtpClient) authLogin(ctx context.Context) error {
	if !c.ext["AUTH-LOGIN"] {
		return errors.New("smtp: server does not advertise AUTH LOGIN")
	}
	if code, txt, err := c.cmd(ctx, "AUTH LOGIN"); err != nil {
		return err
	} else if code != 334 {
		return fmt.Errorf("AUTH LOGIN: %s", txt)
	}
	// The 334 continuations are prompts, not secrets; the responses are,
	// so errors here must never echo user or password.
	if code, _, err := c.cmd(ctx, b64(c.cfg.Username)); err != nil {
		return err
	} else if code != 334 {
		return fmt.Errorf("smtp AUTH: username rejected (code %d)", code)
	}
	if code, _, err := c.cmd(ctx, b64(c.cfg.Password)); err != nil {
		return err
	} else if code != 235 {
		return errors.New("smtp AUTH failed (check email.smtp username/password)")
	}
	return nil
}

func (c *smtpClient) upgradeTLS(ctx context.Context) error {
	conf := cloneTLSConfig(c.tlsConfig)
	// ServerName doubles as the verification hostname; Go's crypto/tls
	// never sends an IP literal as SNI but does verify it against IP SANs.
	conf.ServerName = c.cfg.Host
	conf.MinVersion = tls.VersionTLS12
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	tconn := tls.Client(c.conn, conf)
	if err := tconn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("smtp TLS: %w", err)
	}
	c.conn = tconn
	c.r = bufio.NewReader(tconn)
	return nil
}

// writeMessage emits headers plus the dot-stuffed body, ending the DATA
// block. Lines are normalized to CRLF and a leading dot is doubled, so a
// body line of "." can never fake the terminator.
func (c *smtpClient) writeMessage(ctx context.Context, to, subject, body string) error {
	mid := make([]byte, 16)
	if _, err := rand.Read(mid); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", c.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeader(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@mullsmith>\r\n", hex.EncodeToString(mid))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // body already ended with a newline
	}
	for _, line := range lines {
		line = strings.ReplaceAll(line, "\r", "")
		if strings.HasPrefix(line, ".") {
			b.WriteString(".") // dot-stuffing: a leading dot is doubled
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	b.WriteString(".\r\n")
	return c.write(ctx, b.String())
}

// --- wire plumbing --------------------------------------------------------

func (c *smtpClient) cmd(ctx context.Context, line string) (code int, text string, err error) {
	if err := c.write(ctx, line+"\r\n"); err != nil {
		return 0, "", err
	}
	return c.reply(ctx)
}

func (c *smtpClient) write(ctx context.Context, s string) error {
	dl, err := opDeadline(ctx)
	if err != nil {
		return err
	}
	if err := c.conn.SetDeadline(dl); err != nil {
		return err
	}
	_, err = c.conn.Write([]byte(s))
	return err
}

// reply reads one (possibly multi-line) SMTP response.
func (c *smtpClient) reply(ctx context.Context) (int, string, error) {
	dl, err := opDeadline(ctx)
	if err != nil {
		return 0, "", err
	}
	if err := c.conn.SetDeadline(dl); err != nil {
		return 0, "", err
	}
	var (
		code int
		text []string
	)
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 4 || line[3] != '-' && line[3] != ' ' {
			return 0, "", fmt.Errorf("malformed smtp reply %q", line)
		}
		code, err = strconv.Atoi(line[:3])
		if err != nil {
			return 0, "", fmt.Errorf("malformed smtp reply %q", line)
		}
		text = append(text, line[4:])
		if line[3] == ' ' {
			return code, strings.Join(text, "\n"), nil
		}
	}
}

func parseExtensions(txt string) map[string]bool {
	ext := map[string]bool{}
	for _, line := range strings.Split(txt, "\n")[1:] { // [0] was "host ..."
		fields := strings.Fields(strings.ToUpper(line))
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "AUTH" {
			for _, mech := range fields[1:] {
				if mech == "LOGIN" {
					ext["AUTH-LOGIN"] = true
				}
			}
			continue
		}
		ext[fields[0]] = true
	}
	return ext
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// safeAddr rejects anything that could inject extra protocol commands or
// headers through an address (CR/LF is the whole attack surface here).
func safeAddr(a string) error {
	if a == "" {
		return errors.New("empty address")
	}
	if strings.ContainsAny(a, "\r\n") {
		return errors.New("address contains a line break")
	}
	return nil
}

// sanitizeHeader strips line breaks from header values (subjects are ours,
// but defense in depth is cheap).
func sanitizeHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

func cloneTLSConfig(in *tls.Config) *tls.Config {
	if in == nil {
		return &tls.Config{}
	}
	return in.Clone()
}
