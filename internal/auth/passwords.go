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
		"correcthorsebattery", "gabkeep123", "thisismypassword", "mypassword1",
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

// ValidEmail does a pragmatic address check (syntax + domain dot + length).
func ValidEmail(email string) bool {
	if len(email) > MaxEmailLen {
		return false
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return false
	}
	at := strings.LastIndex(addr.Address, "@")
	if at < 0 {
		return false
	}
	domain := addr.Address[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	return true
}
