// Command mullsmith is a self-hosted, single-binary chat assistant.
//
// Usage:
//
//	mullsmith                                              # serve (default config)
//	mullsmith -config /path/to/config.yaml
//	mullsmith [-config /path/to/config.yaml] admin invite [n]
//	mullsmith [-config /path/to/config.yaml] admin create-user -email E -name N -password P
//	mullsmith [-config /path/to/config.yaml] admin reset-password <email> [-password P]
//	mullsmith [-config /path/to/config.yaml] admin list-users
//	mullsmith [-config /path/to/config.yaml] admin delete-user <id-or-email>
//	mullsmith [-config /path/to/config.yaml] admin disable-user <id-or-email>
//	mullsmith [-config /path/to/config.yaml] admin enable-user  <id-or-email>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"mullsmith/internal/auth"
	"mullsmith/internal/config"
	"mullsmith/internal/id"
	"mullsmith/internal/llm"
	"mullsmith/internal/mail"
	"mullsmith/internal/runner"
	"mullsmith/internal/scratch"
	"mullsmith/internal/server"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
)

// Set via -ldflags at build time.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	// Sandbox launcher path (§9.9): when re-execed as the child interpreter
	// wrapper, apply rlimits/uid drop and execve — never falls through.
	if runner.MaybeSandboxMain(os.Args) {
		return
	}
	os.Exit(run(os.Args[1:]))
}

// defaultConfigPath is where mullsmith looks when -config is omitted: every
// supported install (tarball, .deb, .rpm) puts the file there, so a default
// install never has to name it (spec §2.1).
const defaultConfigPath = "/etc/mullsmith/config.yaml"

