package runner

// Bubblewrap (§9.9): the one and only code-execution sandbox. Every script
// runs inside a per-run bwrap jail: fresh user/mount/PID/net/IPC/UTS/cgroup
// namespaces, a root filesystem containing nothing but the read-only host
// /usr (interpreter + libraries + zoneinfo), a minimal /dev and /proc, and
// the user's own scratchpad mounted at /scratchpad — work/ and artifacts/
// read-write, uploads/ overlaid read-only. The network namespace is dropped
// unless allow_network is set explicitly.
//
// The jail's visibility limits are the security boundary: the database,
// other users' scratchpads, server configs and secrets are not merely
// unreadable — no path to them exists inside the jail. Resource limits
// (RLIMIT_*) and no_new_privs still apply outermost, via the same re-exec
// launcher as before (§9.9): the launcher execve's bwrap instead of the
// interpreter directly, so rlimits are inherited by bwrap and the payload.
//
// If bwrap is missing or its namespaces do not work, mullsmith must refuse to
// start with code execution enabled (fail closed — NewForConfig/ProbeBwrap).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Bwrap is the sole production Runner.
type Bwrap struct {
	opts       Options
	gate       *gate
	cgroupRoot string // resolved at construction; "" = metering unavailable ⇒ Run refuses
}

// Options configures the sandbox. All zero values get safe defaults in
// NewBwrap; Launcher exists only so tests can re-exec the test binary.
type Options struct {
	Timeout      time.Duration
	MaxOutput    int
	MaxMemory    uint64 // RLIMIT_AS per process AND the per-run cgroup memory.max
	MaxFileSize  uint64 // RLIMIT_FSIZE
	GlobalCap    int
	NetworkOpen  bool   // allow_network: keep the net namespace + resolver files
	BwrapBin     string // resolved at startup; "" → LookPath("bwrap") in NewBwrap
	LauncherPath string // self re-exec path override (tests)
	CgroupRoot   string // per-run cgroup parent override (tests); "" → discovered
}

// ProbeBwrap proves the binary exists AND that the complete production jail
// works here (user namespaces can be disabled by kernel policy). A disposable
// scratchpad lets the probe use bwrapArgs itself, so a missing/changed bwrap
// option fails at startup rather than on the first code_run.
func ProbeBwrap(networkOpen bool) error {
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return errors.New("bwrap not found in PATH — install the bubblewrap package " +
			"or set tools.code_run.enabled: false (spec §9.9)")
	}
	scratchDir, err := os.MkdirTemp("", "mullsmith-bwrap-probe-")
	if err != nil {
		return fmt.Errorf("create bubblewrap probe scratchpad: %w", err)
	}
	defer os.RemoveAll(scratchDir)
	workDir := filepath.Join(scratchDir, "work")
	for _, dir := range []string{workDir, filepath.Join(scratchDir, "uploads"), filepath.Join(scratchDir, "artifacts")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("create bubblewrap probe directory: %w", err)
		}
	}
	target, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		return fmt.Errorf("resolve bubblewrap probe command: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 — bin is LookPath output and bwrapArgs constructs the same
	// fixed option shape used for production runs.
	cmd := exec.CommandContext(ctx, bin, bwrapArgs(scratchDir, workDir, target, networkOpen)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bubblewrap production-jail probe failed: %w: %s",
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
		opts.MaxOutput = 256 << 10
	}
	if opts.GlobalCap <= 0 {
		opts.GlobalCap = 4
	}
	root := opts.CgroupRoot
	if root == "" {
		root = DiscoverCgroupRoot()
	}
	return &Bwrap{opts: opts, gate: newGate(opts.GlobalCap), cgroupRoot: root}, nil
}

