package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"mullsmith/internal/auth"
)

func TestLoginSessionRequiresCurrentCredentials(t *testing.T) {
	for _, change := range []string{"none", "password reset", "email", "disabled", "deleted"} {
		t.Run(change, func(t *testing.T) {
			db := openTestDB(t)
			ctx, now := context.Background(), time.Now()
			u := mkUser("login@example.test")
			if err := db.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "password reset":
				var raw string
				raw, err = db.CreateToken(ctx, TokenResetPassword, u.ID, u.Email, now.Add(time.Hour))
				if err == nil {
					_, _, err = db.CompletePasswordReset(ctx, raw, "newhash", now)
				}
			case "email":
				_, err = db.sqlDB.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, "new@example.test", u.ID)
			case "disabled":
				_, err = db.DisableUser(ctx, u.ID)
			case "deleted":
				_, err = db.DeleteUser(ctx, u.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			sess := &Session{IDHash: auth.HashToken("new-session"), UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
			err = db.CreateLoginSession(ctx, sess, u)
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrStaleCredentials) {
				t.Fatalf("got %v, want stale credentials", err)
			}
			stored, err := db.SessionByHash(ctx, sess.IDHash)
			if err != nil || (stored != nil) != (change == "none") {
				t.Fatalf("stored session=%+v error=%v", stored, err)
			}
		})
	}
}
