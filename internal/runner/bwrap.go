package runner

// Bubblewrap (§9.9): the one and only code-execution sandbox. Every script
// runs inside a per-run bwrap jail: fresh user/mount/PID/net/IPC/UTS/cgroup
// namespaces, a root filesystem containing nothing but the read-only host
// /usr (interpreter + libraries + zoneinfo), a minimal /dev and /proc, and
// the user's scratchpad work/ dir bind-mounted read-write at /work. The
// network namespace is dropped unless allow_network is set explicitly.
//
// The jail's visibility limits are the security boundary: the database,
// other users' scratchpads, server configs and secrets are not merely
// unreadable — no path to them exists inside the jail. Resource limits
// (RLIMIT_*) and no_new_privs still apply outermost, via the same re-exec
// launcher as before (§9.9): the launcher execve's bwrap instead of the
// interpreter directly, so rlimits are inherited by bwrap and the payload.
//
// If bwrap is missing or its namespaces do not work, gabkeep must refuse to
// start with code execution enabled (fail closed — NewForConfig/ProbeBwrap).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Bwrap is the sole production Runner.
type Bwrap struct {
	opts Options
	gate *gate
}

// Options configures the sandbox. All zero values get safe defaults in
// NewBwrap; Launcher exists only so tests can re-exec the test binary.
type Options struct {
	Timeout      time.Duration
	MaxOutput    int
	MaxMemory    uint64 // RLIMIT_AS
	MaxFileSize  uint64 // RLIMIT_FSIZE
	GlobalCap    int
	NetworkOpen  bool   // allow_network: keep the net namespace + resolver files
	BwrapBin     string // resolved at startup; "" → LookPath("bwrap") in NewBwrap
	LauncherPath string // self re-exec path override (tests)
}

