package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Host runs scripts as local subprocesses — the "isolated" mode when limits
// and the uid drop are enforced (via the launcher in sandbox.go) and the
// honest-to-goodness "raw" mode when they are not (§9.9). Both share the
// always-on layers: fresh process group, wall-clock kill, capped output,
// scrubbed environment, strict concurrency.
type hostRunner struct {
	opts     Options
	gate     *gate
	launcher string // re-exec path for the sandbox launcher
}

func NewHost(opts Options) (*hostRunner, error) {
	if opts.Enforce && opts.DropUID == 0 && opts.DropGID == 0 {
		// "unset" must never mean "drop to root"; callers state -1 to keep.
		return nil, errors.New("runner: Enforce requires DropUID/DropGID (-1 to keep)")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxOutput <= 0 {
		opts.MaxOutput = 32 << 10
	}
	if opts.GlobalCap <= 0 {
		opts.GlobalCap = 4
	}
	if opts.MaxProcs == 0 {
		opts.MaxProcs = 64
	}
	return &hostRunner{opts: opts, gate: newGate(opts.GlobalCap), launcher: exe}, nil
}

func (h *hostRunner) Mode() string {
	if h.opts.Enforce {
		return "isolated"
	}
	return "raw"
}

func (h *hostRunner) Run(ctx context.Context, userID string, req Request) (Outcome, error) {
	release, err := h.gate.acquire(ctx, userID)
	if err != nil {
		return Outcome{}, err
	}
	defer release()

	if req.Binary == "" || req.WorkDir == "" {
		return Outcome{}, errors.New("runner: binary and work dir are required")
	}
	if fi, err := os.Stat(req.WorkDir); err != nil || !fi.IsDir() {
		return Outcome{}, errors.New("runner: work dir does not exist")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = h.opts.Timeout
	}
	maxOut := req.MaxOutput
	if maxOut <= 0 {
		maxOut = h.opts.MaxOutput
	}
	env := minimalEnv(req.WorkDir)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if h.opts.Enforce {
		spec := sandboxSpec{
			Binary: req.Binary,
			Args:   []string{filepath.Base(req.Binary), "-"},
			Env:    env,
			Cwd:    req.WorkDir,
			UID:    h.opts.DropUID,
			GID:    h.opts.DropGID,
			As:     req.MaxMemory,
			CPU:    uint64(math.Ceil(timeout.Seconds())),
			FSize:  req.MaxFileSize,
			NoFile: 256,
		}
		if spec.As == 0 {
			spec.As = h.opts.MaxMemory
		}
		if spec.FSize == 0 {
			spec.FSize = h.opts.MaxFileSize
		}
		if spec.UID >= 0 && h.opts.SetNproc {
			spec.Nproc = h.opts.MaxProcs
		}
		pr, pw, err := os.Pipe()
		if err != nil {
			return Outcome{}, err
		}
		defer pr.Close()
		go func() { // spec is tiny; close signals EOF after write
			_ = json.NewEncoder(pw).Encode(spec)
			_ = pw.Close()
		}()
		// #nosec G204 -- launcher is /proc/self/exe (or the test binary); the
		// sandboxed program travels as data on fd 3, never as argv.
		cmd = exec.CommandContext(runCtx, h.launcherPath(), SandboxArg)
		cmd.ExtraFiles = []*os.File{pr}
	} else {
		// #nosec G204 -- req.Binary is resolved from the config runtime
		// allowlist in tools.CodeRun; the script is delivered on stdin.
		cmd = exec.CommandContext(runCtx, req.Binary, "-")
		cmd.Dir = req.WorkDir
		cmd.Env = env
	}
	cmd.Stdin = strings.NewReader(req.Script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	// The default cancel kills only the direct child; model code spawns —
	// the whole group must go (§9.9 "kill process group").
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{}, err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return Outcome{}, err
	}
	if err := cmd.Start(); err != nil {
		return Outcome{}, err
	}

	var wg sync.WaitGroup
	var outBuf, errBuf cappedBuffer
	outBuf.max = maxOut
	errBuf.max = maxOut
	for _, t := range []struct {
		r   io.Reader
		buf *cappedBuffer
	}{{outPipe, &outBuf}, {errPipe, &errBuf}} {
		wg.Add(1)
		go func(r io.Reader, buf *cappedBuffer) {
			defer wg.Done()
			_, _ = io.Copy(buf, r)
		}(t.r, t.buf)
	}
	waitErr := cmd.Wait()
	wg.Wait()

	out := Outcome{
		Stdout:    string(outBuf.take()),
		Stderr:    string(errBuf.take()),
		Truncated: outBuf.overflowed || errBuf.overflowed,
	}
	switch {
	case runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		out.TimedOut = true
		out.ExitCode = -1
	case runCtx.Err() != nil: // caller went away (stop button / abort)
		return out, runCtx.Err()
	case waitErr != nil:
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			out.ExitCode = exitCode(ee.ProcessState)
		} else {
			return out, waitErr
		}
	default:
		out.ExitCode = 0
	}
	return out, nil
}

func (h *hostRunner) launcherPath() string {
	if h.opts.Launcher != "" {
		return h.opts.Launcher
	}
	return h.launcher
}

// exitCode normalizes signal deaths to the shell convention 128+signum so
// the model sees a usable integer (-1 stays unknown).
func exitCode(ps *os.ProcessState) int {
	if st, ok := ps.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return 128 + int(st.Signal())
	}
	return ps.ExitCode()
}

// minimalEnv is the explicit allowlist (§9.9): nothing from gabkeep's own
// environment (config secrets, API keys) is inherited. HOME and TMPDIR
// point at the user's work dir so interpreter scratch writes land inside
// the quota.
func minimalEnv(workDir string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + workDir,
		"TMPDIR=" + workDir,
		"LANG=C",
		"LC_ALL=C",
	}
}
