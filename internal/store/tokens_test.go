package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gabkeep/internal/auth"
)

// seedThrough0007 builds a database file exactly as it exists before this
// feature ships: migrations 0001..0007 applied through the same DSN pragmas
// the app uses (the FK-cascade landmine from the plan probe depends on
// _pragma=foreign_keys(1) being live), plus seeded users and sessions.
func seedThrough0007(t *testing.T, path string) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=data_sync(NORMAL)",
		filepath.Clean(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, e := range entries {
		var ver int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &ver); err != nil {
			t.Fatal(err)
		}
		if ver >= 8 {
			continue // 0008 must be applied by Open/migrate, not seeded
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			t.Fatalf("seed %s: %v", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version, applied_at) VALUES(?, ?)`, ver, now); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO users(id, email, name, password_hash, status, default_system_prompt, created_at, updated_at)
		VALUES ('u-ana', 'ana@example.com', 'Ana', 'h1', 'active', 'be brief', 100, 100),
		       ('u-bo',  'Bo@Example.com',  'Bo',  'h2', 'active', '',          200, 200);
		INSERT INTO sessions(id_hash, user_id, created_at, expires_at, remembered, last_seen_at)
		VALUES (x'aa', 'u-ana', 100, 99999999999, 0, 100),
		       (x'bb', 'u-ana', 100, 99999999999, 1, 100),
		       (x'cc', 'u-bo',  200, 99999999999, 0, 200);`); err != nil {
		t.Fatal(err)
	}
}

// TestMigration0008Rebuild is the mandatory rehearsal from the plan: apply
// 0008 through the real runner to a DB seeded through 0007 and prove the
// users-table rebuild destroys nothing and keeps every constraint honest.
// The naive variant (FK enforcement left on inside the tx) silently deletes
// every session — this test is the regression gate for that landmine.
func TestMigration0008Rebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgraded.db")
	seedThrough0007(t, path)

	db, err := Open(path) // applies 0008 via the "+no-foreign-keys" runner path
	if err != nil {
		t.Fatalf("Open through 0008: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	var n int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("sessions after rebuild = %d (err %v), want all 3 to survive", n, err)
	}

	// Users data intact, including the 0007 column; statuses untouched
	// (upgrade rule: enabling the feature never rewrites existing rows).
	ana, err := db.UserByEmail(ctx, "ana@example.com")
	if err != nil || ana == nil {
		t.Fatalf("ana missing after rebuild: %v", err)
	}
	if ana.Name != "Ana" || ana.DefaultSystemPrompt != "be brief" || ana.Status != "active" {
		t.Errorf("ana changed by rebuild: %+v", ana)
	}
	if ana.UnconfirmedEmail != "" {
		t.Errorf("existing user must have no pending email, got %q", ana.UnconfirmedEmail)
	}
	// Case-insensitive lookup still works on the rebuilt table.
	if bo, err := db.UserByEmail(ctx, "bo@example.com"); err != nil || bo == nil || bo.ID != "u-bo" {
		t.Fatalf("NOCASE lookup broken after rebuild: %v %v", bo, err)
	}

	// FK enforcement still LIVE after the marker dance: ghost session rejected.
	if _, err := db.sqlDB.Exec(
		`INSERT INTO sessions(id_hash, user_id, created_at, expires_at, remembered, last_seen_at)
		 VALUES (x'dd','ghost',1,99,0,1)`); err == nil {
		t.Error("FK enforcement lost: ghost-user session accepted")
	}
	if _, err := db.sqlDB.Exec(
		`INSERT INTO sessions(id_hash, user_id, created_at, expires_at, remembered, last_seen_at)
		 VALUES (x'ee','u-ana',1,99,0,1)`); err != nil {
		t.Errorf("legit session rejected: %v", err)
	}
	// auth_tokens FK rejects a ghost user.
	if _, err := db.sqlDB.Exec(
		`INSERT INTO auth_tokens VALUES ('confirm_email', x'ff', 'ghost', 'g@h.io', 1, 99, NULL)`); err == nil {
		t.Error("auth_tokens FK does not reject ghost user")
	}
	// Duplicate confirmed emails still rejected NOCASE.
	dup := mkUser("ANA@example.com")
	if err := db.CreateUser(ctx, dup); !errors.Is(err, ErrUserExists) {
		t.Errorf("dup email after rebuild: got %v, want ErrUserExists", err)
	}
	// Two pending (NULL email) sign-ups coexist — the reason '' could not
	// have been used as the placeholder.
	p1, p2 := mkUser(""), mkUser("")
	p1.UnconfirmedEmail, p1.Status = "p1@pending.io", "disabled"
	p2.UnconfirmedEmail, p2.Status = "p2@pending.io", "disabled"
	if err := db.CreateUser(ctx, p1); err != nil {
		t.Fatalf("first pending row: %v", err)
	}
	if err := db.CreateUser(ctx, p2); err != nil {
		t.Fatalf("second pending row: %v", err)
	}
	if got, _ := db.UserByEmail(ctx, ""); got != nil {
		t.Error("UserByEmail must never match a NULL-email row")
	}
}

