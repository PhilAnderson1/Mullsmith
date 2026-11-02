package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"mullsmith/internal/auth"
)

func emailSecurityFixture(t *testing.T) (*DB, *User, []byte) {
	t.Helper()
	db := openTestDB(t)
	u := mkUser("owner@email.test")
	ctx := context.Background()
	now := time.Now()
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	session := auth.HashToken("email-security-session")
	if err := db.CreateSession(ctx, &Session{IDHash: session, UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return db, u, session
}

func TestPasswordTransitionsCancelPendingEmail(t *testing.T) {
	for _, mode := range []string{"reset", "change", "admin"} {
		t.Run(mode, func(t *testing.T) {
			db, u, session := emailSecurityFixture(t)
			ctx := context.Background()
			now := time.Now()
			if err := db.SetUnconfirmedEmail(ctx, u.ID, "attacker@email.test", now); err != nil {
				t.Fatal(err)
			}
			confirm, err := db.CreateToken(ctx, TokenConfirmEmail, u.ID, "attacker@email.test", now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			reset, err := db.CreateToken(ctx, TokenResetPassword, u.ID, u.Email, now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "reset":
				if _, found, err := db.CompletePasswordReset(ctx, reset, "new-hash", now); err != nil || !found {
					t.Fatalf("reset: %v %v", found, err)
				}
			case "change":
				if _, err := db.ChangePasswordAndRotate(ctx, u.ID, "new-hash", u.PasswordHash, session, false, now.Add(time.Hour), now, ""); err != nil {
					t.Fatal(err)
				}
			case "admin":
				if err := db.SetPassword(ctx, u.ID, "new-hash", now); err != nil {
					t.Fatal(err)
				}
			}
			fresh, err := db.UserByID(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.UnconfirmedEmail != "" || fresh.Email != u.Email {
				t.Fatalf("recovery state=%+v", fresh)
			}
			if found, err := db.ConfirmEmail(ctx, confirm, now); err != nil || found {
				t.Fatalf("old confirmation: %v %v", found, err)
			}
			if valid, err := db.PasswordResetValid(ctx, reset, now); err != nil || valid {
				t.Fatalf("old reset: %v %v", valid, err)
			}
		})
	}
}

func TestAdministrativePasswordChangePreservesSignupConfirmation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	u := mkUser("")
	u.Status = StatusUnconfirmed
	u.UnconfirmedEmail = "signup@email.test"
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateToken(ctx, TokenConfirmEmail, u.ID, u.UnconfirmedEmail, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetPassword(ctx, u.ID, "new-hash", now); err != nil {
		t.Fatal(err)
	}
	if found, err := db.ConfirmEmail(ctx, token, now); err != nil || !found {
		t.Fatalf("signup confirmation: %v %v", found, err)
	}
}

func TestEmailChangeRejectsStaleAuthorization(t *testing.T) {
	for _, change := range []string{"password", "reset", "session revoked", "session expired", "disabled", "address", "deleted"} {
		t.Run(change, func(t *testing.T) {
			db, u, session := emailSecurityFixture(t)
			ctx := context.Background()
			now := time.Now()
			var err error
			switch change {
			case "password":
				err = db.SetPassword(ctx, u.ID, "new-hash", now)
			case "reset":
				var token string
				token, err = db.CreateToken(ctx, TokenResetPassword, u.ID, u.Email, now.Add(time.Hour))
				if err == nil {
					_, _, err = db.CompletePasswordReset(ctx, token, "new-hash", now)
				}
			case "session revoked":
				err = db.DeleteSession(ctx, session)
			case "session expired":
				_, err = db.sqlDB.Exec(`UPDATE sessions SET expires_at = ?`, now.Add(-time.Minute).Unix())
			case "disabled":
				_, err = db.DisableUser(ctx, u.ID)
			case "address":
				_, err = db.sqlDB.Exec(`UPDATE users SET email = ? WHERE id = ?`, "moved@email.test", u.ID)
			case "deleted":
				_, err = db.DeleteUser(ctx, u.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			token, err := db.ChangeEmail(ctx, u, session, "Stale Name", "attacker@email.test", "stale prompt", now.Add(time.Hour), now)
			if !errors.Is(err, ErrStaleCredentials) || token != "" {
				t.Fatalf("change admitted: token=%q error=%v", token, err)
			}
			var count int
			if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE kind = ?`, TokenConfirmEmail).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("stale change created %d confirmation tokens", count)
			}
		})
	}
}

func TestEmailChangeAndConfirmationTokenAreAtomic(t *testing.T) {
	db, u, session := emailSecurityFixture(t)
	ctx := context.Background()
	now := time.Now()
	if err := db.SetUnconfirmedEmail(ctx, u.ID, "previous@email.test", now); err != nil {
		t.Fatal(err)
	}
	previous, err := db.CreateToken(ctx, TokenConfirmEmail, u.ID, "previous@email.test", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sqlDB.Exec(`CREATE TRIGGER reject_confirmation BEFORE INSERT ON auth_tokens BEGIN SELECT RAISE(ABORT,'injected token failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ChangeEmail(ctx, u, session, "Changed Name", "next@email.test", "changed prompt", now.Add(time.Hour), now); err == nil {
		t.Fatal("injected failure ignored")
	}
	fresh, err := db.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Name != u.Name || fresh.Email != u.Email || fresh.UnconfirmedEmail != "previous@email.test" || fresh.DefaultSystemPrompt != "" {
		t.Fatalf("partial update: %+v", fresh)
	}
	if found, err := db.ConfirmEmail(ctx, previous, now); err != nil || !found {
		t.Fatalf("previous token lost: %v %v", found, err)
	}
	// A subsequent metadata save cannot restore the pre-confirmation email.
	if err := db.UpdateProfile(ctx, u.ID, "Metadata", "prompt", now); err != nil {
		t.Fatal(err)
	}
	fresh, err = db.UserByID(ctx, u.ID)
	if err != nil || fresh.Email != "previous@email.test" {
		t.Fatalf("metadata restored old address: %+v %v", fresh, err)
	}
}
