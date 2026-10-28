package mail

// The three transactional messages (plan §4). Plain text only; the link
// goes on its own line so mail clients never wrap it into brokenness.

func confirmLink(baseURL, token string) string {
	return baseURL + "/#confirm?t=" + token
}

func resetLink(baseURL, token string) string {
	return baseURL + "/#reset?t=" + token
}

// ConfirmationMail activates a sign-up or applies a Settings address
// change. The 24 h deadline mirrors the pending-expiry sweep: link and
// account die together.
func ConfirmationMail(baseURL, token string) (subject, body string) {
	return "Confirm your gabkeep account",
		"Confirm this email address to activate your gabkeep account:\n" +
			"\n" +
			"    " + confirmLink(baseURL, token) + "\n" +
			"\n" +
			"The link works once and expires within 24 hours — after that the\n" +
			"pending account is deleted automatically and you can sign up again.\n" +
			"\n" +
			"If you did not create this account, ignore this message.\n"
}

// ResetMail sets a new password. Completing it signs every existing
// session out, which the text says so the user is not surprised.
func ResetMail(baseURL, token string) (subject, body string) {
	return "Reset your gabkeep password",
		"Someone asked to reset the password on your gabkeep account.\n" +
			"\n" +
			"    " + resetLink(baseURL, token) + "\n" +
			"\n" +
			"The link works once and expires within 30 minutes. Choosing a new\n" +
			"password signs every device out.\n" +
			"\n" +
			"If this wasn't you, ignore this message — nothing changes.\n"
}

// AddressChangedMail is the courtesy notice to the OLD address when the
// account's email moves to a new one.
func AddressChangedMail() (subject, body string) {
	return "Your gabkeep account email changed",
		"The email address on your gabkeep account was changed.\n" +
			"\n" +
			"You are receiving this notice at your previous address as a\n" +
			"security courtesy. If you did not make this change, reset your\n" +
			"password immediately (\"Forgot password?\" on the sign-in page)\n" +
			"— that signs every device out.\n"
}
