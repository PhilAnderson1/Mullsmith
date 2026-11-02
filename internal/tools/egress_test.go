package tools

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isBlockedLiteral applies the very predicate GuardedTransport dials by
// (an unparseable literal counts as blocked).
func isBlockedLiteral(s string) bool {
	a, err := netip.ParseAddr(s)
	return err != nil || isBlockedIP(a)
}

// fakeLookup injects DNS answers so the matrix exercises the blocklist
// without touching real DNS (and simulates rebinding-adjacent answer sets).
func fakeLookup(answers ...string) func(ctx context.Context, host string) ([]string, error) {
	return func(ctx context.Context, host string) ([]string, error) {
		return answers, nil
	}
}

// TestSSRFBlocklistMatrix walks the full §9.6 space: loopback, RFC1918,
// CGNAT, link-local/metadata, IPv6 loopback/ULA/link-local, multicast,
// reserved, benchmark, 6to4, IPv4-mapped — every one must be refused.
func TestSSRFBlocklistMatrix(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.8.8", // loopback
		"10.0.0.1", "172.16.5.5", "192.168.0.2", // RFC1918
		"100.64.0.1", "100.127.255.255", // CGNAT
		"169.254.169.254",          // cloud metadata
		"0.0.0.0",                  // unspecified
		"0.0.0.1", "0.255.255.255", // entire "this network" block
		"255.255.255.255",          // broadcast
		"224.0.0.1",                // multicast
		"198.18.0.1",               // benchmarking
		"240.0.0.1",                // reserved
		"192.0.0.1",                // IETF protocol assignments
		"192.88.99.1",              // 6to4 relay anycast
		"192.0.2.1", "192.0.2.255", // documentation
		"198.51.100.1", "203.0.113.1", // documentation
		"::1", "fc00::1", "fd12:3456::7", // IPv6 loopback + ULA
		"fe80::1",                            // IPv6 link-local
		"ff02::1",                            // IPv6 multicast
		"2002:7f00:1::1",                     // 6to4 embedding 127.0.0.1
		"::ffff:127.0.0.1",                   // IPv4-mapped loopback
		"::ffff:192.168.1.1",                 // IPv4-mapped private
		"::ffff:0.1.2.3", "::ffff:192.0.2.1", // mapped special-purpose IPv4
		"64:ff9b::a00:1", "64:ff9b:1::a00:1", // NAT64
		"100::1", "100:0:0:1::1", // discard-only, dummy prefix
		"2001::1", "2001:2::1", "2001:20::1", // Teredo, benchmarking, ORCHID
		"2001:db8::1", "3fff::1", "3fff:fff:ffff:ffff:ffff:ffff:ffff:ffff", // documentation
		"5f00::1", "4000::1", // SRv6, outside ordinary global unicast
		"2606:4700::1111%eth0", "::ffff:8.8.8.8%eth0", // scoped addresses, even otherwise public
	}
	for _, ip := range blocked {
		if !isBlockedLiteral(ip) {
			t.Errorf("%q must be blocked", ip)
		}
		// The same policy must apply to literal URLs and every DNS answer,
		// including a blocked answer mixed with an ordinary public address.
		guard := &GuardedTransport{LookupHost: fakeLookup("8.8.8.8", ip)}
		for _, host := range []string{ip, "mixed.example"} {
			if _, err := guard.validateHost(context.Background(), host); !errors.Is(err, errBlockedHost) {
				t.Errorf("%s via %s: error = %v, want blocked", ip, host, err)
			}
		}
	}
	allowed := []string{
		"8.8.8.8", "93.184.216.34", "::ffff:8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946", "2001:4860:4860::8888",
	}
	for _, ip := range allowed {
		if isBlockedLiteral(ip) {
			t.Errorf("%q must be allowed", ip)
		}
	}
}

