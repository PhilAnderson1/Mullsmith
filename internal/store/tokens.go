package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"gabkeep/internal/auth"
)

// Token kinds (auth_tokens.kind).
const (
	TokenConfirmEmail  = "confirm_email"
	TokenResetPassword = "reset_password"
)

// ErrEmailTakenNow surfaces from ConfirmEmail when the pending address was
// claimed by another account between mail-out and click. The pending value
// is cleared as part of reporting it; nothing else about the account changes.
var ErrEmailTakenNow = errors.New("that address is already in use")

// CreateToken mints a one-shot token for a user and returns the raw URL
// value; only its sha256 is stored (same doctrine as session cookies). The
// confirm kind is BOUND to the address the mail goes to: a confirmation
// authenticates control of that mailbox, and ConfirmEmail only applies while
// the account's pending target still equals this value.
//
// Minting invalidates prior unconsumed tokens of the same kind for the same
// user — one live link per purpose per account; clicking an older mail then
// simply yields the generic invalid-link message.
func (d *DB) CreateToken(ctx context.Context, kind, userID, email string, expiresAt time.Time) (string, error) {
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		return "", err
	}
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL`,
		kind, userID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO auth_tokens(kind, token_hash, user_id, email, created_at, expires_at)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		kind, hash, userID, email, time.Now().Unix(), expiresAt.Unix()); err != nil {
		return "", err
	}
	return raw, tx.Commit()
}

// ConsumeToken validates and single-uses a token, returning the owning user
// and the address the mail went to. Unknown, expired and already-used all
// collapse to found=false — handlers show one generic dead-link message and
// never reveal which happened.
func (d *DB) ConsumeToken(ctx context.Context, kind, raw string, now time.Time) (userID, email string, found bool, err error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, email FROM auth_tokens
		 WHERE kind = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		kind, auth.HashToken(raw), now.Unix()).Scan(&userID, &email)
	if errors.Is(err, ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	// Re-check used_at in the UPDATE: the single-writer pool serialises
	// transactions today, but "one shot" must not rely on that staying true.
	res, err := tx.ExecContext(ctx,
		`UPDATE auth_tokens SET used_at = ? WHERE kind = ? AND token_hash = ? AND used_at IS NULL`,
		now.Unix(), kind, auth.HashToken(raw))
	if err != nil {
		return "", "", false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", "", false, nil // raced to used between SELECT and UPDATE
	}
	return userID, email, true, tx.Commit()
}

// DeleteUnusedTokens drops a user's unconsumed tokens of one kind (the
// cancel-pending-address path; expiry sweeps handle the rest).
func (d *DB) DeleteUnusedTokens(ctx context.Context, kind, userID string) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL`,
		kind, userID)
	return err
}

// ConfirmEmail applies a confirmation token — ONE rule for both flows (new
// sign-up and Settings address change are literally the same operation):
// while the token's bound address still equals users.unconfirmed_email,
// copy it into email, clear the pending value and set status='active'
// (activating a sign-up; a harmless no-op for an active account changing
// its address). The UNIQUE index re-read at that instant is the final
// availability arbiter.
//
// found=false covers every dead-link flavour with one answer: absent,
// expired, used, or address no longer pending (changed again / cancelled).
// ErrEmailTakenNow means the address was claimed meanwhile — pending is
// cleared, nothing else about the account changes.
func (d *DB) ConfirmEmail(ctx context.Context, raw string, now time.Time) (found bool, err error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var (
		userID, tokenEmail string
		pending            sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT t.user_id, t.email, u.unconfirmed_email
		 FROM auth_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.kind = ? AND t.token_hash = ? AND t.used_at IS NULL AND t.expires_at > ?`,
		TokenConfirmEmail, auth.HashToken(raw), now.Unix()).Scan(&userID, &tokenEmail, &pending)
	if errors.Is(err, ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// Address binding: the token confirms ONLY its own mailbox, so the
	// account must still be pending on exactly that address. Timing-safe
	// compare against the lowercased mail address (NOCASE would pass a
	// differently-cased stale target; lowercasing at write time already
	// made case the operator's none-business).
	if !pending.Valid ||
		subtle.ConstantTimeCompare([]byte(tokenEmail), []byte(pending.String)) != 1 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE auth_tokens SET used_at = ? WHERE kind = ? AND token_hash = ? AND used_at IS NULL`,
		now.Unix(), TokenConfirmEmail, auth.HashToken(raw)); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE users SET email = unconfirmed_email, unconfirmed_email = NULL,
		         status = 'active', updated_at = ? WHERE id = ?`,
		now.Unix(), userID)
	if isUnique(err, "users.email") {
		// Someone claimed the address between mail-out and click. Clear
		// the doomed pending value (so a resend cannot re-mint against it)
		// and report the collision — this rollback-then-write needs its own
		// statement, so commit the clear deliberately.
		if err := tx.Commit(); err != nil {
			return false, err
		}
		_, _ = d.sqlDB.ExecContext(ctx,
			`UPDATE users SET unconfirmed_email = NULL, updated_at = ? WHERE id = ?`,
			now.Unix(), userID)
		return false, ErrEmailTakenNow
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// CompletePasswordReset applies a reset atomically: new hash, status
// 'active' (the reset proves mailbox control — see plan §5 interplay) and
// revocation of ALL sessions. Half-applying these three is exactly what a
// stolen session surviving a "successful" reset would look like.
func (d *DB) CompletePasswordReset(ctx context.Context, userID, hash string, now time.Time) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, status = 'active', updated_at = ? WHERE id = ?`,
		hash, now.Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteExpiredTokens purges expired one-shot tokens; returns rows removed.
func (d *DB) DeleteExpiredTokens(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.sqlDB.ExecContext(ctx, `DELETE FROM auth_tokens WHERE expires_at < ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteExpiredPendingUsers deletes never-confirmed sign-ups — exactly
// status='disabled' AND email IS NULL, the invariant's precise definition
// of "pending" (admin-disabled accounts always hold a confirmed address and
// are never matched) — created before cutoff. Their auth_tokens cascade
// via FK; the abandoned scratchpad directory goes to the existing
// unknown-user sweep (§8). Returns rows removed.
func (d *DB) DeleteExpiredPendingUsers(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := d.sqlDB.ExecContext(ctx,
		`DELETE FROM users WHERE status = 'disabled' AND email IS NULL AND created_at < ?`,
		cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
