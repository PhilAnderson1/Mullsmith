// Command gabkeep is a self-hosted, single-binary chat assistant (spec.md).
//
// Usage:
//
//	gabkeep -config /path/to/config.yaml
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
	"syscall"

	"gabkeep/internal/config"
	"gabkeep/internal/llm"
	"gabkeep/internal/server"
)

// Set via -ldflags at build time.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("gabkeep", flag.ContinueOnError)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "usage: gabkeep -config /path/to/config.yaml")
		fmt.Fprintln(out, "\nflags:")
		fs.PrintDefaults()
		fmt.Fprintln(out, "\nadmin subcommands land in milestone M1 (spec §2.1).")
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
			fmt.Fprintln(os.Stderr, "gabkeep: admin commands land in milestone M1 (spec §2.1)")
			return 2
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

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		logger.Error("cannot bind listen address", "addr", cfg.Server.Listen, "error", err)
		return 1
	}

	srv := server.New(cfg, logger, version)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		logger.Info("shutting down", "grace", cfg.Server.ShutdownGrace.D().String())
		sctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.D())
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			logger.Warn("shutdown incomplete within grace period", "error", err)
		}
		logger.Info("bye")
		return 0
	}
}

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
