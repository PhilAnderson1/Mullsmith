package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gabkeep/internal/id"
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

// SetPassword updates the hash and bumps updated_at.
func (d *DB) SetPassword(ctx context.Context, userID, hash string, now time.Time) error {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, now.Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdateProfile changes the account's display details in one statement.
// Taking the whole row (not pointers) keeps the settings page honest: what
// the form shows is what is stored. A taken email maps to ErrUserExists.
// It deliberately does NOT touch unconfirmed_email: clearing a pending
// address change is the caller's explicit decision (the features-off
// Settings email write does it via SetUnconfirmedEmail, §5).
func (d *DB) UpdateProfile(ctx context.Context, userID, name, email, defaultPrompt string, now time.Time) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`UPDATE users SET name = ?, email = ?, default_system_prompt = ?, updated_at = ? WHERE id = ?`,
		name, nilIfEmpty(email), defaultPrompt, now.Unix(), userID)
	if err != nil {
		if isUnique(err, "users.email") {
			return ErrUserExists
		}
		return err
	}
	return nil
}

// SetUserStatus activates or disables an account (admin action; disabled
// users are rejected at login and on every authenticated request).
func (d *DB) SetUserStatus(ctx context.Context, userID, status string) error {
	res, err := d.sqlDB.ExecContext(ctx, `UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
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
func (d *DB) EmailAvailable(ctx context.Context, email string) (bool, error) {
	var one int
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE email = ? OR unconfirmed_email = ? COLLATE NOCASE LIMIT 1`,
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
// address — exactly the pending invariant's shape (disabled + NULL
// email), which is all resend-confirm may match. Active accounts
// mid-Settings-change are deliberately NOT matched: that flow lives behind
// an authenticated session, where a second PATCH re-points the target.
func (d *DB) UserByPendingEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(d.sqlDB.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users
		 WHERE email IS NULL AND status = 'disabled' AND unconfirmed_email = ? COLLATE NOCASE`,
		email))
}
