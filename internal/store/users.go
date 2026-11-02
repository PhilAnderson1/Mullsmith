package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/id"
)

// Sentinel errors surfaced to handlers (mapped to safe API codes).
var (
	ErrUserExists    = errors.New("email already registered")
	ErrInviteInvalid = errors.New("invite code missing, used or unknown")
	ErrUserNotFound  = errors.New("user not found")
)

// User is a users-table row.
type User struct {
	ID   string
	Name string
	// Email is the CONFIRMED address ("" = none yet): the only address
	// login, forgot-password and every credential-facing lookup sees.
	// NULL in the database — pending sign-ups prove ownership of nothing
	// until their confirm link is clicked.
	Email string
	// UnconfirmedEmail is the address awaiting a confirmation click
	// ("" = nothing pending): set at sign-up (email_features on) and by a
	// Settings address change; ConfirmEmail copies it into Email.
	UnconfirmedEmail string
	PasswordHash     string
	Status           string
	// DefaultSystemPrompt seeds new conversations created without an
	// explicit prompt (settings page); existing chats keep their own.
	DefaultSystemPrompt string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

const userCols = `id, email, name, password_hash, status, default_system_prompt, unconfirmed_email, created_at, updated_at`

// nilIfEmpty maps "" to a SQL NULL: the email columns store absence as
// NULL, never as the empty string (which would collide under UNIQUE).
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateUser inserts a user row directly. Production paths never call it:
// registration goes through Register (which also consumes an invite and
// seeds the first conversation). Test fixtures use it precisely for that
// difference — a user with no side effects, so conversation-counting
// tests start from an empty thread list.
func (d *DB) CreateUser(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = id.New()
	}
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO users(id, email, name, password_hash, status, unconfirmed_email, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, nilIfEmpty(u.Email), u.Name, u.PasswordHash, u.Status,
		nilIfEmpty(u.UnconfirmedEmail), u.CreatedAt.Unix(), u.UpdatedAt.Unix())
	if isUnique(err, "users.email") {
		return ErrUserExists
	}
	return err
}

// UserByEmail returns nil, nil when no user matches.
func (d *DB) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(d.sqlDB.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE email = ?`, email))
}

// UserByID returns nil, nil when no user matches.
func (d *DB) UserByID(ctx context.Context, userID string) (*User, error) {
	return scanUser(d.sqlDB.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = ?`, userID))
}