func TestGuardedHostOwnAddresses(t *testing.T) {
	assigned := []net.Addr{
		&net.IPNet{IP: net.ParseIP("93.184.216.34"), Mask: net.CIDRMask(24, 32)},
		&net.IPAddr{IP: net.ParseIP("2606:4700::1111")},
	}
	guard := &GuardedTransport{
		InterfaceAddrs: func() ([]net.Addr, error) { return assigned, nil },
	}
	for _, ip := range []string{"93.184.216.34", "::ffff:93.184.216.34", "2606:4700::1111"} {
		guard.LookupHost = fakeLookup("8.8.8.8", ip)
		for _, host := range []string{ip, "own.example"} {
			if _, err := guard.validateHost(context.Background(), host); !errors.Is(err, errBlockedHost) {
				t.Fatalf("own address %s via %s was not blocked: %v", ip, host, err)
			}
		}
	}
	// Assignment changes must take effect without recreating the client.
	assigned = nil
	if _, err := guard.validateHost(context.Background(), "93.184.216.34"); err != nil {
		t.Fatalf("ordinary remote address blocked: %v", err)
	}
	assigned = []net.Addr{&net.IPAddr{IP: net.ParseIP("93.184.216.34")}}
	if _, err := guard.validateHost(context.Background(), "93.184.216.34"); !errors.Is(err, errBlockedHost) {
		t.Fatalf("newly assigned address not blocked: %v", err)
	}
	guard.InterfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("unavailable") }
	if _, err := guard.validateHost(context.Background(), "8.8.8.8"); !errors.Is(err, errBlockedHost) {
		t.Fatalf("interface inspection failure did not fail closed: %v", err)
	}
}

type egressRoundTripper func(*http.Request) (*http.Response, error)

func (f egressRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGuardedRedirectSpecialAndOwnAddresses(t *testing.T) {
	for _, target := range []string{
		"http://0.1.2.3/file", "http://192.0.2.1/file", "http://198.51.100.1/file",
		"http://[64:ff9b::a00:1]/file", "http://[2001:db8::1]/file",
		"http://[2606:4700::1111%25eth0]/file", "http://93.184.216.34/file", "http://own.example/file",
	} {
		t.Run(target, func(t *testing.T) {
			guard := &GuardedTransport{
				LookupHost: fakeLookup("93.184.216.34"),
				InterfaceAddrs: func() ([]net.Addr, error) {
					return []net.Addr{&net.IPAddr{IP: net.ParseIP("93.184.216.34")}}, nil
				},
			}
			client := NewGuardedClient(time.Second)
			var requests int
			// Model a public redirector without connecting to any external host.
			// The redirected dial runs the real guard; even a regression cannot
			// create an external connection because Control refuses it.
			guard.Dialer = &net.Dialer{Control: func(_, _ string, _ syscall.RawConn) error {
				t.Error("blocked redirect reached the socket dial")
				return errors.New("unexpected dial")
			}}
			client.Transport = egressRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				port := r.URL.Port()
				if port == "" {
					port = "80"
				}
				conn, err := guard.DialContext(r.Context(), "tcp", net.JoinHostPort(r.URL.Hostname(), port))
				if conn != nil {
					conn.Close()
				}
				return nil, err
			})
			resp, err := client.Get("http://redirector.example/start")
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, errBlockedHost) || requests != 2 {
				t.Fatalf("redirect: %d requests, error = %v", requests, err)
			}
		})
	}
}

// dialThrough runs the guarded dialer for host:port with injected DNS.
func dialThrough(tr *GuardedTransport, host string) error {
	_, err := tr.DialContext(context.Background(), "tcp", net.JoinHostPort(host, "80"))
	return err
}

func TestGuardedDialRefusesPrivateAnswers(t *testing.T) {
	for _, tc := range []struct{ name, ip string }{
		{"loopback", "127.0.0.1"},
		{"rfc1918", "10.1.2.3"},
		{"cgnat", "100.64.5.6"},
		{"metadata", "169.254.169.254"},
		{"ipv6-loopback", "::1"},
		{"ipv6-ula", "fd00::dead:beef"},
	} {
		tr := &GuardedTransport{LookupHost: fakeLookup(tc.ip)}
		err := dialThrough(tr, "evil.example")
		if !errors.Is(err, errBlockedHost) {
			t.Errorf("%s: err = %v, want errBlockedHost", tc.name, err)
		}
	}
}

func TestGuardedDialPoisonedAnswerSet(t *testing.T) {
	// A public answer alongside a private one must fail closed: the
	// attacker cannot race us into dialing the good IP.
	tr := &GuardedTransport{LookupHost: fakeLookup("93.184.216.34", "127.0.0.1")}
	if err := dialThrough(tr, "evil.example"); !errors.Is(err, errBlockedHost) {
		t.Fatalf("err = %v, want errBlockedHost", err)
	}
}

