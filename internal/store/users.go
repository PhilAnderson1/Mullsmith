package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

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

// SetPassword updates the hash, revokes every outstanding reset link and
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
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL`,
		TokenResetPassword, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateProfile changes the account's display details in one statement.
// Taking the whole row (not pointers) keeps the settings page honest: what
// the form shows is what is stored. A taken email maps to ErrUserExists.
// It deliberately does NOT touch unconfirmed_email: clearing a pending
// address change is the caller's explicit decision (the features-off
// Settings email write does it via SetUnconfirmedEmail, §5). When the
// confirmed address itself moves (the features-off door), reset links bound
// to the abandoned mailbox die with it — the same rule ConfirmEmail applies
// on the features-on door (astra2 review).
func (d *DB) UpdateProfile(ctx context.Context, userID, name, email, defaultPrompt string, now time.Time) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET name = ?, email = ?, default_system_prompt = ?, updated_at = ? WHERE id = ?`,
		name, nilIfEmpty(email), defaultPrompt, now.Unix(), userID); err != nil {
		if isUnique(err, "users.email") {
			return ErrUserExists
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL AND email IS NOT ?`,
		TokenResetPassword, userID, nilIfEmpty(email)); err != nil {
		return err
	}
	return tx.Commit()
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
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE users SET unconfirmed_email = ?, updated_at = ? WHERE id = ?`,
		nilIfEmpty(email), now.Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// EmailAvailable reports whether an address may be claimed at sign-up:
// it must match neither any confirmed email nor any pending unconfirmed
// one. Without the second column a typo'd pending sign-up's address could
// be registered by someone else, stranding both parties (plan §3).
// Comparisons are plain binary: every stored address and every caller
// passes auth.NormalizeEmail's canonical form, so NOCASE collation would
// only paper over callers that skip it (audit1 L-3).
func (d *DB) EmailAvailable(ctx context.Context, email string) (bool, error) {
	var one int
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE email = ? OR unconfirmed_email = ? LIMIT 1`,
		email, email).Scan(&one)
	switch {
	case errors.Is(err, ErrNoRows):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// UserByPendingEmail finds a never-confirmed sign-up by its pending
// address — exactly the pending invariant's shape (unconfirmed + NULL
// email), which is all resend-confirm may match. Enabled accounts
// mid-Settings-change are deliberately NOT matched: that flow lives behind
// an authenticated session, where a second PATCH re-points the target.
// Binary comparison per the normalization doctrine above (audit1 L-3).
func (d *DB) UserByPendingEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(d.sqlDB.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users
		 WHERE email IS NULL AND status = ? AND unconfirmed_email = ?`,
		StatusUnconfirmed, email))
}
