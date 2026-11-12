package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// GuardedTransport is the single guarded egress path for model-initiated
// requests (spec §9.6): every hostname is resolved first, every returned
// address is checked against a private/reserved blocklist, and the TCP
// connect uses an already-validated IP (pinned), which closes the
// DNS-rebinding TOCTOU window. Redirects are re-checked because every hop
// dials through this transport again.
type GuardedTransport struct {
	// LookupHost resolves a hostname; tests inject fakes to exercise the
	// rebinding and multi-answer cases. Defaults to net.DefaultResolver.
	LookupHost func(ctx context.Context, host string) ([]string, error)
	// InterfaceAddrs defaults to net.InterfaceAddrs. Read afresh for every
	// connection so newly assigned host addresses are blocked too.
	InterfaceAddrs func() ([]net.Addr, error)
	Dialer         *net.Dialer // nil ⇒ 10 s dial timeout
}

var (
	errBlockedHost  = errors.New("target address is not publicly routable")
	errHostNotFound = errors.New("target host could not be resolved")
)

// blockedCIDRs complements netip's predicates using the IANA special-purpose
// registries. Deliberately exclude whole protocol/transition ranges, including
// their public exceptions: these tools need ordinary public web destinations.
var blockedCIDRs = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network", not just 0.0.0.0
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("2001::/23"),       // IETF assignments, Teredo, benchmarking, ORCHID
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4 (can embed private)
	netip.MustParsePrefix("3fff::/20"),       // documentation
}

var publicIPv6 = netip.MustParsePrefix("2000::/3")

func isBlockedIP(addr netip.Addr) bool {
	if addr.Zone() != "" {
		return true // scoped addresses have no public-web use
	}
	a := addr.Unmap() // IPv4-mapped IPv6 (::ffff:x.x.x.x) is judged as v4
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsMulticast() ||
		a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() {
		return true
	}
	// IsGlobalUnicast also accepts reserved/unallocated space. In particular,
	// exclude translation prefixes (NAT64), discard-only and SRv6 addresses.
	if a.Is6() && !publicIPv6.Contains(a) {
		return true
	}
	for _, p := range blockedCIDRs {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (t *GuardedTransport) lookup(ctx context.Context, host string) ([]string, error) {
	if t.LookupHost != nil {
		return t.LookupHost(ctx, host)
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

// validateHost resolves and validates every answer; returns the safe IPs.
func (t *GuardedTransport) validateHost(ctx context.Context, host string) ([]netip.Addr, error) {
	var hosts []string
	if ip, err := netip.ParseAddr(host); err == nil {
		hosts = []string{ip.String()}
	} else {
		var err error
		hosts, err = t.lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, errHostNotFound)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("resolve %q: %w", host, errHostNotFound)
	}
	out := make([]netip.Addr, 0, len(hosts))
	for _, h := range hosts {
		ip, err := netip.ParseAddr(h)
		if err != nil || isBlockedIP(ip) {
			return nil, errBlockedHost // any private answer poisons the set
		}
		out = append(out, ip)
	}
	interfaceAddrs := t.InterfaceAddrs
	if interfaceAddrs == nil {
		interfaceAddrs = net.InterfaceAddrs
	}
	locals, err := interfaceAddrs()
	if err != nil {
		return nil, errBlockedHost // cannot establish that the target is remote
	}
	for _, local := range locals {
		prefix, err := netip.ParsePrefix(local.String())
		ip := prefix.Addr()
		if err != nil {
			ip, err = netip.ParseAddr(local.String())
		}
		if err != nil {
			return nil, errBlockedHost
		}
		for _, candidate := range out {
			if candidate.Unmap() == ip.WithZone("").Unmap() {
				return nil, errBlockedHost
			}
		}
	}
	return out, nil
}

// DialContext implements the pinned-dial contract: validate → connect to
// the validated literal, never to the hostname again.
func (t *GuardedTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := t.validateHost(ctx, host)
	if err != nil {
		return nil, err
	}
	d := t.Dialer
	if d == nil {
		d = &net.Dialer{Timeout: 10 * time.Second}
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errBlockedHost
	}
	return nil, lastErr
}

// NewGuardedClient assembles the shared egress http.Client (§9.6): guarded
// dial, ≤3 redirects, no cookies, no ambient auth, per-request timeout.
func NewGuardedClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:           (&GuardedTransport{}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          8,
		DisableKeepAlives:     false,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		Jar:       nil, // never carries cookies (§9.6)
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to unsupported scheme")
			}
			return nil // dial re-validates the target IP (§9.6)
		},
	}
}