// ProbeBwrap proves the binary exists AND that an actual jail works here
// (user namespaces can be disabled by kernel policy). It mirrors a real
// payload: same unsharing, same mount set, command /usr/bin/true.
func ProbeBwrap() error {
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return errors.New("bwrap not found in PATH — install the bubblewrap package " +
			"or set code_execution.enabled: false (spec §9.9)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 — bin is LookPath output; every argument is a literal.
	// Mirrors bwrapArgs' mount set so the probe answers the real question.
	cmd := exec.CommandContext(ctx, bin,
		"--die-with-parent", "--unshare-all",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--symlink", "usr/sbin", "/sbin",
		"--dev", "/dev", "--proc", "/proc",
		"/usr/bin/true")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bubblewrap probe failed (are unprivileged user namespaces enabled?): %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NewBwrap builds the runner; ProbeBwrap must have passed.
func NewBwrap(opts Options) (*Bwrap, error) {
	if opts.BwrapBin == "" {
		bin, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, err
		}
		opts.BwrapBin = bin
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
	return &Bwrap{opts: opts, gate: newGate(opts.GlobalCap)}, nil
}

// bwrapArgs builds the jail specification. workDir is the HOST path that
// becomes /work; target is the interpreter's host-resolved path (must be
// under /usr). Pure function: unit-testable without executing anything.
func bwrapArgs(workDir, target string, networkOpen bool) []string {
	args := []string{
		"--die-with-parent", // bwrap's parent is our launcher; server death reaches us
		"--unshare-all",     // user+ipc+net+pid+cgroup+uts+mount (§9.9)
		"--new-session",     // detach from any controlling terminal
		"--clearenv",        // payload env is exactly what we set below
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "HOME", "/work",
		"--setenv", "TMPDIR", "/work", // scratch stays on the quota'd filesystem
		"--setenv", "LANG", "C",
		"--setenv", "LC_ALL", "C",

		// The entire visible world: /usr holds interpreters, shared
		// libraries, terminfo and zoneinfo. Everything else simply has
		// no path inside the jail.
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin", // usr-merged layout, recreated inside
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--symlink", "usr/sbin", "/sbin",
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache", // -try: not on every distro
		"--dev", "/dev",
		"--proc", "/proc",
	}
	// --symlink has no -try variant, so host-dependent niceties are added
	// only after a stat (they are a convenience, not a requirement).
	if _, err := os.Stat("/etc/localtime"); err == nil {
		args = append(args, "--symlink", "/etc/localtime", "/etc/localtime")
	}

	if networkOpen {
		// Explicit operator opt-in (§9.6): keep net and make resolution
		// possible. -try variants tolerate stub-resolver layouts.
		args = append(args,
			"--share-net",
			"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
			"--ro-bind-try", "/etc/hosts", "/etc/hosts",
			"--ro-bind-try", "/etc/ssl", "/etc/ssl",
			"--ro-bind-try", "/etc/ssl/certs", "/etc/ssl/certs",
		)
	}

	return append(args,
		"--bind", workDir, "/work", // the one writable location
		"--chdir", "/work",
		target, "-", // interpreter; script arrives on stdin
	)
}

func (b *Bwrap) Run(ctx context.Context, userID string, req Request) (Outcome, error) {
	release, err := b.gate.acquire(ctx, userID)
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
	// Resolve the allowlisted binary to its real path and require it to
	// live under /usr — that is the only tree the jail mounts. Handles
	// /bin/sh → dash-style symlink farms honestly instead of breaking on
	// dangling links inside the jail.
	target, err := filepath.EvalSymlinks(req.Binary)
	if err != nil {
		return Outcome{}, fmt.Errorf("runner: runtime binary %q is not usable: %w", req.Binary, err)
	}
	if !strings.HasPrefix(target, "/usr/") {
		return Outcome{}, fmt.Errorf("runner: runtime binary %q resolves to %q; only paths under /usr are available inside the sandbox", req.Binary, target)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = b.opts.Timeout
	}
	maxOut := req.MaxOutput
	if maxOut <= 0 {
		maxOut = b.opts.MaxOutput
	}
	mem := req.MaxMemory
	if mem == 0 {
		mem = b.opts.MaxMemory
	}
	fsize := req.MaxFileSize
	if fsize == 0 {
		fsize = b.opts.MaxFileSize
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	spec := sandboxSpec{
		Binary: b.opts.BwrapBin,
		Args:   bwrapArgs(req.WorkDir, target, b.opts.NetworkOpen),
		Env:    minimalEnv(req.WorkDir), // bwrap inherits; payload gets --clearenv
		Cwd:    req.WorkDir,             // superseded by --chdir /work, but keep cwd sane if bwrap fails early
		UID:    -1,
		GID:    -1,
		As:     mem,
		CPU:    uint64(math.Ceil(timeout.Seconds())),
		FSize:  fsize,
		NoFile: 256,
		// Nproc stays 0: inside the jail's own user namespace the limit
		// would be applied before bwrap exists, counting the server's
		// own processes. The pid namespace + global gate bound fan-out.
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
	launcher := b.opts.LauncherPath
	if launcher == "" {
		exe, err := os.Executable()
		if err != nil {
			return Outcome{}, err
		}
		launcher = exe
	}
	// #nosec G204 -- launcher is /proc/self/exe (or the test binary); the
	// jail spec travels as data on fd 3, never as argv.
	cmd := exec.CommandContext(runCtx, launcher, SandboxArg)
	cmd.ExtraFiles = []*os.File{pr}
	cmd.Stdin = strings.NewReader(req.Script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	// The default cancel kills only the direct child; the whole group
	// (launcher, bwrap, payload) must go (§9.9 "kill process group").
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
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
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

// exitCode normalizes signal deaths to the shell convention 128+signum so
// the model sees a usable integer (-1 stays unknown).
func exitCode(ps *os.ProcessState) int {
	if st, ok := ps.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return 128 + int(st.Signal())
	}
	return ps.ExitCode()
}

// minimalEnv is the environment the LAUNCHER hands to bwrap (the payload's
// own env is rebuilt by --clearenv/--setenv inside bwrapArgs). Nothing from
// gabkeep's own environment — config secrets, API keys — ever gets here.
func minimalEnv(workDir string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + workDir,
		"TMPDIR=" + workDir,
		"LANG=C",
		"LC_ALL=C",
	}
}
