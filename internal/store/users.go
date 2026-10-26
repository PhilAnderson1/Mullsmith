package store

import (
	"context"
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
	ID           string
	Email        string // stored lowercased (collate NOCASE as belt-and-braces)
	Name         string
	PasswordHash string
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (d *DB) CreateUser(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = id.New()
	}
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO users(id, email, name, password_hash, status, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Name, u.PasswordHash, u.Status,
		u.CreatedAt.Unix(), u.UpdatedAt.Unix())
	if isUnique(err, "users.email") {
		return ErrUserExists
	}
	return err
}

// UserByEmail returns nil, nil when no user matches.
func (d *DB) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(d.sqlDB.QueryRowContext(ctx,
		`SELECT id, email, name, password_hash, status, created_at, updated_at
		 FROM users WHERE email = ?`, email))
}

// UserByID returns nil, nil when no user matches.
func (d *DB) UserByID(ctx context.Context, userID string) (*User, error) {
	return scanUser(d.sqlDB.QueryRowContext(ctx,
		`SELECT id, email, name, password_hash, status, created_at, updated_at
		 FROM users WHERE id = ?`, userID))
}

func scanUser(row row) (*User, error) {
	var (
		u             User
		created, seen int64
	)
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Status, &created, &seen)
	switch {
	case errors.Is(err, ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
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
		`INSERT INTO users(id, email, name, password_hash, status, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Name, u.PasswordHash, u.Status,
		u.CreatedAt.Unix(), u.UpdatedAt.Unix()); err != nil {
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
	return tx.Commit()
}
