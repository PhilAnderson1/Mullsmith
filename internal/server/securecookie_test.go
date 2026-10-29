package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"mullsmith/internal/config"
)

// Cookies derive their Secure flag from the request, never from config
// (§9.1): TLS connection → Secure; trusted proxy declaring https browser
// leg → Secure; everything else (plain HTTP, spoofed header from an
// untrusted peer) → not Secure.
func TestSecureRequest(t *testing.T) {
	trusted := &config.Config{}
	p := netip.MustParsePrefix("10.0.0.0/24")
	trusted.TrustedNets = []netip.Prefix{p}
	plain := &config.Config{}

	mk := func(peer string, xfp string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = peer
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		return r
	}

	tlsReq := mk("1.2.3.4:55", "")
	tlsReq.TLS = &tls.ConnectionState{}

	cases := []struct {
		name string
		cfg  *config.Config
		req  *http.Request
		want bool
	}{
		{"direct tls", plain, tlsReq, true},
		{"plain http", plain, mk("1.2.3.4:55", ""), false},
		{"proxy https, trusted peer", trusted, mk("10.0.0.5:1234", "https"), true},
		{"proxy https, untrusted peer", plain, mk("1.2.3.4:55", "https"), false},
		{"trusted peer, no header", trusted, mk("10.0.0.5:1234", ""), false},
		{"trusted peer, http protocol", trusted, mk("10.0.0.5:1234", "http"), false},
		{"forwarded chain, first hop https", trusted, mk("10.0.0.5:1234", "https, http"), true},
	}
	for _, c := range cases {
		if got := secureRequest(c.cfg, c.req); got != c.want {
			t.Errorf("%s: secureRequest = %v, want %v", c.name, got, c.want)
		}
	}
}
