package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"mullsmith/internal/auth"
)

// Token kinds (auth_tokens.kind).
const (
	TokenConfirmEmail = "confirm_email"
	// ConfirmationTTL is the fixed signup window and address-change link lifetime.
	ConfirmationTTL    = 24 * time.Hour
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
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	raw, err := createToken(ctx, tx, kind, userID, email, expiresAt)
	if err != nil {
		return "", err
	}
	return raw, tx.Commit()
}

// createToken lets an address change and its confirmation token share a commit.
func createToken(ctx context.Context, tx *sql.Tx, kind, userID, email string, expiresAt time.Time) (string, error) {
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL`, kind, userID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO auth_tokens(kind, token_hash, user_id, email, created_at, expires_at) VALUES(?, ?, ?, ?, ?, ?)`,
		kind, hash, userID, email, time.Now().Unix(), expiresAt.Unix()); err != nil {
		return "", err
	}
	return raw, nil
}

// CreateSignupConfirmation issues or replaces a link without extending signup
// lifetime. An empty userID selects by address for the public resend endpoint.
// Missing, already confirmed and expired accounts all produce no token.
func (d *DB) CreateSignupConfirmation(ctx context.Context, userID, email string, now time.Time) (string, error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	var created int64
	err = tx.QueryRowContext(ctx,
		`SELECT id, created_at FROM users WHERE email IS NULL AND status = ?
		 AND unconfirmed_email = ? AND created_at > ? AND (? = '' OR id = ?)`,
		StatusUnconfirmed, email, now.Add(-ConfirmationTTL).Unix(), userID, userID).Scan(&id, &created)
	if errors.Is(err, ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	raw, err := createToken(ctx, tx, TokenConfirmEmail, id, email, time.Unix(created, 0).Add(ConfirmationTTL))
	if err != nil {
		return "", err
	}
	return raw, tx.Commit()
}

// ConfirmEmail applies a confirmation token — ONE rule for both flows (new
// sign-up and Settings address change are literally the same operation):
// while the token's bound address still equals users.unconfirmed_email,
// copy it into email, clear the pending value and — only for a pending
// sign-up (email IS NULL) — set status enabled; an enabled account
// changing its address keeps its status, and a suspended one stays
// suspended (spec §9.17). The UNIQUE index re-read at that instant is the final
// availability arbiter.
//
// found=false covers every dead-link flavour with one answer: absent,
// expired, used, or address no longer pending (changed again / cancelled).
// ErrEmailTakenNow means the address was claimed meanwhile — pending is
// cleared, nothing else about the account changes.
//
// A successful switch also revokes outstanding reset links bound to the
// abandoned mailbox (astra2 review): they proved control of an address the
// account no longer has.
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
		 WHERE t.kind = ? AND t.token_hash = ? AND t.used_at IS NULL AND t.expires_at > ?
		 AND (u.email IS NOT NULL OR (u.status = 'unconfirmed' AND u.created_at > ?))`,
		TokenConfirmEmail, auth.HashToken(raw), now.Unix(), now.Add(-ConfirmationTTL).Unix()).Scan(&userID, &tokenEmail, &pending)
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
	// Activation is for pending sign-ups ONLY (email IS NULL): an enabled
	// account changing its address keeps its status, and a suspended one
	// stays suspended through the click — no link outranks an admin
	// disable (spec §9.17). The CASE reads the pre-update row, which is
	// exactly the distinction wanted.
	_, err = tx.ExecContext(ctx,
		`UPDATE users SET email = unconfirmed_email, unconfirmed_email = NULL,
		         status = CASE WHEN email IS NULL THEN ? ELSE status END,
		         updated_at = ? WHERE id = ?`,
		StatusEnabled, now.Unix(), userID)
	if isUnique(err, "users.email") {
		// Clear the failed reservation in this transaction, so a newer pending
		// change cannot be accidentally cleared after this one commits.
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET unconfirmed_email = NULL, updated_at = ? WHERE id = ?`, now.Unix(), userID); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, ErrEmailTakenNow
	}
	if err != nil {
		return false, err
	}
	// The address has moved: reset links bound to the abandoned mailbox
	// proved control of a mailbox that is no longer the account's — they
	// die here rather than surviving as a recovery path around the move
	// (the direct-change door is ChangeEmail's matching cut).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL AND email <> ?`,
		TokenResetPassword, userID, tokenEmail); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// CompletePasswordReset consumes a reset link and applies the new hash in