func run(args []string) int {
	fs := flag.NewFlagSet("mullsmith", flag.ContinueOnError)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "usage: mullsmith [flags] [command]")
		fmt.Fprintln(out, "\nflags:")
		fs.PrintDefaults()
		fmt.Fprintln(out, "\ncommands:")
		fmt.Fprintln(out, "  admin invite [n]                     mint n single-use invite codes (default 1)")
		fmt.Fprintln(out, "  admin create-user -email -name -password")
		fmt.Fprintln(out, "  admin reset-password <email>         set a new password (or mail a reset")
		fmt.Fprintln(out, "                                       link when email.enabled); revokes all sessions")
		fmt.Fprintln(out, "  admin list-users                     every account incl. pending sign-ups")
		fmt.Fprintln(out, "  admin delete-user <id-or-email>      remove an account (id names pending ones)")
		fmt.Fprintln(out, "  admin disable-user <id-or-email>     suspend: login refused now, sessions and")
		fmt.Fprintln(out, "                                       outstanding reset/confirm links revoked")
		fmt.Fprintln(out, "  admin enable-user <id-or-email>      lift a suspension (login again)")
	}
	cfgPath := fs.String("config", defaultConfigPath, "path to YAML config file")
	showVersion := fs.Bool("version", false, "print version information and exit")
	checkOnly := fs.Bool("check-config", false, "validate configuration, print a report, exit non-zero on failure")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Printf("mullsmith %s (commit %s, built %s)\n", version, commit, buildDate)
		return 0
	}

	rest := fs.Args()
	if len(rest) > 0 {
		if rest[0] == "admin" {
			return adminMain(*cfgPath, rest[1:])
		}
		fmt.Fprintf(os.Stderr, "mullsmith: unexpected argument %q\n", rest[0])
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mullsmith: %v\n", err)
		return 1
	}

	logFile, logger, err := openLogging(cfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mullsmith: %v\n", err)
		return 1
	}
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	if *checkOnly {
		return checkConfig(cfg, logger)
	}

	for _, w := range cfg.Warnings() {
		logger.Warn("config warning", "msg", w)
	}

	if err := cfg.EnsureDirs(); err != nil {
		logger.Error("cannot create data directories", "error", err)
		return 1
	}

	db, err := store.Open(filepath.Join(cfg.Database.Dir, "mullsmith.db"))
	if err != nil {
		logger.Error("cannot open database", "error", err)
		return 1
	}
	defer db.Close()

	// A previous crash can leave messages stuck in "streaming"; mark them
	// aborted before serving (spec §6.2).
	if n, err := db.SweepStreaming(context.Background()); err != nil {
		logger.Warn("startup streaming sweep failed", "error", err)
	} else if n > 0 {
		logger.Info("marked interrupted generations as aborted", "count", n)
	}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		logger.Error("cannot bind listen address", "addr", cfg.Server.Listen, "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Code execution (§9.9): opt-in and always bubblewrap-jailed; a missing
	// or non-functional bwrap is a startup error, never a degradation.
	// (NewForConfig also runs the jail probe here.)
	codeRunner, err := runner.NewForConfig(cfg)
	if err != nil {
		logger.Error("code execution setup failed", "error", err)
		return 1
	}
	if codeRunner != nil && cfg.Tools.Agent.Approval["code_run"] == "auto" &&
		cfg.Auth.Registration == "open" {
		logger.Warn("code_run auto-approved while registration is open: prompt injection " +
			"could execute code unattended (§9.9) — use confirm approval or invite-only")
	}

	// Email features (§ email_features plan): the async outbox exists only
	// while the switch is on; handlers 404 with it off, so a nil mailer
	// is never exercised.
	var mailer *mail.Outbox
	if cfg.Email.Enabled {
		mailer = mail.NewForConfig(cfg.Email, logger)
		logger.Info("email features enabled", "smtp", cfg.Email.SMTP.Addr(),
			"authenticated", cfg.Email.SMTP.AuthenticatedSMTP)
	}

	srv, err := server.New(cfg, db, logger, version, codeRunner, mailer)
	if err != nil {
		logger.Error("server construction failed", "error", err)
		return 1
	}
	srv.StartContextWindowProbe() // no-op unless some entry leaves context_size to auto
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", cfg.Server.Listen,
			"tls", cfg.Server.TLS.Enabled,
			"version", version,
			"registration", cfg.Auth.Registration,
		)
		var err error
		if cfg.Server.TLS.Enabled {
			err = srv.ServeTLS(ln, cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			err = srv.Serve(ln)
		}
		serveErr <- err
	}()

	go expireSessionsLoop(ctx, db, logger)
	go srv.RunMaintenance(ctx) // scratchpad lifecycle sweeps (spec §8)

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		logger.Info("shutting down", "grace", cfg.Server.ShutdownGrace.D().String())
		if n := srv.AbortGenerations(); n > 0 {
			logger.Info("cancelling in-flight generations", "count", n)
		}
		// ONE grace deadline is shared by every stage (review follow-up to
		// issue 5): three sequential WithTimeout(grace) calls gave each
		// stage the FULL grace again, so the worst-case wait was a multiple
		// of the setting. shutdownWithin derives each stage's context from
		// the SAME deadline: what a stage leaves unspent is all its
		// successor gets.
		shutdownWithin(cfg.Server.ShutdownGrace.D(),
			func(sctx context.Context) {
				if err := srv.Shutdown(sctx); err != nil {
					logger.Warn("shutdown incomplete within grace period", "error", err)
				}
			},
			// Let in-flight generations save their partial state before
			// the DB closes (streams detached from browser requests
			// outlive Shutdown).
			func(wctx context.Context) { srv.WaitIdle(wctx) },
			// The mail worker goes last: its enqueuers died with the HTTP
			// server. Close is BOUNDED (review3 issue 5) — the worker
			// context cancels an active delivery and the join respects the
			// shared budget. Anything still pending is abandoned and
			// logged, per the best-effort policy.
			func(mctx context.Context) {
				if err := mailer.Close(mctx); err != nil {
					logger.Warn("mail worker shutdown incomplete", "error", err)
				}
			},
		)
		logger.Info("bye")
		return 0
	}
}

