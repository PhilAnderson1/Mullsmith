// Command gabkeep is a self-hosted, single-binary chat assistant (spec.md).
//
// Usage:
//
//	gabkeep -config /path/to/config.yaml
//	gabkeep -config /path/to/config.yaml admin invite [n]
//	gabkeep -config /path/to/config.yaml admin create-user -email E -name N -password P
//	gabkeep -config /path/to/config.yaml admin reset-password <email>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

	"gabkeep/internal/auth"
	"gabkeep/internal/config"
	"gabkeep/internal/id"
	"gabkeep/internal/llm"
	"gabkeep/internal/mail"
	"gabkeep/internal/runner"
	"gabkeep/internal/scratch"
	"gabkeep/internal/server"
	"gabkeep/internal/store"
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

func run(args []string) int {
	fs := flag.NewFlagSet("gabkeep", flag.ContinueOnError)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "usage: gabkeep -config /path/to/config.yaml [command]")
		fmt.Fprintln(out, "\nflags:")
		fs.PrintDefaults()
		fmt.Fprintln(out, "\ncommands:")
		fmt.Fprintln(out, "  admin invite [n]                     mint n single-use invite codes (default 1)")
		fmt.Fprintln(out, "  admin create-user -email -name -password")
		fmt.Fprintln(out, "  admin reset-password <email>         set a new password; revokes all sessions")
	}
	cfgPath := fs.String("config", "", "path to YAML config file (required)")
	showVersion := fs.Bool("version", false, "print version information and exit")
	checkOnly := fs.Bool("check-config", false, "validate configuration, print a report, exit non-zero on failure")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Printf("gabkeep %s (commit %s, built %s)\n", version, commit, buildDate)
		return 0
	}

	rest := fs.Args()
	if len(rest) > 0 {
		if rest[0] == "admin" {
			return adminMain(*cfgPath, rest[1:])
		}
		fmt.Fprintf(os.Stderr, "gabkeep: unexpected argument %q\n", rest[0])
		return 2
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "gabkeep: -config is required")
		fs.Usage()
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gabkeep: %v\n", err)
		return 1
	}

	logFile, logger, err := openLogging(cfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gabkeep: %v\n", err)
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

	db, err := store.Open(filepath.Join(cfg.Database.Dir, "gabkeep.db"))
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
	codeRunner, codeWarns, err := runner.NewForConfig(cfg)
	if err != nil {
		logger.Error("code execution setup failed", "error", err)
		return 1
	}
	for _, w := range codeWarns {
		logger.Warn("code execution", "msg", w)
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

	srv := server.New(cfg, db, logger, version, codeRunner, mailer)
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
		sctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.D())
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			logger.Warn("shutdown incomplete within grace period", "error", err)
		}
		// Let in-flight generations save their partial state before the DB
		// closes (streams detached from browser requests outlive Shutdown).
		wctx, wcancel := context.WithTimeout(context.Background(),
			cfg.Server.ShutdownGrace.D())
		srv.WaitIdle(wctx)
		wcancel()
		logger.Info("bye")
		return 0
	}
}

// expireSessionsLoop prunes dead sessions hourly (spec §8 background jobs).
func expireSessionsLoop(ctx context.Context, db *store.DB, logger *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := db.DeleteExpired(context.Background(), time.Now()); err != nil {
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
		fmt.Fprintln(os.Stderr, "usage: gabkeep -config CFG admin <invite|create-user|reset-password> [...]")
		return 2
	}
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "gabkeep: -config is required")
		return 2
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gabkeep: %v\n", err)
		return 1
	}
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "gabkeep: %v\n", err)
		return 1
	}
	db, err := store.Open(filepath.Join(cfg.Database.Dir, "gabkeep.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gabkeep: %v\n", err)
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
	default:
		fmt.Fprintf(os.Stderr, "gabkeep: unknown admin command %q\n", args[0])
		return 2
	}
}

func adminInvite(ctx context.Context, cfg *config.Config, db *store.DB, args []string) int {
	fs := flag.NewFlagSet("admin invite", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	n := 1
	if fs.NArg() > 0 {
		var err error
		n, err = strconv.Atoi(fs.Arg(0))
		if err != nil || n < 1 || n > 100 {
			fmt.Fprintln(os.Stderr, "invite count must be 1..100")
			return 2
		}
	}
	for i := 0; i < n; i++ {
		code, err := auth.NewInviteCode()
		if err != nil {
			fmt.Fprintln(os.Stderr, "gabkeep:", err)
			return 1
		}
		if err := db.CreateInvite(ctx, code, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "gabkeep: create invite:", err)
			return 1
		}
		fmt.Println(code)
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
	if !auth.ValidEmail(*email) {
		fmt.Fprintln(os.Stderr, "gabkeep: invalid email address")
		return 1
	}
	if err := auth.CheckPasswordPolicy(*password); err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep:", err)
		return 1
	}
	hash, err := auth.HashPassword(*password, cfg.Auth.Argon2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep:", err)
		return 1
	}
	now := time.Now()
	u := &store.User{
		ID:           id.New(),
		Email:        strings.ToLower(*email),
		Name:         *name,
		PasswordHash: hash,
		Status:       "active",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := db.Register(ctx, u, ""); err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep: create user:", err)
		return 1
	}
	if err := scratch.EnsureUserDirs(cfg.Scratchpad.Root, u.ID); err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep: scratchpad:", err)
		return 1
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
	u, err := db.UserByEmail(ctx, strings.ToLower(rest[0]))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep:", err)
		return 1
	}
	if u == nil {
		fmt.Fprintln(os.Stderr, "gabkeep: no account with that email")
		return 1
	}

	if newPw == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprintln(os.Stderr, "gabkeep: no terminal for prompt; use -password")
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
			fmt.Fprintln(os.Stderr, "gabkeep: passwords do not match")
			return 1
		}
		newPw = string(b)
	}
	if err := auth.CheckPasswordPolicy(newPw); err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep:", err)
		return 1
	}
	hash, err := auth.HashPassword(newPw, cfg.Auth.Argon2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep:", err)
		return 1
	}
	if err := db.SetPassword(ctx, u.ID, hash, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep:", err)
		return 1
	}
	if n, err := db.DeleteSessionsForUser(ctx, u.ID, nil); err != nil {
		fmt.Fprintln(os.Stderr, "gabkeep: session revocation failed:", err)
		return 1
	} else {
		fmt.Printf("password reset for %s; %d session(s) revoked\n", u.Email, n)
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

	ids, err := llm.Probe(context.Background(), cfg)
	if err != nil {
		ok = false
		fmt.Printf("[FAIL] llm.endpoint           %v\n", err)
	} else {
		found := llm.HasModel(ids, cfg.LLM.Model)
		if !found {
			ok = false
		}
		status := "ok"
		if !found {
			status = "FAIL"
		}
		fmt.Printf("[%s] llm.endpoint           %d model(s) served: %v\n", status, len(ids), ids)
		if !found {
			fmt.Printf("[FAIL] llm.model              %q not offered by endpoint\n", cfg.LLM.Model)
		}
	}
	if cfg.Scratchpad.CodeExec.Enabled {
		if err := runner.ProbeBwrap(); err != nil {
			ok = false
			fmt.Printf("[FAIL] code_execution       %v\n", err)
		} else {
			fmt.Printf("[ok]   code_execution       bubblewrap jail verified\n")
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
