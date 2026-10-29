package mail

// The transactional messages (plan §4). Plain text only; the link goes on
// its own line so mail clients never wrap it into brokenness.

func confirmLink(baseURL, token string) string {
	return baseURL + "/#confirm?t=" + token
}

func resetLink(baseURL, token string) string {
	return baseURL + "/#reset?t=" + token
}

// ConfirmationMail activates a sign-up. The 24 h deadline mirrors the
// pending-expiry sweep: link and pending account die together.
func ConfirmationMail(baseURL, token string) (subject, body string) {
	return "Confirm your mullsmith account",
		"Confirm this email address to activate your mullsmith account:\n" +
			"\n" +
			"    " + confirmLink(baseURL, token) + "\n" +
			"\n" +
			"The link works once and expires within 24 hours — after that the\n" +
			"pending account is deleted automatically and you can sign up again.\n" +
			"\n" +
			"If you did not create this account, ignore this message.\n"
}

// ConfirmationMailChange is the Settings-flow sibling: identical token
// machinery, different promise — nothing gets deleted when this one
// expires, the account simply keeps its current address. Wording must
// never read like an account activation (field feedback: the two flows
// arrived indistinguishable).
func ConfirmationMailChange(baseURL, token string) (subject, body string) {
	return "Confirm your mullsmith email address change",
		"Confirm this email address to activate your Mullsmith email address change:\n" +
			"\n" +
			"    " + confirmLink(baseURL, token) + "\n" +
			"\n" +
			"The link works once and expires within 24 hours. Until it is clicked,\n" +
			"your account keeps signing in with its current address.\n" +
			"\n" +
			"If you did not request this change, ignore this message — nothing\n" +
			"changes until this link is clicked; your current address was\n" +
			"notified as well.\n"
}

// ResetMail sets a new password. Completing it signs every existing
// session out, which the text says so the user is not surprised.
func ResetMail(baseURL, token string) (subject, body string) {
	return "Reset your mullsmith password",
		"Someone asked to reset the password on your mullsmith account.\n" +
			"\n" +
			"    " + resetLink(baseURL, token) + "\n" +
			"\n" +
			"The link works once and expires within 30 minutes. Choosing a new\n" +
			"password signs every device out.\n" +
			"\n" +
			"If this wasn't you, ignore this message — nothing changes.\n"
}

// AddressChangedMail is the security notice to the OLD address, sent the
// moment a change is REQUESTED — before anything moves. Warning early is
// the whole point (a hijacked cookie surfaces the same minute it strikes);
// the wording therefore never claims the change has happened, because at
// send time it has not.
func AddressChangedMail() (subject, body string) {
	return "Your mullsmith account email change was requested",
		"A change of the email address on your mullsmith account was requested.\n" +
			"Nothing has changed yet: the move takes effect only when the new\n" +
			"address confirms through the link sent to it.\n" +
			"\n" +
			"You are receiving this notice at your current address as a security\n" +
			"courtesy. If you did not request this, cancel the pending change on\n" +
			"the Settings page and reset your password immediately (\"Forgot\n" +
			"password?\" on the sign-in page) — that signs every device out.\n"
}
