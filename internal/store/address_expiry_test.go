package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"mullsmith/internal/auth"
)

func TestPendingChangeReservationRequiresLiveMatchingLink(t *testing.T) {
	for _, kind := range []string{"live", "expired", "missing", "used", "wrong purpose", "wrong address"} {
		t.Run(kind, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			now := time.Now().Truncate(time.Second)
			u := mkUser("owner@expiry.test")
			u.UnconfirmedEmail = "target@expiry.test"
			if err := db.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			if kind != "missing" {
				purpose, address, expires := TokenConfirmEmail, u.UnconfirmedEmail, now.Add(time.Hour)
				if kind == "expired" {
					expires = now
				}
				if kind == "wrong purpose" {
					purpose = TokenResetPassword
				}
				if kind == "wrong address" {
					address = "different@expiry.test"
				}
				token, err := db.CreateToken(ctx, purpose, u.ID, address, expires)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "used" {
					if _, err := db.sqlDB.Exec(`UPDATE auth_tokens SET used_at = ? WHERE token_hash = ?`, now.Unix(), auth.HashToken(token)); err != nil {
						t.Fatal(err)
					}
				}
			}
			available, err := db.EmailAvailable(ctx, u.UnconfirmedEmail, now)
			if err != nil || available != (kind != "live") {
				t.Fatalf("availability=%v %v", available, err)
			}
			cleared, err := db.DeleteExpiredEmailChanges(ctx, now)
			want := int64(1)
			if kind == "live" {
				want = 0
			}
			if err != nil || cleared != want {
				t.Fatalf("cleared=%d want=%d err=%v", cleared, want, err)
			}
			if available, err := db.EmailAvailable(ctx, u.Email, now); err != nil || available {
				t.Fatal("confirmed address became available", err)
			}
		})
	}
}

func TestExpiredSignupCanBeReclaimedBeforeSweep(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	old := mkUser("")
	old.UnconfirmedEmail = "signup@expiry.test"
	old.Status = StatusUnconfirmed
	old.CreatedAt = now.Add(-ConfirmationTTL)
	if err := db.Register(ctx, old, ""); err != nil {
		t.Fatal(err)
	}
	// Even an incorrectly long-lived token cannot outlive the signup itself.
	oldToken, err := db.CreateToken(ctx, TokenConfirmEmail, old.ID, old.UnconfirmedEmail, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if available, err := db.EmailAvailable(ctx, old.UnconfirmedEmail, now.Add(-time.Second)); err != nil || available {
		t.Fatalf("early release=%v %v", available, err)
	}
	if available, err := db.EmailAvailable(ctx, old.UnconfirmedEmail, now); err != nil || !available {
		t.Fatalf("boundary availability=%v %v", available, err)
	}
	if found, err := db.ConfirmEmail(ctx, oldToken, now); err != nil || found {
		t.Fatalf("expired signup activated=%v %v", found, err)
	}
	if token, err := db.CreateSignupConfirmation(ctx, old.ID, old.UnconfirmedEmail, now); err != nil || token != "" {
		t.Fatalf("expired signup resent=%q %v", token, err)
	}
	fresh := mkUser("")
	fresh.Status = StatusUnconfirmed
	fresh.UnconfirmedEmail = old.UnconfirmedEmail
	if err := db.Register(ctx, fresh, ""); err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateSignupConfirmation(ctx, "", fresh.UnconfirmedEmail, now)
	if err != nil || token == "" {
		t.Fatalf("fresh signup resend=%q %v", token, err)
	}
	var owner string
	if err := db.sqlDB.QueryRow(`SELECT user_id FROM auth_tokens WHERE token_hash = ?`, auth.HashToken(token)).Scan(&owner); err != nil || owner != fresh.ID {
		t.Fatalf("resend selected %s %v", owner, err)
	}
	if found, err := db.ConfirmEmail(ctx, token, now); err != nil || !found {
		t.Fatalf("fresh confirmation=%v %v", found, err)
	}
	if got, err := db.UserByID(ctx, old.ID); err != nil || got == nil || got.Email != "" {
		t.Fatalf("old signup state=%+v %v", got, err)
	}
	if deleted, err := db.DeleteExpiredPendingUsers(ctx, now.Add(-ConfirmationTTL)); err != nil || deleted != 1 {
		t.Fatalf("expired row cleanup=%d %v", deleted, err)
	}
}

func TestSignupResendsKeepOriginalDeadline(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	u := mkUser("")
	u.Status = StatusUnconfirmed
	u.UnconfirmedEmail = "resend@expiry.test"
	u.CreatedAt = now.Add(-23 * time.Hour)
	if err := db.Register(ctx, u, ""); err != nil {
		t.Fatal(err)
	}
	first, err := db.CreateSignupConfirmation(ctx, u.ID, u.UnconfirmedEmail, now)
	if err != nil || first == "" {
		t.Fatalf("first=%q %v", first, err)
	}
	second, err := db.CreateSignupConfirmation(ctx, "", u.UnconfirmedEmail, now.Add(30*time.Minute))
	if err != nil || second == "" || second == first {
		t.Fatalf("resend=%q %v", second, err)
	}
	var expiry int64
	if err := db.sqlDB.QueryRow(`SELECT expires_at FROM auth_tokens WHERE token_hash = ?`, auth.HashToken(second)).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	deadline := u.CreatedAt.Add(ConfirmationTTL)
	if expiry != deadline.Unix() {
		t.Fatalf("resend expiry=%d want=%d", expiry, deadline.Unix())
	}
	if found, err := db.ConfirmEmail(ctx, first, now); err != nil || found {
		t.Fatalf("old link survived resend=%v %v", found, err)
	}
	if token, err := db.CreateSignupConfirmation(ctx, "", u.UnconfirmedEmail, deadline); err != nil || token != "" {
		t.Fatalf("expired resend=%q %v", token, err)
	}
	if found, err := db.ConfirmEmail(ctx, second, deadline); err != nil || found {
		t.Fatalf("boundary confirmation=%v %v", found, err)
	}
}

func TestExpiryCleanupIncludesExactBoundary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	u := mkUser("cleanup@expiry.test")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, &Session{IDHash: auth.HashToken("expired"), UserID: u.ID, CreatedAt: now.Add(-time.Hour), ExpiresAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateToken(ctx, TokenResetPassword, u.ID, u.Email, now); err != nil {
		t.Fatal(err)
	}
	if deleted, err := db.DeleteExpired(ctx, now); err != nil || deleted != 1 {
		t.Fatalf("sessions=%d %v", deleted, err)
	}
	if deleted, err := db.DeleteExpiredTokens(ctx, now); err != nil || deleted != 1 {
		t.Fatalf("tokens=%d %v", deleted, err)
	}
}

func TestReservationLookupUsesAddressIndexes(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	rows, err := db.sqlDB.Query("EXPLAIN QUERY PLAN "+emailAvailabilityQuery, "", "address@expiry.test", "address@expiry.test", now.Add(-ConfirmationTTL).Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	t.Log(plan)
	for _, index := range []string{"idx_users_pending_email", "idx_authtokens_live_address"} {
		if !strings.Contains(plan, index) {
			t.Fatalf("missing indexed lookup %s: %s", index, plan)
		}
	}
}