func TestGuardedDialLiteralIP(t *testing.T) {
	tr := &GuardedTransport{} // LookupHost must not even be consulted
	if err := dialThrough(tr, "127.0.0.1"); !errors.Is(err, errBlockedHost) {
		t.Fatalf("literal loopback: err = %v, want errBlockedHost", err)
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "[::1]:80"); !errors.Is(err, errBlockedHost) {
		t.Fatalf("literal ::1: err = %v, want errBlockedHost", err)
	}
}

func TestGuardedDialEmptyAnswers(t *testing.T) {
	tr := &GuardedTransport{LookupHost: fakeLookup()}
	if err := dialThrough(tr, "nowhere.example"); !errors.Is(err, errBlockedHost) {
		t.Fatalf("err = %v, want errBlockedHost", err)
	}
}

// TestGuardedClientBlocksLoopbackServer proves the shipped client cannot be
// pointed at the machine it runs on, even when the target is genuinely
// listening (the classic SSRF shape: http://127.0.0.1:PORT/admin).
func TestGuardedClientBlocksLoopbackServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("guarded client reached the loopback server")
	}))
	defer srv.Close()

	client := NewGuardedClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("loopback fetch succeeded, want refusal")
	}
	// httptest URLs are 127.0.0.1 literals: must be the policy refusal,
	// not an incidental connection error.
	if !strings.Contains(err.Error(), "not publicly routable") {
		t.Fatalf("err = %v, want the blocklist refusal", err)
	}
}

// TestGuardedClientRedirectToPrivate is the redirect-hop re-validation
// case: a public-ish front (localhost here stands in for the attacker's
// public host, reachable only because we relax the literal check for the
// first hop via a custom transport) 302s into loopback. The dialer sees
// every hop, so the second one must die.
func TestGuardedClientRedirectToPrivate(t *testing.T) {
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect landed on the private target")
	}))
	defer private.Close()

	// Front server lives on loopback too — dial it through a *relaxed*
	// transport so hop 1 succeeds, hop 2 (Location: private) goes through
	// the guarded dialer again. We emulate "attacker's public redirector"
	// by wrapping: first address checked is allowed unconditionally.
	var hop int
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			hop++
			if hop == 1 {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			return (&GuardedTransport{}).DialContext(ctx, network, addr)
		},
	}
	client := NewGuardedClient(5 * time.Second)
	client.Transport = tr

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/secret", http.StatusFound)
	}))
	defer front.Close()

	resp, err := client.Get(front.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect to private target succeeded, want refusal")
	}
	if hop < 2 {
		t.Fatalf("only %d dial(s): the redirect target was never re-validated", hop)
	}
}

func TestGuardedClientNoCookieJar(t *testing.T) {
	c := NewGuardedClient(5 * time.Second)
	if c.Jar != nil {
		t.Fatal("guarded client must carry no cookie jar (§9.6)")
	}
}

func TestRedirectCapAndScheme(t *testing.T) {
	c := NewGuardedClient(5 * time.Second)
	req, _ := http.NewRequest("GET", "http://public.example/", nil)
	// 3 hops already done → a 4th is refused (≤3 redirects, §7.2).
	via := make([]*http.Request, 0, 4)
	for i := 0; i < 3; i++ {
		r := req.Clone(context.Background())
		via = append(via, r)
	}
	next, _ := http.NewRequest("GET", "http://next.example/", nil)
	if err := c.CheckRedirect(next, via); err != nil {
		t.Fatalf("3rd redirect: err = %v, want allowed", err)
	}
	via = append(via, next)
	if err := c.CheckRedirect(next, via); err == nil {
		t.Fatal("4th redirect allowed, want refusal")
	}
	// Scheme downgrade must be refused even within the hop budget.
	bad, _ := http.NewRequest("GET", "gopher://x/", nil)
	via2 := via[:1]
	if err := c.CheckRedirect(bad, via2); err == nil {
		t.Fatal("redirect to gopher:// allowed, want refusal")
	}
}
