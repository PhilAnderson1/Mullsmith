package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// --- audit2 finding 1: login must not retain attacker-sized emails ---------

func loginStatusAndCode(t *testing.T, a *authTest, email string) (int, string) {
	t.Helper()
	resp := a.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": email, "password": "whatever-1234"})
	defer resp.Body.Close()
	var env map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&env)
	code := ""
	if e, ok := env["error"].(map[string]any); ok {
		code, _ = e["code"].(string)
	}
	return resp.StatusCode, code
}

func TestLoginRejectsOversizedEmail(t *testing.T) {
	a, s := pwTestApp(t, authCfg(t))

	for _, n := range []int{255, 1000, 64 << 10} {
		email := strings.Repeat("a", n-5) + "@x.io"
		if len(email) != n && n <= 255 {
			t.Fatalf("fixture wrong length %d", len(email))
		}
		if len(email) > n {
			email = email[:n]
		}
		status, code := loginStatusAndCode(t, a, email)
		if status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("email of %d bytes: got %d/%s, want 400/invalid_request", len(email), status, code)
		}
	}
	if n := s.locks.Len(); n != 0 {
		t.Errorf("%d lock entries retained after oversized attempts — must be zero", n)
	}
}

// The boundary belongs to the normal path: a 254-byte email is refused as
// unknown (401), never as too long (400).
func TestLoginEmailLengthBoundary(t *testing.T) {
	a, _ := pwTestApp(t, authCfg(t))
	at := strings.Repeat("l", 10) + "@"
	email := at + strings.Repeat("d", 254-len(at)-3) + ".io"
	if len(email) != 254 {
		t.Fatalf("fixture = %d bytes, want 254", len(email))
	}
	if status, code := loginStatusAndCode(t, a, email); status == http.StatusBadRequest {
		t.Errorf("254-byte email rejected as too long (code %s)", code)
	} else if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for unknown email", status)
	}
}