// shutdownWithin runs the shutdown stages against ONE deadline shared by
// all of them: every stage receives the budget that REMAINS when its turn
// comes, so one hung stage cannot multiply the total by handing each
// successor a fresh grace period.
func shutdownWithin(grace time.Duration, stages ...func(context.Context)) {
	deadline := time.Now().Add(grace)
	for _, stage := range stages {
		sctx, cancel := context.WithDeadline(context.Background(), deadline)
		stage(sctx)
		cancel()
	}
}

// expireSessionsLoop prunes dead sessions hourly (spec §8 background jobs).
func expireSessionsLoop(ctx context.Context, db *store.DB, logger *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	expireSessionsOnTicks(ctx, db, logger, t.C)
}

// expireSessionsOnTicks is the clock-free loop contract. Production supplies
// the hourly ticker above; tests can drive an exact tick and cancellation
// without a timing sleep.
func expireSessionsOnTicks(ctx context.Context, db *store.DB, logger *slog.Logger, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			if n, err := db.DeleteExpired(context.Background(), now); err != nil {
				logger.Warn("session purge failed", "error", err)
			} else if n > 0 {
				logger.Info("purged expired sessions", "count", n)
			}
		}
	}
}

// --- admin subcommands (spec §2.1) -------------------------------------------------

func adminMain(cfgPath string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mullsmith [-config CFG] admin <invite|create-user|reset-password|list-users|delete-user|disable-user|enable-user> [...]")
		return 2
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mullsmith: %v\n", err)
		return 1
	}
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "mullsmith: %v\n", err)
		return 1
	}
	db, err := store.Open(filepath.Join(cfg.Database.Dir, "mullsmith.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mullsmith: %v\n", err)
		return 1
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch args[0] {
	case "invite":
		return adminInvite(ctx, cfg, db, args[1:])
	case "create-user":
		return adminCreateUser(ctx, cfg, db, args[1:])
	case "reset-password":
		return adminResetPassword(ctx, cfg, db, args[1:])
	case "list-users":
		return adminListUsers(ctx, db, os.Stdout, args[1:])
	case "delete-user":
		return adminDeleteUser(ctx, db, cfg.Scratchpad.Root, os.Stdout, args[1:])
	case "disable-user":
		return adminSetSuspended(ctx, db, os.Stdout, args[1:], true)
	case "enable-user":
		return adminSetSuspended(ctx, db, os.Stdout, args[1:], false)
	default:
		fmt.Fprintf(os.Stderr, "mullsmith: unknown admin command %q\n", args[0])
		return 2
	}
}

func adminInvite(ctx context.Context, cfg *config.Config, db *store.DB, args []string) int {
	return adminInviteTo(ctx, db, args, os.Stdout, os.Stderr)
}

func adminInviteTo(ctx context.Context, db *store.DB, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("admin invite", flag.ContinueOnError)
	fs.SetOutput(errOut)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	n := 1
	if fs.NArg() > 0 {
		var err error
		n, err = strconv.Atoi(fs.Arg(0))
		if err != nil || n < 1 || n > 100 {
			fmt.Fprintln(errOut, "invite count must be 1..100")
			return 2
		}
	}
	for i := 0; i < n; i++ {
		code, err := auth.NewInviteCode()
		if err != nil {
			fmt.Fprintln(errOut, "mullsmith:", err)
			return 1
		}
		if err := db.CreateInvite(ctx, code, time.Now()); err != nil {
			fmt.Fprintln(errOut, "mullsmith: create invite:", err)
			return 1
		}
		fmt.Fprintln(out, code)
	}
	return 0
}