func TestConfirmEmailOneRule(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	// --- sign-up activation: disabled + NULL email + pending -> confirmed ---
	su := mkUser("")
	su.Email, su.UnconfirmedEmail, su.Status = "", "new@sign.io", "disabled"
	if err := db.CreateUser(ctx, su); err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateToken(ctx, TokenConfirmEmail, su.ID, "new@sign.io", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found, err := db.ConfirmEmail(ctx, tok, now)
	if err != nil || !found {
		t.Fatalf("sign-up confirm: found=%v err=%v", found, err)
	}
	got, _ := db.UserByID(ctx, su.ID)
	if got.Email != "new@sign.io" || got.UnconfirmedEmail != "" || got.Status != "active" {
		t.Errorf("after confirm: %+v, want email copied, pending cleared, active", got)
	}
	// One-shot: the same token is dead now.
	if found, err := db.ConfirmEmail(ctx, tok, now); found || err != nil {
		t.Errorf("reused token: found=%v err=%v, want false nil", found, err)
	}

	// --- Settings change: active account moves its address ---
	st := mkUser("old@move.io")
	if err := db.CreateUser(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUnconfirmedEmail(ctx, st.ID, "new@move.io", now); err != nil {
		t.Fatal(err)
	}
	tok2, _ := db.CreateToken(ctx, TokenConfirmEmail, st.ID, "new@move.io", now.Add(time.Hour))
	if found, err := db.ConfirmEmail(ctx, tok2, now); !found || err != nil {
		t.Fatalf("settings confirm: found=%v err=%v", found, err)
	}
	got, _ = db.UserByID(ctx, st.ID)
	if got.Email != "new@move.io" || got.UnconfirmedEmail != "" || got.Status != "active" {
		t.Errorf("settings change result: %+v", got)
	}

	// --- stale flavours all collapse to the generic not-found ---
	// (a) address moved on before the old link was clicked
	ch := mkUser("cur@stale.io")
	if err := db.CreateUser(ctx, ch); err != nil {
		t.Fatal(err)
	}
	_ = db.SetUnconfirmedEmail(ctx, ch.ID, "first@stale.io", now)
	stale, _ := db.CreateToken(ctx, TokenConfirmEmail, ch.ID, "first@stale.io", now.Add(time.Hour))
	_ = db.SetUnconfirmedEmail(ctx, ch.ID, "second@stale.io", now) // re-point
	if found, err := db.ConfirmEmail(ctx, stale, now); found || err != nil {
		t.Errorf("token for superseded target: found=%v err=%v, want false nil", found, err)
	}
	// (b) cancelled pending ("" = NULL)
	_ = db.SetUnconfirmedEmail(ctx, ch.ID, "third@stale.io", now)
	tok3, _ := db.CreateToken(ctx, TokenConfirmEmail, ch.ID, "third@stale.io", now.Add(time.Hour))
	_ = db.SetUnconfirmedEmail(ctx, ch.ID, "", now)
	if found, err := db.ConfirmEmail(ctx, tok3, now); found || err != nil {
		t.Errorf("cancelled pending: found=%v err=%v, want false nil", found, err)
	}
	// (c) expired
	exp := mkUser("exp@soon.io")
	if err := db.CreateUser(ctx, exp); err != nil {
		t.Fatal(err)
	}
	_ = db.SetUnconfirmedEmail(ctx, exp.ID, "exp@soon.io", now)
	expTok, _ := db.CreateToken(ctx, TokenConfirmEmail, exp.ID, "exp@soon.io", now.Add(time.Minute))
	if found, err := db.ConfirmEmail(ctx, expTok, now.Add(time.Hour)); found || err != nil {
		t.Errorf("expired token: found=%v err=%v, want false nil", found, err)
	}

	// --- taken-in-the-meantime: pending cleared, ErrEmailTakenNow ---
	claim := "claimed@x.io"
	loser := mkUser("")
	loser.UnconfirmedEmail, loser.Status = claim, "disabled"
	if err := db.CreateUser(ctx, loser); err != nil {
		t.Fatal(err)
	}
	winner := mkUser(claim)
	if err := db.CreateUser(ctx, winner); err != nil {
		t.Fatal(err)
	}
	loseTok, _ := db.CreateToken(ctx, TokenConfirmEmail, loser.ID, claim, now.Add(time.Hour))
	found, err = db.ConfirmEmail(ctx, loseTok, now)
	if !errors.Is(err, ErrEmailTakenNow) || found {
		t.Fatalf("taken race: found=%v err=%v, want false ErrEmailTakenNow", found, err)
	}
	got, _ = db.UserByID(ctx, loser.ID)
	if got.Email != "" || got.UnconfirmedEmail != "" || got.Status != "disabled" {
		t.Errorf("loser must keep its shape with pending cleared: %+v", got)
	}
	if w, _ := db.UserByID(ctx, winner.ID); w.Email != claim {
		t.Errorf("winner untouched rule broken: %+v", w)
	}
}

func TestResetTokenLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	u := mkUser("reset@me.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	// A session that must not survive a completed reset.
	_, sh, _ := auth.NewSessionToken()
	if err := db.CreateSession(ctx, &Session{
		IDHash: sh, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := db.CreateToken(ctx, TokenResetPassword, u.ID, "reset@me.io", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Unknown token / wrong kind / expired / reused → found=false, never an error.
	if _, _, found, err := db.ConsumeToken(ctx, TokenResetPassword, "bogus", now); found || err != nil {
		t.Errorf("bogus token: found=%v err=%v", found, err)
	}
	if _, _, found, _ := db.ConsumeToken(ctx, TokenConfirmEmail, raw, now); found {
		t.Error("reset token must not pass as confirm token")
	}
	// Reissue invalidates the previous link (one live link per purpose).
	raw2, err := db.CreateToken(ctx, TokenResetPassword, u.ID, "reset@me.io", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := db.ConsumeToken(ctx, TokenResetPassword, raw, now); found {
		t.Error("reissue must invalidate the older token")
	}
	userID, email, found, err := db.ConsumeToken(ctx, TokenResetPassword, raw2, now)
	if err != nil || !found || userID != u.ID || email != "reset@me.io" {
		t.Fatalf("consume: %q %q found=%v err=%v", userID, email, found, err)
	}
	if _, _, found, _ := db.ConsumeToken(ctx, TokenResetPassword, raw2, now); found {
		t.Error("one-shot violated")
	}

	// CompletePasswordReset: hash + active + ALL sessions gone, atomically.
	dis := mkUser("dis@abl.io")
	dis.Status = "disabled"
	if err := db.CreateUser(ctx, dis); err != nil {
		t.Fatal(err)
	}
	if err := db.CompletePasswordReset(ctx, dis.ID, "newhash", now); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.UserByID(ctx, dis.ID); got.PasswordHash != "newhash" || got.Status != "active" {
		t.Errorf("reset not applied: %+v", got)
	}
	if n, _ := db.DeleteSessionsForUser(ctx, u.ID, nil); n != 1 {
		t.Errorf("session for reset user: expected 1 revoked, got %d", n)
	}
	if err := db.CompletePasswordReset(ctx, "01GHOSTGHOSTGHOSTGHOSTGHOS", "h", now); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("ghost reset: got %v, want ErrUserNotFound", err)
	}
}

func TestEmailAvailableAndSweeps(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	avail, err := db.EmailAvailable(ctx, "free@x.io")
	if err != nil || !avail {
		t.Fatalf("free address reported taken (avail=%v err=%v)", avail, err)
	}
	taken := mkUser("Taken@X.io") // handler stores lowercased; NOCASE guards anyway
	if err := db.CreateUser(ctx, taken); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.EmailAvailable(ctx, "taken@x.io"); ok {
		t.Error("confirmed email must not be available")
	}
	pend := mkUser("")
	pend.UnconfirmedEmail, pend.Status = "pend@X.io", "disabled"
	if err := db.CreateUser(ctx, pend); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.EmailAvailable(ctx, "pend@x.io"); ok {
		t.Error("pending email must not be available (both-column check)")
	}

	// --- token expiry sweep ---
	soon, _ := db.CreateToken(ctx, TokenResetPassword, taken.ID, "taken@x.io", now.Add(time.Minute))
	if _, err := db.CreateToken(ctx, TokenConfirmEmail, pend.ID, "pend@X.io", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = soon
	if n, err := db.DeleteExpiredTokens(ctx, now); err != nil || n != 0 {
		t.Errorf("premature expiry: n=%d err=%v", n, err)
	}
	if n, err := db.DeleteExpiredTokens(ctx, now.Add(2*time.Minute)); err != nil || n != 1 {
		t.Errorf("token sweep: n=%d err=%v, want 1", n, err)
	}

	// --- pending sign-up expiry: disabled+NULL-email past the window dies,
	// everything else survives ---
	old1 := mkUser("")
	old1.ID, old1.UnconfirmedEmail, old1.Status = "01OLDPENDING0000000000000a", "gone@x.io", "disabled"
	old1.CreatedAt, old1.UpdatedAt = now.Add(-25*time.Hour), now.Add(-25*time.Hour)
	if err := db.CreateUser(ctx, old1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateToken(ctx, TokenConfirmEmail, old1.ID, "gone@x.io", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	young := mkUser("")
	young.UnconfirmedEmail, young.Status = "young@x.io", "disabled"
	if err := db.CreateUser(ctx, young); err != nil {
		t.Fatal(err)
	}
	adminDisabled := mkUser("banned@x.io") // confirmed address ⇒ never auto-deleted
	adminDisabled.Status = "disabled"
	if err := db.CreateUser(ctx, adminDisabled); err != nil {
		t.Fatal(err)
	}

	if n, err := db.DeleteExpiredPendingUsers(ctx, now.Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("pending sweep: n=%d err=%v, want 1", n, err)
	}
	if got, _ := db.UserByID(ctx, old1.ID); got != nil {
		t.Error("expired pending user survived the sweep")
	}
	var tokens int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE user_id = ?`, old1.ID).Scan(&tokens); err != nil || tokens != 0 {
		t.Errorf("pending user's tokens must cascade; count=%d err=%v", tokens, err)
	}
	for _, keep := range []string{young.ID, adminDisabled.ID, taken.ID} {
		if got, _ := db.UserByID(ctx, keep); got == nil {
			t.Errorf("user %s must survive the sweep", keep)
		}
	}
}

// TestPendingUserVisibility pins the invariant rule: NULL-email rows are
// invisible to every credential-facing read.
func TestPendingUserVisibility(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	p := mkUser("")
	p.UnconfirmedEmail, p.Status = "only@pending.io", "disabled"
	if err := db.Register(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.UserByEmail(ctx, "only@pending.io"); got != nil {
		t.Error("pending address must not resolve via UserByEmail")
	}
	if ok, _ := db.EmailAvailable(ctx, "only@pending.io"); ok {
		t.Error("pending address must block re-registration")
	}
}