// ONE transaction: consume, apply, revoke ALL sessions — half-applying is
// exactly what a stolen session surviving a "successful" reset would look
// like. The link is bound to the mailbox it was mailed to: it completes only
// while that address is still the account's confirmed one, so a link to a
// mailbox the owner has since abandoned cannot overwrite the fresh
// credential (astra2 review). found=false collapses every dead-link
// flavour — absent, expired, used, address moved, account deleted.
// The status is deliberately NOT touched (spec §9.17): forgot-password
// mails only enabled addresses, so a status write here could only ever do
// the wrong thing — resurrecting a suspended account on a link minted
// before the suspension.
func (d *DB) CompletePasswordReset(ctx context.Context, raw, hash string, now time.Time) (userID string, found bool, err error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var bound string
	err = tx.QueryRowContext(ctx,
		resetAccountQuery,
		TokenResetPassword, auth.HashToken(raw), now.Unix()).Scan(&userID, &bound)
	if errors.Is(err, ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// Burn in the same transaction (used_at re-checked in the UPDATE:
	// "one shot" must not rely on the single-writer pool, same pattern
	// ConfirmEmail uses).
	res, err := tx.ExecContext(ctx,
		`UPDATE auth_tokens SET used_at = ? WHERE kind = ? AND token_hash = ? AND used_at IS NULL`,
		now.Unix(), TokenResetPassword, auth.HashToken(raw))
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", false, nil // raced to used between SELECT and UPDATE
	}
	// The address guard is re-applied at write time rather than trusting
	// the read above.
	res, err = tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ? AND email = ?`,
		hash, now.Unix(), userID, bound)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", false, nil // address moved or account gone
	}
	if err := revokePasswordLinks(ctx, tx, userID); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return "", false, err
	}
	return userID, true, tx.Commit()
}

const resetAccountQuery = `SELECT t.user_id, t.email FROM auth_tokens t JOIN users u ON u.id = t.user_id
	WHERE t.kind = ? AND t.token_hash = ? AND t.used_at IS NULL AND t.expires_at > ?
	  AND u.email = t.email`

// PasswordResetValid is a non-consuming preflight to avoid hashing dead links.
// CompletePasswordReset must still recheck and consume the token atomically.
func (d *DB) PasswordResetValid(ctx context.Context, raw string, now time.Time) (bool, error) {
	var valid bool
	err := d.sqlDB.QueryRowContext(ctx, `SELECT EXISTS (`+resetAccountQuery+`)`,
		TokenResetPassword, auth.HashToken(raw), now.Unix()).Scan(&valid)
	return valid, err
}

// DeleteExpiredTokens purges expired one-shot tokens; returns rows removed.
func (d *DB) DeleteExpiredTokens(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.sqlDB.ExecContext(ctx, `DELETE FROM auth_tokens WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteExpiredPendingUsers deletes never-confirmed sign-ups — exactly
// status='unconfirmed' AND email IS NULL, the invariant's precise
// definition of "pending" (suspended accounts always hold a confirmed
// address and are never matched) — created at or before cutoff. Their
// auth_tokens cascade via FK; the abandoned scratchpad directory goes to
// the existing unknown-user sweep (§8). Returns rows removed.
func (d *DB) DeleteExpiredPendingUsers(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := d.sqlDB.ExecContext(ctx,
		`DELETE FROM users WHERE status = ? AND email IS NULL AND created_at <= ?`,
		StatusUnconfirmed, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteExpiredEmailChanges clears reservations after their links expire or are
// revoked. It is cleanup only: availability already ignores them immediately.
func (d *DB) DeleteExpiredEmailChanges(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE users AS u SET unconfirmed_email = NULL, updated_at = ?
		 WHERE u.email IS NOT NULL AND u.unconfirmed_email IS NOT NULL
		 AND NOT EXISTS (`+liveConfirmationQuery+`)`, now.Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