// bwrapArgs builds the jail specification: bwrap's OPTION list plus the
// payload command, WITHOUT argv[0] — the launcher prepends the program name
// before execve (sandbox.go, astra finding 3). scratchDir/workDir are HOST
// paths: the user's scratchpad root (mounted READ-ONLY at /scratchpad)
// and its work dir. Writable directories are work/ and artifacts/, each
// overlaid rw at its own position; uploads/ and the root stay read-only.
// target is the interpreter's host-resolved path (must be under /usr).
// Pure function: unit-testable without executing anything.
func bwrapArgs(scratchDir, workDir, target string, networkOpen bool) []string {
	// Where workDir sits inside the jail, computed from the caller's own
	// paths so the runner stays layout-agnostic (tmpfiles land on the
	// quota'd filesystem, not the jail's RAM).
	tmpInJail := "/scratchpad/work"
	if rel, err := filepath.Rel(scratchDir, workDir); err == nil {
		tmpInJail = path.Join("/scratchpad", filepath.ToSlash(rel))
	}
	args := []string{
		"--die-with-parent", // bwrap's parent is our launcher; server death reaches us
		"--unshare-all",     // user+ipc+net+pid+cgroup+uts+mount (§9.9)
		"--new-session",     // detach from any controlling terminal
		"--clearenv",        // payload env is exactly what we set below
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "HOME", tmpInJail, // $HOME must be writable: caches go to work/
		"--setenv", "TMPDIR", tmpInJail,
		"--setenv", "LANG", "C",
		"--setenv", "LC_ALL", "C",

		// The entire visible world: /usr holds interpreters, shared
		// libraries, terminfo and zoneinfo; /scratchpad is the user's own
		// file tree. Everything else simply has no path inside the jail.
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

	// The user's file tree, presented as the workflow intends it (§9.9):
	// the ROOT is read-only — scripts that scribble beside work/, uploads/
	// and artifacts/ corrupt the layout the UI and sweeper assume — so
	// the three designated directories are passed separately:
	//   work/       READ-WRITE (the script's workspace, and TMPDIR/$HOME)
	//   uploads/    READ-ONLY  (user data: readable in place, never mutated)
	//   artifacts/  READ-WRITE (a script may drop finished output there
	//              directly; like every artifact it ages out via mtime)
	// Later binds overlay the earlier root bind; bwrap applies them in
	// order. All three are hard binds (no -try): the tool layer ran
	// scratch.EnsureUserDirs, so a missing directory means a broken
	// layout — fail closed rather than hand the payload the root view.
	// Run() has already pinned these same sources fd-wise before building
	// the spec (verifyJailLayout, astra finding 4).
	return append(args,
		"--ro-bind", scratchDir, "/scratchpad",
		"--bind", workDir, tmpInJail,
		"--ro-bind", filepath.Join(scratchDir, "uploads"), "/scratchpad/uploads",
		"--bind", filepath.Join(scratchDir, "artifacts"), "/scratchpad/artifacts",
		"--chdir", "/scratchpad",
		target, "-", // interpreter; script arrives on stdin
	)
}

// verifyJailLayout pins the jail's bind sources before they are handed to
// bubblewrap (astra finding 4): the scratch dir is opened no-follow and the
// work path is descended component by component, so a layout directory
// substituted for a symlink fails closed here instead of becoming a bind of
// some unintended host tree. Residual, by design: stock bubblewrap has no
// bind-by-descriptor option, so it re-resolves the host STRINGS at mount
// time. The validate→mount window that remains is only reachable by a
// process that can already write the scratch root — never the jail itself,
// whose view of that tree is read-only, nor the fs_* tools, which cannot
// create symlinks.
func verifyJailLayout(scratchDir, workRel string) error {
	fd, err := unix.Open(scratchDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return errors.New("runner: scratch dir is a symlink or not a directory")
		}
		return errors.New("runner: scratch dir does not exist")
	}
	cur := os.NewFile(uintptr(fd), scratchDir)
	for _, s := range strings.Split(filepath.ToSlash(workRel), "/") {
		if s == "" || s == "." {
			continue
		}
		nfd, err := unix.Openat(int(cur.Fd()), s,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = cur.Close()
		if err != nil {
			if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return fmt.Errorf("runner: work dir path component %q is a symlink or not a directory", s)
			}
			return fmt.Errorf("runner: work dir does not exist: %w", err)
		}
		cur = os.NewFile(uintptr(nfd), s)
	}
	return cur.Close()
}

func (b *Bwrap) Run(ctx context.Context, userID string, req Request) (Outcome, error) {
	release, err := b.gate.acquire(ctx, userID)
	if err != nil {
		return Outcome{}, err
	}
	defer release()

	if req.Binary == "" || req.ScratchDir == "" || req.WorkDir == "" {
		return Outcome{}, errors.New("runner: binary, scratch dir and work dir are required")
	}
	rel, err := filepath.Rel(req.ScratchDir, req.WorkDir)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// "." is refused deliberately: the jail's only writable overlay is
		// WorkDir — letting it equal the root would re-expose everything.
		return Outcome{}, errors.New("runner: work dir must be a strict subdirectory inside the scratch dir")
	}
	if err := verifyJailLayout(req.ScratchDir, rel); err != nil {
		return Outcome{}, err
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

	// The per-run cgroup meter goes up BEFORE the launcher exists:
	// placement is via clone3(CGROUP), so no payload process can ever
	// run un-metered, and an arming failure means nothing runs at all
	// (audit2 finding 2).
	if b.cgroupRoot == "" {
		return Outcome{}, ErrNoCgroup
	}
	runCg, cgFile, err := StartRunCgroup(b.cgroupRoot, randHex(), mem)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: setup failed: %w", ErrNoCgroup, err)
	}
	defer cgFile.Close() // consumed by clone3; execve already hides it from the jail
	defer runCg.Finish() // sweep stragglers, remove the cgroup

	spec := sandboxSpec{
		Binary: b.opts.BwrapBin,
		Args:   bwrapArgs(req.ScratchDir, req.WorkDir, target, b.opts.NetworkOpen),
		Env:    minimalEnv(req.ScratchDir, req.WorkDir), // bwrap inherits; payload gets --clearenv
		Cwd:    req.ScratchDir,                          // superseded by --chdir /scratchpad, but keep cwd sane if bwrap fails early
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
		Setpgid:     true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true, // cloned straight into the run's cgroup — no move, no race
		CgroupFD:    int(cgFile.Fd()),
	}
	// The default cancel kills only the direct child; the whole group
	// (launcher, bwrap, payload) must go (§9.9 "kill process group"), and
	// cgroup.kill catches anything inside the jail that re-grouped itself
	// away from us.
	cmd.Cancel = func() error {
		runCg.KillAll()
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	var outBuf, errBuf cappedBuffer
	outBuf.max = maxOut
	errBuf.max = maxOut
	// os/exec owns the copy goroutines and joins them before Wait returns.
	// WaitDelay also bounds descendants that retain output descriptors.
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return Outcome{}, err
	}
	waitErr := cmd.Wait()
	oomKilled := runCg.OOMKilled() // read before the deferred Finish() drops the dir

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
	if oomKilled {
		// Without this line, an OOM-killed script is indistinguishable
		// from any other death — to the model and to the logs.
		out.Stderr = strings.TrimSuffix(out.Stderr, "\n") +
			"\n[mullsmith: the run's process tree exceeded its total memory budget; the kernel killed part of it]"
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
// mullsmith's own environment — config secrets, API keys — ever gets here.
func minimalEnv(scratchDir, workDir string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + scratchDir,
		"TMPDIR=" + workDir,
		"LANG=C",
		"LC_ALL=C",
	}
}
