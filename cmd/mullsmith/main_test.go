package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
	"mullsmith/internal/scratch"
	"mullsmith/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "mullsmith.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func putUser(t *testing.T, db *store.DB, email, pending, status string) *store.User {
	t.Helper()
	u := &store.User{
		Email:            email,
		Name:             "T",
		PasswordHash:     "x",
		Status:           status,
		UnconfirmedEmail: pending,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := db.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// --- list-users (plan §8): pending rows must be visible and nameable -----

// The default config path is the whole point of a "default install": bare
// `mullsmith` (and bare `mullsmith admin …`) must find /etc/mullsmith/config.yaml
// without being told (spec §2.1).
func TestDefaultConfigPath(t *testing.T) {
	if defaultConfigPath != "/etc/mullsmith/config.yaml" {
		t.Errorf("defaultConfigPath = %q, want /etc/mullsmith/config.yaml", defaultConfigPath)
	}
}

func TestExpireSessionsOnTicks(t *testing.T) {
	db := testDB(t)
	u := putUser(t, db, "expiry@cli.io", "", store.StatusEnabled)
	now := time.Now().Truncate(time.Second)
	hash := auth.HashToken("expired-session")
	if err := db.CreateSession(context.Background(), &store.Session{
		IDHash: hash, UserID: u.ID, CreatedAt: now.Add(-time.Hour),
		LastSeenAt: now.Add(-time.Hour), ExpiresAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		expireSessionsOnTicks(context.Background(), db,
			slog.New(slog.NewTextHandler(io.Discard, nil)), ticks)
		close(done)
	}()
	ticks <- now
	close(ticks)
	<-done

	if got, err := db.SessionByHash(context.Background(), hash); err != nil || got != nil {
		t.Fatalf("expired session after tick = (%+v, %v), want nil, nil", got, err)
	}
}

func TestAdminCreateUserAndResetPassword(t *testing.T) {
	db := testDB(t)
	cfg := &config.Config{
		Scratchpad: config.Scratchpad{Root: t.TempDir()},
		Auth:       config.Auth{Argon2: config.Argon2{Time: 1, Memory: 1024, Threads: 1}},
	}
	ctx := context.Background()
	const oldPassword = "initial-secret-42"
	if code := adminCreateUser(ctx, cfg, db, []string{
		"-email", "CLI@Example.Test", "-name", "CLI User", "-password", oldPassword,
	}); code != 0 {
		t.Fatalf("create-user exit = %d", code)
	}
	u, err := db.UserByEmail(ctx, "cli@example.test")
	if err != nil || u == nil {
		t.Fatalf("created user = (%+v, %v)", u, err)
	}
	if !auth.VerifyPassword(oldPassword, u.PasswordHash) {
		t.Fatal("created user's password does not verify")
	}
	for _, dir := range []string{"uploads", "work", "artifacts"} {
		if st, err := os.Stat(filepath.Join(cfg.Scratchpad.Root, u.ID, dir)); err != nil || !st.IsDir() {
			t.Fatalf("scratchpad %s = (%v, %v), want directory", dir, st, err)
		}
	}

	sessionHash := auth.HashToken("cli-reset-session")
	if err := db.CreateSession(ctx, &store.Session{
		IDHash: sessionHash, UserID: u.ID, CreatedAt: time.Now(),
		LastSeenAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	const newPassword = "replacement-secret-84"
	if code := adminResetPassword(ctx, cfg, db,
		[]string{u.Email, "-password", newPassword}); code != 0 {
		t.Fatalf("reset-password exit = %d", code)
	}
	u, err = db.UserByID(ctx, u.ID)
	if err != nil || u == nil || !auth.VerifyPassword(newPassword, u.PasswordHash) {
		t.Fatalf("reset password did not verify: user=%+v err=%v", u, err)
	}
	if session, err := db.SessionByHash(ctx, sessionHash); err != nil || session != nil {
		t.Fatalf("session after password reset = (%+v, %v), want revoked", session, err)
	}
}

func TestAdminInvitePersistsRequestedCodes(t *testing.T) {
	db := testDB(t)
	var out, errOut bytes.Buffer
	if code := adminInviteTo(context.Background(), db, []string{"3"}, &out, &errOut); code != 0 {
		t.Fatalf("invite exit = %d, stderr = %s", code, errOut.String())
	}
	lines := strings.Fields(out.String())
	if len(lines) != 3 {
		t.Fatalf("invite output = %q, want three codes", out.String())
	}
	for _, code := range lines {
		if ok, err := db.InviteAvailable(context.Background(), code); err != nil || !ok {
			t.Errorf("printed invite %q available = %v, err = %v", code, ok, err)
		}
	}
	if code := adminInviteTo(context.Background(), db, []string{"101"}, io.Discard, &errOut); code != 2 {
		t.Errorf("invite count 101 exit = %d, want 2", code)
	}
}

func TestCheckConfigChecksEveryConfiguredModel(t *testing.T) {
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"one","context_length":8192},{"id":"two","context_length":16384}]}`)
	}))
	defer models.Close()

	root := t.TempDir()
	cfgPath := filepath.Join(root, "check.yaml")
	body := "server:\n  listen: \"127.0.0.1:0\"\n" +
		"database:\n  dir: " + strconv.Quote(filepath.Join(root, "db")) + "\n" +
		"scratchpad:\n  root: " + strconv.Quote(filepath.Join(root, "scratch")) + "\n" +
		"llm:\n  endpoint: " + strconv.Quote(models.URL+"/v1") + "\n" +
		"  models:\n    - id: one\n    - id: two\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if code := checkConfig(cfg, logger); code != 0 {
		t.Fatalf("check-config with all models served exit = %d", code)
	}
	cfg.LLM.Models = append(cfg.LLM.Models, config.LLMModel{ID: "missing"})
	if code := checkConfig(cfg, logger); code != 1 {
		t.Fatalf("check-config with a missing configured model exit = %d", code)
	}
}

func TestRunServesHealthAndShutsDownOnSIGTERM(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	body := "server:\n" +
		"  listen: " + strconv.Quote(addr) + "\n" +
		"  shutdown_grace: 2s\n" +
		"database:\n  dir: " + strconv.Quote(filepath.Join(root, "db")) + "\n" +
		"scratchpad:\n  root: " + strconv.Quote(filepath.Join(root, "scratch")) + "\n" +
		"llm:\n  endpoint: \"http://127.0.0.1:1/v1\"\n" +
		"  models:\n    - id: test-model\n      context_size: 8192\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunProcessHelper$", "--", "-config", cfgPath)
	cmd.Env = append(os.Environ(), "MULLSMITH_RUN_PROCESS_HELPER=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become healthy: %v\n%s", err, output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("server shutdown: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server did not stop after SIGTERM\n%s", output.String())
	}
	if _, err := os.Stat(filepath.Join(root, "db", "mullsmith.db")); err != nil {
		t.Fatalf("startup did not create database: %v", err)
	}
}

// TestRunProcessHelper runs the production assembly path in a subprocess;
// the parent test above owns its signal and observes the real HTTP listener.
func TestRunProcessHelper(t *testing.T) {
	if os.Getenv("MULLSMITH_RUN_PROCESS_HELPER") != "1" {
		return
	}
	sep := -1
	for i, arg := range os.Args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatal("helper arguments missing separator")
	}
	os.Exit(run(os.Args[sep+1:]))
}

func TestAdminListUsersShowsPendingRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	active := putUser(t, db, "shown@cli.io", "", "enabled")
	pending := putUser(t, db, "", "typo@cli.io", "unconfirmed")

	var buf bytes.Buffer
	if code := adminListUsers(ctx, db, &buf, nil); code != 0 {
		t.Fatalf("list-users exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{
		active.ID + "  shown@cli.io  enabled",
		pending.ID + "  <pending: typo@cli.io>  unconfirmed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list-users output missing %q:\n%s", want, out)
		}
	}
}

// --- delete-user: id names a pending row, email names a confirmed one ----

func TestAdminDeleteUserByIdAndEmail(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pending := putUser(t, db, "", "gone@cli.io", "unconfirmed")
	active := putUser(t, db, "bye@cli.io", "", "enabled")

	// Pending sign-up: ONLY its id names it (the address lives in
	// unconfirmed_email, which no delete lookup may match).
	var buf bytes.Buffer
	if code := adminDeleteUser(ctx, db, t.TempDir(), &buf, []string{pending.ID}); code != 0 {
		t.Fatalf("delete pending by id = %d, want 0; out=%s", code, buf.String())
	}
	if u, _ := db.UserByID(ctx, pending.ID); u != nil {
		t.Error("pending row survived delete")
	}

	// Confirmed account by (uppercase-insensitive on storage) email.
	buf.Reset()
	if code := adminDeleteUser(ctx, db, t.TempDir(), &buf, []string{"bye@cli.io"}); code != 0 {
		t.Fatalf("delete by email = %d, want 0; out=%s", code, buf.String())
	}
	if u, _ := db.UserByEmail(ctx, "bye@cli.io"); u != nil {
		t.Error("confirmed row survived delete")
	}
	if !strings.Contains(buf.String(), "deleted user "+active.ID) {
		t.Errorf("delete output = %q", buf.String())
	}
}

// The user's scratchpad tree goes with the row — not a day later with
// the unknown-user sweep.
func TestAdminDeleteUserRemovesScratchpad(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	u := putUser(t, db, "scratch@cli.io", "", "enabled")
	if err := scratch.EnsureUserDirs(root, u.ID); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := adminDeleteUser(ctx, db, root, &buf, []string{u.ID}); code != 0 {
		t.Fatalf("delete = %d; out=%s", code, buf.String())
	}
	if _, err := os.Stat(filepath.Join(root, u.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scratchpad tree survived delete (stat err = %v)", err)
	}
}

func TestAdminDeleteUserUnknownTarget(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	confirmed := putUser(t, db, "keep@cli.io", "", "enabled")

	// Unknown id, and the pending address (deletable ONLY by id — naming
	// the pending address must miss, same invariant as login).
	pending := putUser(t, db, "", "wait@cli.io", "unconfirmed")
	for _, target := range []string{"nosuchid", "wait@cli.io"} {
		var buf bytes.Buffer
		if code := adminDeleteUser(ctx, db, t.TempDir(), &buf, []string{target}); code == 0 {
			t.Errorf("delete %q = 0, want non-zero", target)
		}
	}
	// Nothing was deleted.
	for _, u := range []*store.User{confirmed, pending} {
		if got, _ := db.UserByID(ctx, u.ID); got == nil {
			t.Errorf("%s vanished after a refused delete", adminEmailLabel(*u))
		}
	}
	// No argument: usage error.
	if code := adminDeleteUser(ctx, db, t.TempDir(), &bytes.Buffer{}, nil); code != 2 {
		t.Errorf("delete-user with no target = %d, want 2", code)
	}
}

// --- disable-user / enable-user: suspension round-trip and refusals ------

func TestAdminDisableEnableUser(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	u := putUser(t, db, "sus@cli.io", "", "enabled")

	var buf bytes.Buffer
	if code := adminSetSuspended(ctx, db, &buf, []string{"sus@cli.io"}, true); code != 0 {
		t.Fatalf("disable by email = %d, want 0; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "disabled user "+u.ID) {
		t.Errorf("disable output = %q", buf.String())
	}
	if got, _ := db.UserByID(ctx, u.ID); got.Status != store.StatusDisabled {
		t.Errorf("status after disable = %q", got.Status)
	}

	// Idempotence is a refusal the operator can read, not silence.
	buf.Reset()
	if code := adminSetSuspended(ctx, db, &buf, []string{u.ID}, true); code != 1 {
		t.Errorf("re-disable = %d, want 1", code)
	}
	if got, _ := db.UserByID(ctx, u.ID); got.Status != store.StatusDisabled {
		t.Errorf("re-disable changed status to %q", got.Status)
	}

	buf.Reset()
	if code := adminSetSuspended(ctx, db, &buf, []string{u.ID}, false); code != 0 {
		t.Fatalf("enable = %d, want 0; out=%s", code, buf.String())
	}
	if got, _ := db.UserByID(ctx, u.ID); got.Status != store.StatusEnabled {
		t.Errorf("status after enable = %q", got.Status)
	}
	if code := adminSetSuspended(ctx, db, &buf, []string{u.ID}, false); code != 1 {
		t.Errorf("enable of an enabled account = %d, want 1", code)
	}

	// Pending sign-ups are already unusable — suspending one is refused,
	// and enabling one must never happen (confirmation is the only door).
	pending := putUser(t, db, "", "wait2@cli.io", "unconfirmed")
	if code := adminSetSuspended(ctx, db, &buf, []string{pending.ID}, true); code != 1 {
		t.Errorf("disable of pending = %d, want 1", code)
	}
	if got, _ := db.UserByID(ctx, pending.ID); got.Status != store.StatusUnconfirmed {
		t.Errorf("pending status changed to %q", got.Status)
	}
	if code := adminSetSuspended(ctx, db, &buf, []string{pending.ID}, false); code != 1 {
		t.Errorf("enable of pending = %d, want 1", code)
	}

	if code := adminSetSuspended(ctx, db, &buf, []string{"nobody@cli.io"}, true); code != 1 {
		t.Errorf("unknown target = %d, want 1", code)
	}
}

// Every shutdown stage must live inside ONE shared grace deadline (review
// follow-up to issue 5): sequential WithTimeout(grace) calls gave each
// stage the full grace again, so the total could stretch to a multiple of
// the setting.
func TestShutdownStagesShareOneDeadline(t *testing.T) {
	const grace = 200 * time.Millisecond
	var (
		deadlines []time.Time
		expired   []bool
	)
	stage := func(burn time.Duration) func(context.Context) {
		return func(ctx context.Context) {
			dl, _ := ctx.Deadline()
			deadlines = append(deadlines, dl)
			time.Sleep(burn)
			expired = append(expired, ctx.Err() != nil)
		}
	}
	start := time.Now()
	shutdownWithin(grace,
		stage(grace+100*time.Millisecond), // overruns the shared budget
		stage(time.Millisecond),
		stage(time.Millisecond),
	)
	if d := time.Since(start); d > 2*grace {
		t.Errorf("total shutdown %v exceeded the shared grace %v", d, grace)
	}
	for i, dl := range deadlines {
		if !dl.Equal(deadlines[0]) {
			t.Errorf("stage %d saw deadline %v, want the shared %v", i, dl, deadlines[0])
		}
	}
	if !expired[1] || !expired[2] {
		t.Error("a stage starting after the shared deadline must inherit the expiry, not a fresh grace period")
	}
}
