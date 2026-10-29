package auth

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// ErrWeakPassword is returned for policy violations; the message is
// user-presentable (no account-existence information).
var ErrWeakPassword = errors.New("password too weak")

const minPasswordLen = 10

// CheckPasswordPolicy enforces spec §5.1: >= 10 characters and not a known
// common password.
func CheckPasswordPolicy(pw string) error {
	if len(pw) < minPasswordLen {
		return fmt.Errorf("%w: at least %d characters", ErrWeakPassword, minPasswordLen)
	}
	if strings.TrimSpace(pw) != pw {
		return fmt.Errorf("%w: leading/trailing whitespace", ErrWeakPassword)
	}
	if isCommon(strings.ToLower(pw)) {
		return fmt.Errorf("%w: too common", ErrWeakPassword)
	}
	return nil
}

// commonPasswords is a small embedded deny-list; v1.1 candidate: a larger
// breached-password list shipped as an embedded file.
var commonList = map[string]bool{}

func init() {
	for _, p := range []string{
		"password", "password1", "password123", "passw0rd", "p@ssw0rd",
		"qwertyuiop", "qwerty123", "abc123456", "1234567890", "12345678901",
		"iloveyou", "letmein123", "welcome123", "admin1234", "administrator",
		"changeme123", "trustno1!", "dragon123", "monkey123", "shadow123",
		"sunshine1", "princess1", "football1", "baseball1", "superman1",
		"00000000000", "aaaaaaaaaa", "password1234", "passwort123", "motdepasse1",
		"correcthorsebattery", "mullsmith123", "thisismypassword", "mypassword1",
	} {
		commonList[p] = true
	}
}

func isCommon(lowered string) bool { return commonList[lowered] }

// MaxEmailLen is the RFC 5321 upper bound for a full address. Login and
// registration enforce the same ceiling (login historically did not —
// audit2 finding 1, where unvalidated oversized emails were retained as
// account-lock keys).
const MaxEmailLen = 254

// NormalizeEmail parses an address and returns its canonical form: the
// bare addr-spec with display names and comments stripped, lowercased
// (audit1 L-3 — previously ValidEmail checked addr.Address but callers
// stored the raw string, so "PayPal Support <a@b.com>" and "A@B.COM"
// became separate accounts for one mailbox). Doctrine:
//
//   - every email WRITE and every email LOOKUP passes through here;
//     equality is plain binary once both sides are canonical (audit1 L-3);
//   - case is folded in the local part too. RFC 5321 says it MAY be
//     significant; every large provider ignores it, and case-split
//     accounts are the very bypass being closed;
//   - "+detail" tags are NOT folded: people who plus-alias use them
//     deliberately as distinct mailboxes;
//   - input longer than MaxEmailLen is refused BEFORE parsing, so the
//     parser never eats attacker-sized strings (audit2 finding 1).
//
// Valid things that are NOT already canonical (display names, capitals)
// normalize instead of failing; genuinely broken input reports ok=false.
func NormalizeEmail(raw string) (string, bool) {
	if len(raw) > MaxEmailLen {
		return "", false
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return "", false
	}
	norm := strings.ToLower(addr.Address)
	at := strings.LastIndex(norm, "@")
	if at < 0 {
		return "", false
	}
	domain := norm[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", false
	}
	return norm, true
}

// ValidEmail is the verdict-only form of NormalizeEmail.
func ValidEmail(raw string) bool {
	_, ok := NormalizeEmail(raw)
	return ok
}