func adminCreateUser(ctx context.Context, cfg *config.Config, db *store.DB, args []string) int {
	fs := flag.NewFlagSet("admin create-user", flag.ContinueOnError)
	email := fs.String("email", "", "account email address")
	name := fs.String("name", "", "display name")
	password := fs.String("password", "", "initial password")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *email == "" || *name == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "usage: admin create-user -email E -name N -password P")
		return 2
	}
	emailNorm, ok := auth.NormalizeEmail(*email)
	if !ok {
		fmt.Fprintln(os.Stderr, "mullsmith: invalid email address")
		return 1
	}
	if err := auth.CheckPasswordPolicy(*password); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	hash, err := auth.HashPassword(*password, cfg.Auth.Argon2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	now := time.Now()
	u := &store.User{
		ID:           id.New(),
		Email:        emailNorm, // canonical form, like every other write (L-3)
		Name:         *name,
		PasswordHash: hash,
		Status:       store.StatusEnabled,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := db.Register(ctx, u, ""); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: create user:", err)
		return 1
	}
	if err := scratch.EnsureUserDirs(cfg.Scratchpad.Root, u.ID); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: scratchpad:", err)
		return 1
	}
	// Run as root, the tree above belongs to root and the unprivileged
	// daemon could not write it; hand it to the owner the daemon itself
	// uses (derived from the scratchpad path). Warn-only: the account is
	// created either way, and a non-root run never trips this.
	if err := scratch.AdoptOwnership(cfg.Scratchpad.Root, u.ID); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: warning:", err)
	}
	fmt.Printf("created user %s (%s)\n", u.ID, u.Email)
	return 0
}

func adminResetPassword(ctx context.Context, cfg *config.Config, db *store.DB, args []string) int {
	// Go's flag package stops parsing at the first positional argument, but
	// "reset-password <email> -password X" is the natural way to type this;
	// pre-scan so -password works in any position.
	newPw := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-password" || a == "--password":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: admin reset-password <email> [-password P]")
				return 2
			}
			newPw, i = args[i+1], i+1
		case strings.HasPrefix(a, "-password="), strings.HasPrefix(a, "--password="):
			newPw = args[i][strings.LastIndexByte(args[i], '=')+1:]
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: admin reset-password <email> [-password P]")
		return 2
	}
	lookup, ok := auth.NormalizeEmail(rest[0])
	if !ok {
		fmt.Fprintln(os.Stderr, "mullsmith: invalid email address")
		return 1
	}
	u, err := db.UserByEmail(ctx, lookup)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	if u == nil {
		fmt.Fprintln(os.Stderr, "mullsmith: no account with that email")
		return 1
	}

	// email_features enhancement (plan §2): with the switch on and no
	// explicit -password override, mail the user a real reset link — the
	// same flow a user triggers themselves. Any mail problem degrades to
	// the classic prompt: mail is an upgrade, never a new recovery
	// dependency for the operator.
	if newPw == "" && cfg.Email.Enabled && adminMailReset(ctx, cfg, db, u) {
		return 0
	}

	if newPw == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprintln(os.Stderr, "mullsmith: no terminal for prompt; use -password")
			return 2
		}
		fmt.Print("New password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return 1
		}
		fmt.Print("Confirm password: ")
		b2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return 1
		}
		if string(b) != string(b2) {
			fmt.Fprintln(os.Stderr, "mullsmith: passwords do not match")
			return 1
		}
		newPw = string(b)
	}
	if err := auth.CheckPasswordPolicy(newPw); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	hash, err := auth.HashPassword(newPw, cfg.Auth.Argon2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	if err := db.SetPassword(ctx, u.ID, hash, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	if n, err := db.DeleteSessionsForUser(ctx, u.ID, nil); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: session revocation failed:", err)
		return 1
	} else {
		fmt.Printf("password reset for %s; %d session(s) revoked\n", u.Email, n)
	}
	return 0
}

