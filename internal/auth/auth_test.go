package auth

import (
	"strings"
	"testing"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/id"
)

func testArgon() config.Argon2 {
	// Fast parameters for tests only.
	return config.Argon2{Time: 1, Memory: 1024, Threads: 1}
}

func TestArgonRoundtrip(t *testing.T) {
	p := testArgon()
	h, err := HashPassword("correct horse battery", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Errorf("unexpected encoding: %s", h)
	}
	if !VerifyPassword("correct horse battery", h) {
		t.Error("valid password rejected")
	}
	if VerifyPassword("wrong", h) {
		t.Error("invalid password accepted")
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, bad := range []string{"", "x", "$argon2id", "bcrypt$1$2$3", "$argon2id$v=99$m=1,t=1,p=1$AA$BB"} {
		if VerifyPassword("anything", bad) {
			t.Errorf("malformed hash accepted: %q", bad)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	if err := CheckPasswordPolicy("short"); err == nil {
		t.Error("short password accepted")
	}
	if err := CheckPasswordPolicy("password123"); err == nil {
		t.Error("common password accepted")
	}
	if err := CheckPasswordPolicy(" trailing space1"); err == nil {
		t.Error("leading whitespace accepted")
	}
	if err := CheckPasswordPolicy("c3rtainly-unlikely-pw"); err != nil {
		t.Errorf("good password rejected: %v", err)
	}
}

func TestValidEmail(t *testing.T) {
	for _, ok := range []string{"a@b.co", "first.last+tag@sub.example.com"} {
		if !ValidEmail(ok) {
			t.Errorf("should be valid: %s", ok)
		}
	}
	for _, bad := range []string{"", "no-at", "a@b", "a@b.", "a b@c.de", "@b.co", strings.Repeat("x", 300) + "@b.co"} {
		if ValidEmail(bad) {
			t.Errorf("should be invalid: %q", bad)
		}
	}
}

// --- audit1 L-3: canonical form, not just a verdict ----------------------

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"a@b.co":                            "a@b.co",
		"Phil@Example.COM":                  "phil@example.com",               // domain AND local fold
		"PayPal Support <PHil@ExAmPlE.COM>": "phil@example.com",               // display-name junk stripped
		"first.last+tag@sub.example.com":    "first.last+tag@sub.example.com", // +detail survives
	} {
		got, ok := NormalizeEmail(in)
		if !ok || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	// Same verdict table as ValidEmail — and the variants above now prove
	// the round trip: valid AND already canonical.
	for _, bad := range []string{"", "no-at", "a@b", "a@b.", "a b@c.de", "@b.co", strings.Repeat("x", 300) + "@b.co"} {
		if got, ok := NormalizeEmail(bad); ok {
			t.Errorf("should be invalid: %q -> %q", bad, got)
		}
	}
}

func TestSessionTokens(t *testing.T) {
	raw, hash, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || len(raw) < 40 {
		t.Errorf("token too short: %q", raw)
	}
	if string(hash) == string(raw) {
		t.Error("hash must differ from raw")
	}
	if string(HashToken(raw)) != string(hash) {
		t.Error("HashToken not deterministic")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(nil)
	window := 100 * time.Millisecond
	for i := 0; i < 3; i++ {
		if !l.Allow("k", 3, window) {
			t.Fatalf("token %d denied unexpectedly", i)
		}
	}
	if l.Allow("k", 3, window) {
		t.Error("4th token within window should be denied")
	}
	time.Sleep(120 * time.Millisecond)
	if !l.Allow("k", 3, window) {
		t.Error("refill failed")
	}
}

func TestLoginLocks(t *testing.T) {
	l := NewLoginLocks(nil)
	if l.Locked("u") {
		t.Error("fresh key locked")
	}
	for i := 0; i < 3; i++ {
		l.Fail("u", 3, time.Hour)
	}
	if !l.Locked("u") {
		t.Error("not locked after threshold")
	}
	l.Reset("u")
	if l.Locked("u") {
		t.Error("still locked after reset")
	}
}

func TestIDULID(t *testing.T) {
	a, b := id.New(), id.New()
	if a == b {
		t.Error("collision")
	}
	if len(a) != 26 {
		t.Errorf("length = %d", len(a))
	}
	if !id.Valid(a) {
		t.Error("generated id fails Valid")
	}
	for _, bad := range []string{"", "short", strings.Repeat("U", 26), strings.Repeat("!", 26), strings.Repeat("A", 25)} {
		if id.Valid(bad) {
			t.Errorf("Valid accepted %q", bad)
		}
	}
}