func scanUser(row row) (*User, error) {
	var (
		u              User
		email, pending sql.NullString
		created, seen  int64
	)
	err := row.Scan(&u.ID, &email, &u.Name, &u.PasswordHash, &u.Status,
		&u.DefaultSystemPrompt, &pending, &created, &seen)
	switch {
	case errors.Is(err, ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	u.Email = email.String
	u.UnconfirmedEmail = pending.String
	u.CreatedAt = time.Unix(created, 0)
	u.UpdatedAt = time.Unix(seen, 0)
	return &u, nil
}

// UserIDs lists every existing user id — the maintenance sweep's liveness
// set (spec §8): a scratchpad ULID not in this list belongs to nobody and
// its aged bytes are sweepable.
func (d *DB) UserIDs(ctx context.Context) ([]string, error) {
	rows, err := d.sqlDB.QueryContext(ctx, `SELECT id FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetPassword updates the hash, cancels pending address changes and links, and
// bumps updated_at, in one transaction. Changing the password is the owner's
// attempt to evict an uninvited visitor — a mailbox-derived link that can
// overwrite the fresh credential would silently undo that attempt (astra2
// review: reset links survived password and email changes). Same cut
// DisableUser makes wholesale, scoped to this credential; covers both the
// Settings change and the admin CLI reset.
func (d *DB) SetPassword(ctx context.Context, userID, hash string, now time.Time) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, now.Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	if err := revokePasswordLinks(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrStalePassword surfaces from ChangePasswordAndRotate when the stored hash
// no longer matches the one the caller verified: a concurrent change (another
// Settings tab, an admin reset, a completed mail link) beat this rotation,
// and applying it now would silently overwrite the fresher credential.
var ErrStalePassword = errors.New("password changed since it was verified")

// PasswordRotation is the durable half of a rotated session: the raw token
// belongs ONLY to the response cookie, which the handler may publish after
// the transaction commits.
type PasswordRotation struct {
	Raw       string
	ExpiresAt time.Time
}

// ChangePasswordAndRotate commits the whole security transition of a
// password change in ONE transaction (C4): the new hash — guarded by the
// exact hash the handler verified, so a simultaneous change cannot be
// silently overwritten — the revocation of outstanding reset links
// (SetPassword's established policy, made explicit here), the revocation of
// every OTHER session, the deletion of the current one, and the insertion of
// its replacement. Before commit, any failure leaves nothing changed; after
// commit, no old session and no mailbox-derived link survives the fresh
// credential. The handler prepares the hash and expiry before entering and
// publishes the cookie only once this returns.
func (d *DB) ChangePasswordAndRotate(ctx context.Context, userID, newHash, verifiedHash string,
	currentSessionHash []byte, remembered bool, expiresAt, now time.Time, userAgent string,
) (*PasswordRotation, error) {
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		return nil, err
	}
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ? AND password_hash = ?`,
		newHash, now.Unix(), userID, verifiedHash)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish the vanished account from a raced change: one
		// reads as 401-worthy, the other as "try again", never both.
		var one int
		if qerr := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one); errors.Is(qerr, ErrNoRows) {
			return nil, ErrUserNotFound
		} else if qerr != nil {
			return nil, qerr
		}
		return nil, ErrStalePassword
	}
	// Policy (astra2, kept from SetPassword): a password change cuts every
	// mailbox-derived path that could overwrite the fresh credential.
	if err := revokePasswordLinks(ctx, tx, userID); err != nil {
		return nil, err
	}
	// Every other device is out (spec §9.1)…
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`, userID, currentSessionHash); err != nil {
		return nil, err
	}
	// …and the caller's own session is rotated, never kept: it predates
	// the credential the request just proved.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, currentSessionHash); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions(id_hash, user_id, created_at, expires_at, remembered, last_seen_at, user_agent)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		hash, userID, now.Unix(), expiresAt.Unix(), remembered, now.Unix(), userAgent); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &PasswordRotation{Raw: raw, ExpiresAt: expiresAt}, nil
}

// revokePasswordLinks cuts recovery paths in the password transition itself.
// Pending signup confirmation is preserved: it establishes the first address,
// whereas a pending change on an established account must be cancelled.
func revokePasswordLinks(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET unconfirmed_email = NULL WHERE id = ? AND email IS NOT NULL`, userID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE user_id = ? AND used_at IS NULL
		 AND (kind = ? OR (kind = ? AND EXISTS (SELECT 1 FROM users WHERE id = ? AND email IS NOT NULL)))`,
		userID, TokenResetPassword, TokenConfirmEmail, userID)
	return err
}

// UpdateProfile changes display details only. A stale metadata save must never
// restore an old email address after a concurrent confirmation or recovery.
func (d *DB) UpdateProfile(ctx context.Context, userID, name, defaultPrompt string, now time.Time) error {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE users SET name = ?, default_system_prompt = ?, updated_at = ? WHERE id = ?`, name, defaultPrompt, now.Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ChangeEmail commits the password-verified address change, display details and
// confirmation link together. A zero confirmation expiry selects a direct change
// (email features off). Every authorization predicate is checked inside the same
// transaction, so a recovery that won the race cannot be undone by this request.
func (d *DB) ChangeEmail(ctx context.Context, verified *User, sessionHash []byte,
	name, email, prompt string, confirmationExpiry, now time.Time,
) (string, error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM users u JOIN sessions s ON s.user_id = u.id
		 WHERE u.id = ? AND u.password_hash = ? AND u.email = ? AND u.status = ?
		 AND s.id_hash = ? AND s.expires_at > ?`,
		verified.ID, verified.PasswordHash, verified.Email, StatusEnabled, sessionHash, now.Unix()).Scan(&one)
	if errors.Is(err, ErrNoRows) {
		return "", ErrStaleCredentials
	}
	if err != nil {
		return "", err
	}
	available, err := emailAvailable(ctx, tx, email, verified.ID, now)
	if err != nil {
		return "", err
	}
	if !available {
		return "", ErrUserExists
	}
	confirmed, pending := email, ""
	if !confirmationExpiry.IsZero() {
		confirmed, pending = verified.Email, email
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET name = ?, email = ?, default_system_prompt = ?, unconfirmed_email = ?, updated_at = ? WHERE id = ?`,
		name, confirmed, prompt, nilIfEmpty(pending), now.Unix(), verified.ID); err != nil {
		if isUnique(err, "users.email") {
			return "", ErrUserExists
		}
		return "", err
	}
	var raw string
	if pending != "" {
		raw, err = createToken(ctx, tx, TokenConfirmEmail, verified.ID, pending, confirmationExpiry)
		if err != nil {
			return "", err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM auth_tokens WHERE user_id = ? AND used_at IS NULL
			 AND (kind = ? OR (kind = ? AND email <> ?))`,
			verified.ID, TokenConfirmEmail, TokenResetPassword, email); err != nil {
			return "", err
		}
	}
	return raw, tx.Commit()
}

// Account-lifecycle values for users.status (spec §9.17).
// StatusUnconfirmed is a pending sign-up: email NULL, address parked in
// unconfirmed_email, invisible to login and every address-keyed lookup.
// StatusEnabled is what a session may use. StatusDisabled is reserved
// for administrative suspension — nothing sets it except that admin
// action, and reactivation never happens implicitly (outstanding links
// must not resurrect a suspended account).
const (
	StatusEnabled     = "enabled"
	StatusUnconfirmed = "unconfirmed"
	StatusDisabled    = "disabled"
)

// DisableUser suspends an account (admin disable-user): status
// 'disabled', every session revoked and every outstanding one-shot token
// revoked, atomically. Login and the auth middleware refuse anything but
// 'enabled', so sessions die on the suspended user's next request even
// without the delete; the token cut is the essential half — a reset or
// confirmation link issued BEFORE the suspension must not resurrect the
// account afterwards (spec §9.17). Only an enabled account can be
// suspended: pending sign-ups are already unusable and sweep themselves
// (false answers "nothing changed", the caller explains via the status).
func (d *DB) DisableUser(ctx context.Context, userID string) (bool, error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		StatusDisabled, time.Now().Unix(), userID, StatusEnabled)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // unknown id, or not an enabled account — change nothing
	}
	// Sessions and links die with the privilege, one transaction: no
	// window where the status says disabled but a live token does not.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE user_id = ?`, userID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// EnableUser lifts a suspension (admin enable-user), and ONLY one: an
// 'unconfirmed' sign-up activates through its confirmation link, never
// through this door. Sessions are not restored — there is nothing to
// restore; the user logs in again and the middleware admits them.
func (d *DB) EnableUser(ctx context.Context, userID string) (bool, error) {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE users SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		StatusEnabled, time.Now().Unix(), userID, StatusDisabled)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Register creates the user and, when invite != "", consumes the invite in
// the same transaction: an unknown or already-used code aborts the insert.
func (d *DB) Register(ctx context.Context, u *User, invite string) error {
	if u.ID == "" {
		u.ID = id.New()
	}
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if invite != "" {
		var usedBy *string
		if err := tx.QueryRowContext(ctx,
			`SELECT used_by FROM invites WHERE code = ?`, invite).Scan(&usedBy); err != nil {
			if errors.Is(err, ErrNoRows) {
				return ErrInviteInvalid
			}
			return err
		}
		if usedBy != nil {
			return ErrInviteInvalid
		}
	}
	address := u.Email
	if address == "" {
		address = u.UnconfirmedEmail
	}
	available, err := emailAvailable(ctx, tx, address, "", time.Now())
	if err != nil {
		return err
	}
	if !available {
		return ErrUserExists
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users(id, email, name, password_hash, status, unconfirmed_email, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, nilIfEmpty(u.Email), u.Name, u.PasswordHash, u.Status,
		nilIfEmpty(u.UnconfirmedEmail), u.CreatedAt.Unix(), u.UpdatedAt.Unix()); err != nil {
		if isUnique(err, "users.email") {
			return ErrUserExists
		}
		return err
	}

	if invite != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE invites SET used_by = ?, used_at = ? WHERE code = ?`,
			u.ID, u.CreatedAt.Unix(), invite); err != nil {
			return err
		}
	}

	// Every account opens with exactly one untitled conversation: the SPA
	// lands a fresh login on the newest row, so the first session is ready
	// for typing without a "New chat" click (§8). In this transaction the
	// account can never exist without it, and a dead invite (rollback)
	// never leaves the chat behind. The placeholder title lives the normal
	// life — auto-titling renames it after the first exchange, exactly as
	// for button-made conversations.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversations(id, user_id, title, model, system_prompt, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		id.New(), u.ID, DefaultTitle, "", u.DefaultSystemPrompt,
		u.CreatedAt.Unix(), u.CreatedAt.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// ListUsers returns every account, newest first — the admin CLI's view.
// It is the only way to see pending sign-ups (NULL email), which no
// email-keyed lookup can name.
func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT `+userCols+` FROM users ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var (
			u              User
			email, pending sql.NullString
			created, seen  int64
		)
		if err := rows.Scan(&u.ID, &email, &u.Name, &u.PasswordHash, &u.Status,
			&u.DefaultSystemPrompt, &pending, &created, &seen); err != nil {
			return nil, err
		}
		u.Email = email.String
		u.UnconfirmedEmail = pending.String
		u.CreatedAt = time.Unix(created, 0)
		u.UpdatedAt = time.Unix(seen, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser removes an account by id. FKs cascade (sessions, invites.used_by
// is left as a dangling historical reference exactly as deleting any user
// today did — there is no FK on it); scratchpad bytes age out through the
// existing unknown-user sweep (§8). Reports whether a row was deleted.
func (d *DB) DeleteUser(ctx context.Context, userID string) (bool, error) {
	res, err := d.sqlDB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetUnconfirmedEmail points (or re-points) an account at a new address
// awaiting a confirmation click; "" clears the pending value (the cancel
// path). It does not mint the token — the caller pairs this with
// CreateToken(confirm_email) bound to the same address.
func (d *DB) SetUnconfirmedEmail(ctx context.Context, userID, email string, now time.Time) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET unconfirmed_email = ?, updated_at = ? WHERE id = ?`, nilIfEmpty(email), now.Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	if email == "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL`, TokenConfirmEmail, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// A reservation on an established account exists only while its matching
// confirmation link is live. Signup reservations use the fixed signup window,
// including the interval between account creation and first token issuance.
const liveConfirmationQuery = `SELECT 1 FROM auth_tokens t
 WHERE t.user_id = u.id AND t.kind = 'confirm_email' AND t.email = u.unconfirmed_email
 AND t.used_at IS NULL AND t.expires_at > ?`

const emailAvailabilityQuery = `SELECT NOT EXISTS (
 SELECT 1 FROM users u WHERE u.id <> ? AND
 (u.email = ? OR (u.unconfirmed_email = ? AND
 ((u.email IS NULL AND u.status = 'unconfirmed' AND u.created_at > ?)
 OR (u.email IS NOT NULL AND EXISTS (` + liveConfirmationQuery + `))))))`

type accountQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func emailAvailable(ctx context.Context, q accountQueryer, email, exceptUser string, now time.Time) (bool, error) {
	var available bool
	err := q.QueryRowContext(ctx, emailAvailabilityQuery, exceptUser, email, email,
		now.Add(-ConfirmationTTL).Unix(), now.Unix()).Scan(&available)
	return available, err
}

// EmailAvailable disregards expired reservations before the hourly sweep.
// The same rule runs in the transactions that register or change an address.
func (d *DB) EmailAvailable(ctx context.Context, email string, now time.Time) (bool, error) {
	return emailAvailable(ctx, d.sqlDB, email, "", now)
}