// adminMailReset mails a real reset link (the user-facing flow: one-shot,
// expires in 30 min, completing it revokes every session). It DIALS
// SYNCHRONOUSLY — the CLI is short-lived and has no outbox worker to hand
// the job to. Returns false on any failure so the caller falls back to the
// interactive prompt; -password stays the offline escape hatch regardless.
func adminMailReset(ctx context.Context, cfg *config.Config, db *store.DB, u *store.User) bool {
	raw, err := db.CreateToken(ctx, store.TokenResetPassword, u.ID, u.Email, time.Now().Add(30*time.Minute))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: reset token:", err, "— falling back to password prompt")
		return false
	}
	subject, body := mail.ResetMail(cfg.Email.BaseURL, raw)
	if err := mail.NewSMTP(cfg.Email.SMTP).Deliver(ctx, u.Email, subject, body); err != nil {
		fmt.Fprintf(os.Stderr, "mullsmith: mail failed (%v) — falling back to password prompt\n", err)
		return false
	}
	fmt.Printf("reset link mailed to %s (one-shot, 30 min; completing it revokes all sessions)\n", u.Email)
	return true
}

// adminListUsers prints every row — the only way to see (and name) pending
// sign-ups, whose email column is NULL and who are therefore invisible to
// every address-keyed lookup (email_features plan §2).
func adminListUsers(ctx context.Context, db *store.DB, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("admin list-users", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	users, err := db.ListUsers(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: list users:", err)
		return 1
	}
	for _, u := range users {
		fmt.Fprintf(out, "%s  %s  %s  %s\n", u.ID, adminEmailLabel(u), u.Status,
			u.CreatedAt.UTC().Format(time.RFC3339))
	}
	return 0
}

// adminEmailLabel is how both list-users and delete-user speak of an
// account: its confirmed address, or the pending one in angle brackets.
func adminEmailLabel(u store.User) string {
	if u.Email != "" {
		return u.Email
	}
	return "<pending: " + u.UnconfirmedEmail + ">"
}

// resolveAdminUser names an account by id, falling back to a confirmed
// email — the addressing delete-user, disable-user and enable-user all
// accept. Id first: pending sign-ups can ONLY be named that way (their
// address lives in unconfirmed_email, which no lookup key matches — same
// invariant as login). A nil user is "no such account", never an error.
func resolveAdminUser(ctx context.Context, db *store.DB, target string) (*store.User, error) {
	u, err := db.UserByID(ctx, target)
	if err != nil {
		return nil, err
	}
	if u == nil && strings.Contains(target, "@") {
		// Canonical form, or no match is possible (L-3); an unparseable
		// target simply keeps the "not found" answer below.
		if lookup, ok := auth.NormalizeEmail(target); ok {
			return db.UserByEmail(ctx, lookup)
		}
	}
	return u, nil
}

// adminDeleteUser removes an account by id or confirmed email, scratchpad
// included. Id first: pending sign-ups can ONLY be named that way (their
// address lives in unconfirmed_email, which no lookup key matches — same
// invariant as login). auth_tokens cascade via FK; if the tree cannot be
// deleted here the unknown-user sweep (§8) collects it later. Status is
// never a reason to refuse — the operator has create-user standing (plan
// §11.3).
func adminDeleteUser(ctx context.Context, db *store.DB, scratchRoot string, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("admin delete-user", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: admin delete-user <id-or-email>")
		return 2
	}
	target := fs.Arg(0)
	u, err := resolveAdminUser(ctx, db, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	if u == nil {
		fmt.Fprintf(os.Stderr, "mullsmith: no account with that id or confirmed email: %s\n", target)
		return 1
	}
	deleted, err := db.DeleteUser(ctx, u.ID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: delete user:", err)
		return 1
	}
	if !deleted {
		fmt.Fprintln(os.Stderr, "mullsmith: user vanished mid-delete; nothing removed")
		return 1
	}
	// The row is gone, so the scratchpad is orphaned; removing it here
	// spares the operator a day's wait for the server sweep, which stays
	// the backstop when this fails (unreadable tree, wrong permissions).
	if err := scratch.RemoveUserDirs(scratchRoot, u.ID); err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith: warning: scratchpad not removed (the maintenance sweep will retry):", err)
		fmt.Fprintf(out, "deleted user %s (%s)\n", u.ID, adminEmailLabel(*u))
		return 0
	}
	fmt.Fprintf(out, "deleted user %s (%s); removed the scratchpad\n", u.ID, adminEmailLabel(*u))
	return 0
}

// adminSetSuspended implements disable-user and enable-user. Suspension
// is the whole story at once (spec §9.17): DisableUser drops sessions and
// outstanding one-shot links in the same transaction as the status, so
// nothing issued while the account worked can undo the operator's action.
// Every answer names the current status, so a no-op never needs guessing.
func adminSetSuspended(ctx context.Context, db *store.DB, out io.Writer, args []string, suspend bool) int {
	name := "enable-user"
	if suspend {
		name = "disable-user"
	}
	fs := flag.NewFlagSet("admin "+name, flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: admin %s <id-or-email>\n", name)
		return 2
	}
	target := fs.Arg(0)
	u, err := resolveAdminUser(ctx, db, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mullsmith:", err)
		return 1
	}
	if u == nil {
		fmt.Fprintf(os.Stderr, "mullsmith: no account with that id or confirmed email: %s\n", target)
		return 1
	}
	switch {
	case suspend && u.Status == store.StatusDisabled:
		fmt.Fprintf(os.Stderr, "mullsmith: %s is already disabled\n", u.ID)
		return 1
	case suspend && u.Status == store.StatusUnconfirmed:
		// Already unusable — suspending a pending sign-up would invent a
		// state no confirmation click can undo (the guard there preserves
		// the status). Delete it if it should not age out.
		fmt.Fprintf(os.Stderr, "mullsmith: %s is a pending sign-up — already unusable, swept after 24 h (delete-user removes it now)\n", u.ID)
		return 1
	case !suspend && u.Status != store.StatusDisabled:
		fmt.Fprintf(os.Stderr, "mullsmith: %s is not disabled (status %s) — nothing to enable\n", u.ID, u.Status)
		return 1
	}
	changed := false
	if suspend {
		changed, err = db.DisableUser(ctx, u.ID)
	} else {
		changed, err = db.EnableUser(ctx, u.ID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mullsmith: %s:", name)
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !changed {
		fmt.Fprintf(os.Stderr, "mullsmith: account vanished or changed mid-%s; nothing applied\n", name)
		return 1
	}
	if suspend {
		fmt.Fprintf(out, "disabled user %s (%s); sessions and outstanding links revoked\n", u.ID, adminEmailLabel(*u))
	} else {
		fmt.Fprintf(out, "enabled user %s (%s); login is open again\n", u.ID, adminEmailLabel(*u))
	}
	return 0
}

// --- check-config & logging (unchanged since M0) --------------------------------------

// checkConfig implements -check-config: validation (already done), IO checks
// and an LLM endpoint probe. No secrets appear in the report (spec §9.12).
func checkConfig(cfg *config.Config, logger *slog.Logger) int {
	ok := true
	report := func(c config.Check) {
		status := "ok"
		if !c.OK {
			status = "FAIL"
			ok = false
		}
		fmt.Printf("[%s] %-22s %s\n", status, c.Name, c.Detail)
	}
	fmt.Printf("config: %s\n", cfg.Path)
	for _, c := range cfg.CheckIO() {
		report(c)
	}
	for _, c := range tools.CheckMCPServers(context.Background(), cfg, logger) {
		report(c)
	}

	models, err := llm.ProbeModels(context.Background(), cfg)
	if err != nil {
		ok = false
		fmt.Printf("[FAIL] llm.endpoint           %v\n", err)
	} else {
		ids := make([]string, 0, len(models))
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		// Every picker model must be served, not just the default: a
		// conversation can select any of them.
		var missing []string
		for _, want := range cfg.LLM.ModelNames() {
			if !llm.HasModel(ids, want) {
				missing = append(missing, want)
			}
		}
		found := len(missing) == 0
		if !found {
			ok = false
		}
		status := "ok"
		if !found {
			status = "FAIL"
		}
		fmt.Printf("[%s] llm.endpoint           %d model(s) served: %v\n", status, len(ids), ids)
		for _, want := range missing {
			fmt.Printf("[FAIL] llm.models             %q not offered by endpoint\n", want)
		}
		// Context windows (§2.2): each entry's context_size is the
		// operator's word; auto entries read the endpoint's listing, and
		// we report honestly, per model, when the endpoint says nothing.
		for _, m := range cfg.LLM.Models {
			window, known := llm.ContextLengthFor(models, m.ID)
			switch {
			case m.ContextSize > 0 && known && window != m.ContextSize:
				fmt.Printf("[warn] llm.models             %q: config pins context_size %d, endpoint reports %d\n",
					m.ID, m.ContextSize, window)
			case m.ContextSize > 0:
				fmt.Printf("[ok]   llm.models             %q: context_size pinned at %d (endpoint: %s)\n",
					m.ID, m.ContextSize, windowOrNone(window, known))
			case known:
				fmt.Printf("[ok]   llm.models             %q: context_size auto, %d reported by the endpoint\n", m.ID, window)
			default:
				fmt.Printf("[warn] llm.models             %q: context_size auto and the endpoint reports no\n", m.ID)
				fmt.Printf("                           window — the fullness meter shows token counts only\n")
				fmt.Printf("                           and overlong conversations fail at the endpoint.\n")
			}
		}
		// Vision (§7.2) is per entry too: name the seers, or say plainly
		// that view_image will never appear.
		var seers []string
		for _, m := range cfg.LLM.Models {
			if m.Vision {
				seers = append(seers, m.ID)
			}
		}
		if len(seers) > 0 {
			fmt.Printf("[ok]   llm.models             view_image offered to: %v\n", seers)
		} else {
			fmt.Printf("[ok]   llm.models             no entry spells vision — view_image is never offered\n")
		}
	}
	if cfg.Tools.CodeRun.Enabled {
		if err := runner.ProbeBwrap(); err != nil {
			ok = false
			fmt.Printf("[FAIL] tools.code_run        %v\n", err)
		} else {
			fmt.Printf("[ok]   tools.code_run        bubblewrap jail verified\n")
		}
	}
	// email.smtp: warn-only by design — a transient mail-server blip must
	// not block a deploy, and a dial cannot prove SMTP credentials anyway.
	if cfg.Email.Enabled {
		pctx, pcancel := context.WithTimeout(context.Background(), 12*time.Second)
		err := cfg.ProbeSMTP(pctx)
		pcancel()
		if err != nil {
			fmt.Printf("[warn] email.smtp             %s unreachable: %v\n", cfg.Email.SMTP.Addr(), err)
		} else {
			fmt.Printf("[ok]   email.smtp             %s auth=%t\n",
				cfg.Email.SMTP.Addr(), cfg.Email.SMTP.AuthenticatedSMTP)
		}
	}
	for _, w := range cfg.Warnings() {
		fmt.Printf("[warn]                        %s\n", w)
	}
	if ok {
		fmt.Println("config check passed")
		return 0
	}
	logger.Warn("config check failed")
	return 1
}

// windowOrNone phrases an endpoint-reported window for the check report.
func windowOrNone(window int, known bool) string {
	if !known {
		return "no window reported"
	}
	return fmt.Sprintf("%d tokens", window)
}

func openLogging(l config.Logging) (*os.File, *slog.Logger, error) {
	lvl := slog.LevelInfo
	switch l.Level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	var w *os.File = os.Stderr
	if l.File != "" {
		f, err := os.OpenFile(l.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		w = f
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if l.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return w, slog.New(h), nil
}
