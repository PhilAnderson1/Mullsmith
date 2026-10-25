// Package runner executes model-authored scripts inside a user's scratchpad
// work/ directory with the defense layers of spec §9.9: an allowlisted
// interpreter, the script delivered on stdin (never argv), a scrubbed
// environment, resource limits applied in the child before exec,
// process-group kill on timeout, capped output, and strict concurrency
// (one run per user, bounded global parallelism).
//
// It is deliberately separate from internal/tools so the sandbox machinery
// can be audited and tested on its own; CodeRun is a thin adapter over it.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"gabkeep/internal/config"
)

// ErrBusy is returned when the user already has a run in flight or the
// global cap is saturated (§9.9 layer 5: "max one concurrent run per user
// (queue of 1, else busy)").
var ErrBusy = errors.New("another code run is already in flight")

// Request describes one execution. The caller resolves the interpreter from
// the config allowlist; a RuntimeID that is not allowlisted never reaches a
// Runner and Binary always comes from the server's own config, never the
// model (§9.9 layer 2).
type Request struct {
	RuntimeID   string
	Binary      string        // allowlisted absolute path (host modes)
	WorkDir     string        // absolute; cwd of the process (user's work/)
	Script      string        // fed to the interpreter on stdin
	Timeout     time.Duration // wall clock; process group killed past it
	MaxOutput   int           // bytes retained per stream (stdout/stderr)
	MaxMemory   uint64        // RLIMIT_AS bytes, enforced in isolated mode
	MaxFileSize uint64        // RLIMIT_FSIZE bytes, enforced in isolated mode
	MaxProcs    uint64        // RLIMIT_NPROC, only when the uid actually changes
}

// Outcome is the sanitized result: exit status plus capped output.
type Outcome struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool // either stream hit MaxOutput
	TimedOut  bool
}

// Runner executes one Request per call, honoring ctx cancellation (which
// must kill the process, not just stop waiting).
type Runner interface {
	Run(ctx context.Context, userID string, req Request) (Outcome, error)
	Mode() string
}

// --- concurrency gate ---------------------------------------------------------

type gate struct {
	global chan struct{}
	users  sync.Map // userID -> struct{}
}

func newGate(globalCap int) *gate {
	if globalCap < 1 {
		globalCap = 4
	}
	return &gate{global: make(chan struct{}, globalCap)}
}

// acquire reserves the per-user slot then a global slot; callers release on
// completion. A busy user is refused rather than queued (§9.9); the global
// slot waits (runs are wall-clock bounded, so saturation is self-healing)
// but honors cancellation.
func (g *gate) acquire(ctx context.Context, userID string) (func(), error) {
	if _, taken := g.users.LoadOrStore(userID, struct{}{}); taken {
		return nil, ErrBusy
	}
	select {
	case g.global <- struct{}{}:
	case <-ctx.Done():
		g.users.Delete(userID)
		return nil, ctx.Err()
	}
	return func() {
		<-g.global
		g.users.Delete(userID)
	}, nil
}

// --- construction from config (§9.9 startup validation) ------------------------

// NewForConfig builds the runner for cfg.Scratchpad.CodeExec and reports
// startup warnings (degraded isolation, raw mode). Errors are configuration
// errors: gabkeep must not serve code_run promises it cannot enforce.
func NewForConfig(cfg *config.Config) (Runner, []string, error) {
	ce := &cfg.Scratchpad.CodeExec
	if !ce.Enabled {
		return nil, nil, nil
	}
	timeout := ce.Timeout.D()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxOut := int(ce.MaxOutput)
	if maxOut <= 0 {
		maxOut = 32 << 10
	}
	maxMem := uint64(ce.MaxMemory) // #nosec G115 -- config.Size parses only non-negative values
	if maxMem == 0 {
		maxMem = 512 << 20
	}
	opts := Options{
		Timeout:     timeout,
		MaxOutput:   maxOut,
		MaxMemory:   maxMem,
		MaxFileSize: uint64(cfg.Scratchpad.MaxFileSize), // #nosec G115 -- config.Size parses only non-negative values
	}

	var warn []string
	switch ce.Mode {
	case "raw":
		warn = append(warn,
			"code execution mode \"raw\": scripts run as the gabkeep OS user with only timeout/output/concurrency limits — trusted single-user dev boxes only (§9.9)")
		r, err := NewHost(opts)
		return r, warn, err

	case "isolated":
		u, err := user.Lookup(ce.RunAsUser)
		if err != nil {
			return nil, nil, fmt.Errorf("code_execution.run_as_user %q not found: %w", ce.RunAsUser, err)
		}
		uid, err1 := strconv.Atoi(u.Uid)
		gid, err2 := strconv.Atoi(u.Gid)
		if err1 != nil || err2 != nil {
			return nil, nil, fmt.Errorf("code_execution.run_as_user %q: unparseable uid/gid", ce.RunAsUser)
		}
		opts.Enforce = true
		root := syscall.Geteuid() == 0
		switch {
		case root:
			opts.DropUID, opts.DropGID = uid, gid
			opts.SetNproc = true // the uid switch makes RLIMIT_NPROC meaningful
			r, err := NewHost(opts)
			return r, warn, err
		case uid == syscall.Geteuid():
			// Non-root cannot setuid — not even to itself (setgroups
			// needs CAP_SETGID). Degrade honestly: rlimits still land,
			// the uid switch does not.
			warn = append(warn,
				"code execution isolated mode DEGRADED: gabkeep is not root, run_as_user switch impossible — enforcing rlimits only (§9.9)")
			opts.DropUID, opts.DropGID = -1, -1
			r, err := NewHost(opts)
			return r, warn, err
		default:
			return nil, nil, fmt.Errorf("isolated mode needs root to run as %q (current uid %d); run gabkeep as root, use container mode, or set mode: raw on a trusted dev box",
				ce.RunAsUser, syscall.Geteuid())
		}

	case "container":
		if err := ProbeDocker(); err != nil {
			return nil, nil, fmt.Errorf("code_execution mode container requested but docker is unavailable: %w", err)
		}
		r, err := NewDocker(opts, ce.ContainerImage, !ce.AllowNetwork)
		return r, warn, err

	default:
		return nil, nil, fmt.Errorf("unknown code_execution mode %q", ce.Mode)
	}
}

// Options carries the shared enforcement knobs across modes.
type Options struct {
	Timeout     time.Duration
	MaxOutput   int
	MaxMemory   uint64
	MaxFileSize uint64
	MaxProcs    uint64
	GlobalCap   int

	// Host-mode fields:
	Enforce  bool   // run through the launcher (rlimits, no_new_privs, uid drop)
	SetNproc bool   // apply RLIMIT_NPROC — only sound when the uid actually drops
	DropUID  int    // target uid, -1 to keep current
	DropGID  int    // target gid, -1 to keep current
	Launcher string // self re-exec path override (tests)
}
