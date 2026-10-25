package runner_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/runner"
)

// TestMain lets the test binary double as the sandbox launcher: when the
// host runner re-execs it with __sandbox-exec, MaybeSandboxMain takes over
// before any test runs.
func TestMain(m *testing.M) {
	if runner.MaybeSandboxMain(os.Args) {
		return
	}
	os.Exit(m.Run())
}

func selfExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func rawOptions(t *testing.T) runner.Options {
	t.Helper()
	return runner.Options{
		Timeout: 10 * time.Second, MaxOutput: 1 << 20, MaxMemory: 256 << 20,
		MaxFileSize: 1 << 20, GlobalCap: 4,
	}
}

// isolatedOptions: launcher-enforced (rlimits + no_new_privs). As non-root
// the uid stays (-1); the spec is still delivered on fd 3 and every other
// layer lands, which is exactly what these tests observe.
func isolatedOptions(t *testing.T) runner.Options {
	t.Helper()
	o := rawOptions(t)
	o.Enforce = true
	o.DropUID, o.DropGID = -1, -1
	o.Launcher = selfExe(t)
	return o
}

func TestRawRunExitCodeAndCwd(t *testing.T) {
	h, err := runner.NewHost(rawOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()

	out, err := h.Run(t.Context(), "userA", runner.Request{
		RuntimeID: "sh", Binary: "/bin/sh", WorkDir: wd,
		Script: "pwd; echo hello; exit 3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", out.ExitCode)
	}
	if !strings.Contains(out.Stdout, "hello") {
		t.Errorf("stdout = %q", out.Stdout)
	}
	got := strings.TrimSpace(strings.Split(out.Stdout, "\n")[0])
	if g2, _ := filepath.EvalSymlinks(got); g2 != mustEval(t, wd) {
		t.Errorf("cwd = %q, want %q", got, wd)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRawRunSignalExitNormalized(t *testing.T) {
	h, err := runner.NewHost(rawOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Run(t.Context(), "userA", runner.Request{
		Binary: "/bin/sh", WorkDir: t.TempDir(), Script: `kill -TERM $$`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 128+15 {
		t.Errorf("exit = %d, want 143", out.ExitCode)
	}
}

func TestEnvIsScrubbed(t *testing.T) {
	t.Setenv("GABKEEP_LLM_API_KEY", "SECRET-KEY")
	h, err := runner.NewHost(rawOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Run(t.Context(), "userA", runner.Request{
		Binary: "/bin/sh", WorkDir: t.TempDir(),
		Script: `echo "S=[${GABKEEP_LLM_API_KEY}]"; echo "P=${PATH}"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Stdout, "SECRET-KEY") {
		t.Error("host environment leaked into the run")
	}
	if !strings.Contains(out.Stdout, "/usr/bin") {
		t.Errorf("PATH allowlist missing: %q", out.Stdout)
	}
}

func TestOutputCap(t *testing.T) {
	h, err := runner.NewHost(rawOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Run(t.Context(), "userA", runner.Request{
		Binary: "/bin/sh", WorkDir: t.TempDir(), MaxOutput: 1000,
		Script: `head -c 100000 /dev/zero | tr '\0' 'x'`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Stdout) != 1000 || !out.Truncated {
		t.Errorf("stdout len = %d trunc = %v, want 1000/true", len(out.Stdout), out.Truncated)
	}
}

func TestTimeoutKillsWholeGroup(t *testing.T) {
	h, err := runner.NewHost(rawOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	start := time.Now()
	out, err := h.Run(t.Context(), "userA", runner.Request{
		Binary: "/bin/sh", WorkDir: wd, Timeout: 1 * time.Second,
		Script: "sleep 60 & echo $! > child.pid; wait",
	})
	elapsed := time.Since(start)
	if !out.TimedOut {
		t.Fatalf("TimedOut = false (out %+v, err %v)", out, err)
	}
	if elapsed > 8*time.Second {
		t.Errorf("timeout took %v, want ~1s", elapsed)
	}
	// The backgrounded grandchild must die with the process group, not
	// linger as an orphan (§9.9 "kill process group").
	b, err := os.ReadFile(filepath.Join(wd, "child.pid"))
	if err != nil {
		t.Fatalf("grandchild never recorded its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("bad pid file %q: %v", b, err)
	}
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(proc); err != nil || !st.IsDir() {
			return // gone: the whole group died
		}
		// A zombie is already dead (awaiting reap); its cmdline is empty.
		if cb, err := os.ReadFile(filepath.Join(proc, "cmdline")); err == nil && len(cb) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // don't leak the survivor
	t.Errorf("grandchild pid %d survived the process-group kill", pid)
}

func TestCancelKillsRun(t *testing.T) {
	h, err := runner.NewHost(rawOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	wd := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := h.Run(ctx, "userCancel", runner.Request{
			Binary: "/bin/sh", WorkDir: wd, Timeout: 30 * time.Second,
			Script: "sleep 20",
		})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel() // stop button / abort
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestPerUserBusy uses a marker file so the "slow" run is demonstrably in
// flight before the contention attempts start (a bare sleep race lost).
func TestPerUserBusy(t *testing.T) {
	h, err := runner.NewHost(rawOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	ctx := t.Context()

	marker := filepath.Join(wd, "running.marker")
	first := make(chan error, 1)
	go func() {
		_, err := h.Run(ctx, "userBusy", runner.Request{
			Binary: "/bin/sh", WorkDir: wd, Timeout: 5 * time.Second,
			Script: "echo started > running.marker; sleep 1.5",
		})
		first <- err
	}()
	waitForFile(t, marker, 3*time.Second)

	// Same user while their run is in flight ⇒ refused, never queued (§9.9).
	deadline := time.Now().Add(time.Second) // safely inside the 1.5 s sleep
	for time.Now().Before(deadline) {
		if _, err := h.Run(ctx, "userBusy", runner.Request{
			Binary: "/bin/sh", WorkDir: wd, Timeout: 1 * time.Second, Script: "true",
		}); !errors.Is(err, runner.ErrBusy) {
			t.Fatalf("same-user run during in-flight run: err = %v, want ErrBusy", err)
		}
	}

	// A different user is unaffected by userBusy's slot.
	if _, err := h.Run(ctx, "userOther", runner.Request{
		Binary: "/bin/sh", WorkDir: wd, Timeout: 5 * time.Second, Script: "true",
	}); err != nil {
		t.Errorf("other user blocked by userBusy's run: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("slow run failed: %v", err)
	}
	// The slot frees when the run finishes.
	if _, err := h.Run(ctx, "userBusy", runner.Request{
		Binary: "/bin/sh", WorkDir: wd, Timeout: 5 * time.Second, Script: "true",
	}); err != nil {
		t.Errorf("user still busy after their run completed: %v", err)
	}
}

func waitForFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- launcher (isolated) layer observations --------------------------------

func TestIsolatedLauncherAppliesLayers(t *testing.T) {
	h, err := runner.NewHost(isolatedOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	out, err := h.Run(t.Context(), "userA", runner.Request{
		Binary: "/bin/sh", WorkDir: wd, Timeout: 7 * time.Second,
		MaxMemory: 64 << 20, MaxFileSize: 1 << 20,
		Script: `grep NoNewPrivs /proc/self/status
echo "V=$(ulimit -v)"
echo "F=$(ulimit -f)"
echo "C=$(ulimit -c)"
echo "T=$(ulimit -t)"
echo "ENVN=$(env | grep -vE '^(_|PWD)=' | wc -l)"   # '_' and 'PWD' are the shell's own exports
echo "ARG0=$0"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", out.ExitCode, out.Stderr)
	}
	want := map[string]string{
		"NoNewPrivs:": "1",     // prctl NO_NEW_PRIVS landed
		"V=":          "65536", // RLIMIT_AS 64 MiB, in KiB
		"F=":          "2048",  // RLIMIT_FSIZE 1 MiB, in 512B blocks
		"C=":          "0",     // no core dumps
		"T=":          "7",     // RLIMIT_CPU = wall timeout
		"ENVN=":       "5",     // exactly the allowlist
		"ARG0=":       "sh",    // argv is [sh -], script on stdin
	}
	for k, v := range want {
		line := findLine(t, out.Stdout, k)
		if !strings.Contains(line, v) {
			t.Errorf("%s layer: line %q does not carry %q (full stdout %q)", k, line, v, out.Stdout)
		}
	}
}

func findLine(t *testing.T, haystack, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(haystack, "\n") {
		if strings.HasPrefix(l, prefix) || strings.Contains(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line with %q in %q", prefix, haystack)
	return ""
}

func TestIsolatedMemoryCapKillsGreedyScript(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	h, err := runner.NewHost(isolatedOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Run(t.Context(), "userA", runner.Request{
		Binary: py, WorkDir: t.TempDir(), Timeout: 20 * time.Second,
		MaxMemory: 256 << 20,
		Script: `
b = bytearray(1024 * 1024 * 1024)   # 1 GiB under a 256 MiB RLIMIT_AS
print("allocated", len(b))          # must never execute
`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode == 0 || strings.Contains(out.Stdout, "allocated") {
		t.Errorf("1 GiB allocation survived the memory cap: exit=%d out=%q", out.ExitCode, out.Stdout)
	}
	if !strings.Contains(out.Stderr, "MemoryError") {
		t.Errorf("want MemoryError in stderr, got %q", out.Stderr)
	}
}

// --- NewForConfig startup validation (§9.9) ---------------------------------

func codeExecConfig(mode, runAs string) *config.Config {
	cfg := &config.Config{}
	cfg.Scratchpad.CodeExec = config.CodeExecution{
		Enabled: true, Mode: mode, RunAsUser: runAs,
		AllowedRuntimes: []config.Runtime{{ID: "sh", Binary: "/bin/sh"}},
		Timeout:         config.Duration(30 * time.Second),
		MaxOutput:       config.Size(32 << 10),
		MaxMemory:       config.Size(256 << 20),
	}
	cfg.Scratchpad.MaxFileSize = config.Size(1 << 20)
	return cfg
}

func TestNewForConfigDisabled(t *testing.T) {
	cfg := codeExecConfig("raw", "")
	cfg.Scratchpad.CodeExec.Enabled = false
	r, warns, err := runner.NewForConfig(cfg)
	if r != nil || err != nil || len(warns) != 0 {
		t.Fatalf("disabled code exec must yield nothing, got r=%v warns=%v err=%v", r, warns, err)
	}
}

func TestNewForConfigRawWarns(t *testing.T) {
	r, warns, err := runner.NewForConfig(codeExecConfig("raw", ""))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Mode() != "raw" {
		t.Fatalf("mode = %+v, want raw", r)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "raw") {
		t.Errorf("raw mode must warn, got %v", warns)
	}
}

func TestNewForConfigIsolatedSelf(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	r, warns, err := runner.NewForConfig(codeExecConfig("isolated", u.Name))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Mode() != "isolated" {
		t.Fatalf("want an isolated runner, got %v", r)
	}
	if syscall.Geteuid() == 0 {
		return // real uid drop, nothing degraded
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "DEGRADED") {
			found = true
		}
	}
	if !found {
		t.Errorf("non-root self-drop must warn DEGRADED, got %v", warns)
	}
}

func TestNewForConfigIsolatedUnknownUser(t *testing.T) {
	_, _, err := runner.NewForConfig(codeExecConfig("isolated", "no-such-user-xyz"))
	if err == nil || !strings.Contains(err.Error(), "run_as_user") {
		t.Fatalf("unknown run_as_user must be a config error, got %v", err)
	}
}

func TestNewForConfigIsolatedOtherUser(t *testing.T) {
	// "nobody" exists on any Linux box; a non-root gabkeep cannot drop to
	// it, so this must refuse to start rather than pretend (§9.9).
	if syscall.Geteuid() == 0 {
		t.Skip("running as root: the drop would succeed")
	}
	_, _, err := runner.NewForConfig(codeExecConfig("isolated", "nobody"))
	if err == nil {
		t.Fatal("non-root + foreign run_as_user must be a config error")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("error should explain the root requirement: %v", err)
	}
}

func TestNewForConfigContainerWithoutDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err == nil {
		t.Skip("docker is present here; refusal path not applicable")
	}
	_, _, err := runner.NewForConfig(codeExecConfig("container", ""))
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("container mode without docker must be a config error, got %v", err)
	}
}
