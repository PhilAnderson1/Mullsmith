// Package runner executes model-authored scripts inside a user's scratchpad
// work/ directory with the defense layers of spec §9.9: an allowlisted
// interpreter, the script delivered on stdin (never argv), a scrubbed
// environment, resource limits applied in the child before exec,
// process-group kill on timeout, capped output, strict concurrency (one run
// per user, bounded global parallelism), and — always — a bubblewrap jail
// with its own namespaces: no network (unless explicitly allowed), no
// visibility of host files beyond /usr and the one writable work dir.
//
// It is deliberately separate from internal/tools so the sandbox machinery
// can be audited and tested on its own; CodeRun is a thin adapter over it.
package runner

import (
	"context"
	"errors"
	"sync"
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
	Binary      string        // allowlisted absolute path (host path)
	WorkDir     string        // absolute; the user's work/ dir, mounted at /work
	Script      string        // fed to the interpreter on stdin
	Timeout     time.Duration // wall clock; process group killed past it
	MaxOutput   int           // bytes retained per stream (stdout/stderr)
	MaxMemory   uint64        // RLIMIT_AS bytes
	MaxFileSize uint64        // RLIMIT_FSIZE bytes
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

// NewForConfig builds the (bubblewrap) runner when code execution is
// enabled. The probe is a hard requirement: gabkeep must not serve
// code_run promises it cannot enforce, so a missing or non-functional
// bwrap is a startup error, never a silent degradation.
func NewForConfig(cfg *config.Config) (Runner, []string, error) {
	ce := &cfg.Scratchpad.CodeExec
	if !ce.Enabled {
		return nil, nil, nil
	}
	if err := ProbeBwrap(); err != nil {
		return nil, nil, err
	}
	var warn []string
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
	r, err := NewBwrap(Options{
		Timeout:     timeout,
		MaxOutput:   maxOut,
		MaxMemory:   maxMem,
		MaxFileSize: uint64(cfg.Scratchpad.MaxFileSize), // #nosec G115 -- config.Size parses only non-negative values
		NetworkOpen: ce.AllowNetwork,
	})
	return r, warn, err
}
