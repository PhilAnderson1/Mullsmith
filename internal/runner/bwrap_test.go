package runner_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/runner"
)

// TestMain lets the test binary double as the sandbox launcher: when the
// bwrap runner re-execs it with __sandbox-exec, MaybeSandboxMain takes over
// before any test runs.
func TestMain(m *testing.M) {
	if runner.MaybeSandboxMain(os.Args) {
		return
	}
	os.Exit(m.Run())
}

// requireBwrap skips unless a real jail works here (user namespaces can be
// disabled by kernel policy — same question ProbeBwrap asks at startup).
func requireBwrap(t *testing.T) {
	t.Helper()
	if err := runner.ProbeBwrap(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}
}

func bwrapOptions(t *testing.T) runner.Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return runner.Options{
		Timeout: 10 * time.Second, MaxOutput: 1 << 20, MaxMemory: 256 << 20,
		MaxFileSize: 1 << 20, GlobalCap: 4, LauncherPath: exe,
		CgroupRoot: testCgroupRoot(t),
	}
}

// testCgroupRoot prepares the parent dir for test jail cgroups. It prefers
// the PRODUCTION discovery path (runner.DiscoverCgroupRoot: a Delegate=yes
// unit, a root-run cgroup v2 box, or the self-placement shape) so in-unit
// and sudo runs exercise exactly what the server does. Only when discovery
// finds nothing does it fall back to a dedicated delegated directory under
// the developer's user@.service.
//
// A clone probe gates more than directory writability: CLONE_INTO_CGROUP
// additionally requires the CALLER's own cgroup to be owned by it (or
// CAP_SYS_ADMIN). A plain login session's scope is root-owned, so
// unprivileged test runs skip here; `sudo -E go test` and production (a
// Delegate=yes unit chowned to its service user) both satisfy the rule.
var testCgroupOnce sync.Once
var testCgroupErr error
var testCgroupDir string

func testCgroupRoot(t *testing.T) string {
	t.Helper()
	testCgroupOnce.Do(func() {
		if dir := runner.DiscoverCgroupRoot(); dir != "" {
			testCgroupDir = dir // production discovery succeeded
		} else {
			uid := os.Getuid()
			dir := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/gk-runner-tests", uid, uid)
			if uid == 0 {
				dir = "/sys/fs/cgroup/gk-runner-tests" // root: skip user-session plumbing
			}
			if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
				testCgroupErr = err
				return
			}
			// Idempotent: re-listing an enabled controller is a no-op.
			if err := os.WriteFile(dir+"/cgroup.subtree_control",
				[]byte("+cpu +memory +pids"), 0o600); err != nil {
				testCgroupErr = err
				return
			}
			testCgroupDir = dir
		}
		// End-to-end probe: this is the whole question — can we clone into
		// a meter and run anything at all?
		exe, err := os.Executable()
		if err != nil {
			testCgroupErr = err
			return
		}
		b, err := runner.NewBwrap(runner.Options{
			Timeout: 10 * time.Second, MaxOutput: 1 << 16, MaxMemory: 64 << 20,
			MaxFileSize: 1 << 16, LauncherPath: exe, CgroupRoot: testCgroupDir,
		})
		if err != nil {
			testCgroupErr = err
			return
		}
		s := t.TempDir()
		for _, sub := range []string{"uploads", "artifacts", "work"} {
			if err := os.Mkdir(filepath.Join(s, sub), 0o700); err != nil {
				testCgroupErr = err
				return
			}
		}
		if _, err := b.Run(context.Background(), "userA", runner.Request{
			Binary: "/bin/sh", ScratchDir: s,
			WorkDir: filepath.Join(s, "work"), Script: "true",
		}); err != nil {
			testCgroupErr = err
		}
	})
	if testCgroupErr != nil {
		t.Skipf("per-run cgroup metering unavailable here (%v) — runner refuses to run un-metered; on the dev box use sudo -E go test, in production use Delegate=yes", testCgroupErr)
	}
	return testCgroupDir
}

func newJail(t *testing.T) *runner.Bwrap {
	t.Helper()
	requireBwrap(t)
	b, err := runner.NewBwrap(bwrapOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dirs mirrors the production scratch layout: <scratch>/{uploads,artifacts,work}.
type dirs struct{ scratch, work string }

func scratchDirs(t *testing.T) dirs {
	t.Helper()
	s := t.TempDir()
	for _, sub := range []string{"uploads", "artifacts", "work"} {
		if err := os.Mkdir(filepath.Join(s, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dirs{scratch: s, work: filepath.Join(s, "work")}
}

func (d dirs) req(script string) runner.Request {
	return runner.Request{
		Binary:     "/bin/sh",
		ScratchDir: d.scratch, WorkDir: d.work, Script: script,
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

// --- basic execution through the full launcher→bwrap chain ----------------

func TestJailRunExitCodeAndCwd(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)

	out, err := b.Run(t.Context(), "userA", d.req("pwd; echo hello; exit 3"))
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", out.ExitCode)
	}
	if !strings.Contains(out.Stdout, "hello") {
		t.Errorf("stdout = %q", out.Stdout)
	}
	if got, want := strings.TrimSpace(strings.Split(out.Stdout, "\n")[0]), "/scratchpad"; got != want {
		t.Errorf("cwd inside jail = %q, want %q", got, want)
	}
}

func TestJailScratchWritableAndVisibleOnHost(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	out, err := b.Run(t.Context(), "userA", d.req(
		`echo inside > work/wrote.txt
echo done > artifacts/final.txt && echo ARTIFACTS-OK
echo x > "loose file.txt" 2>/dev/null && echo "WRITE-ROOT(!)"
mkdir mydir 2>/dev/null && echo "MKDIR-ROOT(!)"
rm -f uploads 2>/dev/null
exit 0`))
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("run failed: %v %+v", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(d.work, "wrote.txt")); err != nil ||
		strings.TrimSpace(string(got)) != "inside" {
		t.Errorf("work/ write inside jail not visible on host: %q %v", got, err)
	}
	// Finished output may be written straight into artifacts/ (owner
	// policy): it must land on the host like any work/ file.
	if !strings.Contains(out.Stdout, "ARTIFACTS-OK") {
		t.Errorf("artifacts/ must be writable in jail: %q", out.Stdout)
	}
	if got, err := os.ReadFile(filepath.Join(d.scratch, "artifacts", "final.txt")); err != nil ||
		strings.TrimSpace(string(got)) != "done" {
		t.Errorf("artifacts/ write not visible on host: %q %v", got, err)
	}
	// The user root is read-only: nothing a script tries may appear
	// beside the three designated directories (root-litter fix).
	for _, forbidden := range []string{"WRITE-ROOT(!)", "MKDIR-ROOT(!)"} {
		if strings.Contains(out.Stdout, forbidden) {
			t.Errorf("jail let a script write where it must not (%s)", forbidden)
		}
	}
	for _, host := range []string{
		filepath.Join(d.scratch, "loose file.txt"),
		filepath.Join(d.scratch, "mydir"),
	} {
		if _, err := os.Stat(host); !os.IsNotExist(err) {
			t.Errorf("refused write still landed on host: %s", host)
		}
	}
}

// work/ and artifacts/ accept writes; uploads/ stays readable (the
// workflow reads inputs in place) but is the user's namespace: a script
// that mutates or deletes uploads would destroy what the user attached.
func TestJailWritableDirsAreWorkAndArtifacts(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	orig := filepath.Join(d.scratch, "uploads", "data.csv")
	if err := os.WriteFile(orig, []byte("a,b,c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(d.scratch, "artifacts", "shared.txt")
	if err := os.WriteFile(shared, []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := b.Run(t.Context(), "userA", d.req(`
cat uploads/data.csv                      # readable in place
cat artifacts/shared.txt                  # readable in place
echo "mutated" > uploads/data.csv 2>/dev/null && echo "WRITE-UPLOADS(!)"
rm -f uploads/data.csv 2>/dev/null && [ ! -e uploads/data.csv ] && echo "DELETE-UPLOADS(!)"
echo x > uploads/new-upload.txt 2>/dev/null && echo "CREATE-UPLOADS(!)"
echo x > artifacts/shared.txt && echo MUTATE-ARTIFACTS-OK
touch artifacts/generated.txt && echo CREATE-ARTIFACTS-OK
echo ok > work/script-output.txt && echo work-ok
exit 0   # the refused uploads writes above must not set the run's exit status
`))
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("run failed: %v %+v", err, out)
	}
	if !strings.Contains(out.Stdout, "a,b,c") || !strings.Contains(out.Stdout, "shared") {
		t.Errorf("uploads/artifacts not readable in jail: %q", out.Stdout)
	}
	for _, forbidden := range []string{
		"WRITE-UPLOADS(!)", "DELETE-UPLOADS(!)", "CREATE-UPLOADS(!)",
	} {
		if strings.Contains(out.Stdout, forbidden) {
			t.Errorf("jail write that must be refused succeeded (%s)", forbidden)
		}
	}
	for _, required := range []string{"work-ok", "MUTATE-ARTIFACTS-OK", "CREATE-ARTIFACTS-OK"} {
		if !strings.Contains(out.Stdout, required) {
			t.Errorf("jail refused a write that must succeed (%s): %q", required, out.Stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(d.work, "script-output.txt")); err != nil {
		t.Errorf("work/ write not visible on host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.scratch, "artifacts", "generated.txt")); err != nil {
		t.Errorf("artifacts/ write not visible on host: %v", err)
	}
	if b, err := os.ReadFile(orig); err != nil || string(b) != "a,b,c\n" {
		t.Errorf("original upload mutated on host: %q %v", b, err)
	}
}

func TestJailSignalExitNormalized(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	out, err := b.Run(t.Context(), "userA", d.req(`kill -TERM $$`))
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 128+15 {
		t.Errorf("exit = %d, want 143", out.ExitCode)
	}
}

// --- the jail boundary (audit finding 1: this is what was missing) ---------

func TestJailCannotSeeHostFiles(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	// A canary outside the scratchpad: the jail must not know it exists.
	outside := filepath.Join(t.TempDir(), "canary-outside.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := b.Run(t.Context(), "userA", d.req(`for p in `+outside+` /etc/mullsmith/env /var/lib/mullsmith/db/mullsmith.db /etc/passwd /home; do
  if [ -e "$p" ]; then echo "VISIBLE: $p"; else echo "no-path: $p"; fi
done
echo "scratch-entries: $(ls /scratchpad | tr '\n' ' ')"`))
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("run failed: %v %+v", err, out)
	}
	if strings.Contains(out.Stdout, "VISIBLE") {
		t.Errorf("host file visible from jail:\n%s", out.Stdout)
	}
	for _, want := range []string{"no-path: " + outside, "no-path: /etc/mullsmith/env", "no-path: /var/lib/mullsmith/db/mullsmith.db"} {
		if !strings.Contains(out.Stdout, want) {
			t.Errorf("missing observation %q in %q", want, out.Stdout)
		}
	}
	if got := findLine(t, out.Stdout, "scratch-entries:"); !strings.Contains(got, "artifacts") ||
		!strings.Contains(got, "uploads") || !strings.Contains(got, "work") {
		t.Errorf("scratchpad mount incomplete inside jail: %q", got)
	}
}

func TestJailRootIsReadOnly(t *testing.T) {
	b := newJail(t)
	out, err := b.Run(t.Context(), "userA", scratchDirs(t).req(`touch /bin/evil && echo "WRITE-TO-/(!)"`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Stdout, "WRITE-TO-/(!)") || out.ExitCode == 0 {
		t.Errorf("jail root filesystem was writable: exit=%d out=%q", out.ExitCode, out.Stdout)
	}
}

func TestJailNetworkBlockedByDefault(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	b := newJail(t)
	r := scratchDirs(t).req(`
import socket
try:
    socket.create_connection(("192.0.2.1", 80), timeout=3)
    print("NETWORK(!)")
except OSError as e:
    print("blocked:", e.strerror or e)
`)
	r.Binary = py
	out, err := b.Run(t.Context(), "userA", r)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("run failed: %v %+v", err, out)
	}
	if strings.Contains(out.Stdout, "NETWORK(!)") || !strings.Contains(out.Stdout, "blocked:") {
		t.Errorf("allow_network=false not enforced inside jail: %q", out.Stdout)
	}
}

func TestJailNetworkAllowedWhenConfigured(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	requireBwrap(t)
	o := bwrapOptions(t)
	o.NetworkOpen = true
	b, err := runner.NewBwrap(o)
	if err != nil {
		t.Fatal(err)
	}
	r := scratchDirs(t).req(`
import socket
try:
    socket.create_connection(("127.0.0.1", ` + strings.Split(ln.Addr().String(), ":")[1] + `, ), timeout=3)
    print("reachable")
except OSError as e:
    print("unreachable:", e)
`)
	r.Binary = py
	out, err := b.Run(t.Context(), "userA", r)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("run failed: %v %+v", err, out)
	}
	if !strings.Contains(out.Stdout, "reachable") {
		t.Errorf("allow_network=true should reach the host netns: %q %q", out.Stdout, out.Stderr)
	}
}

// --- always-on layers, observed from inside the jail -----------------------

func TestJailEnvIsScrubbed(t *testing.T) {
	t.Setenv("MULLSMITH_LLM_API_KEY", "SECRET-KEY")
	b := newJail(t)
	out, err := b.Run(t.Context(), "userA", scratchDirs(t).req(
		`echo "S=[${MULLSMITH_LLM_API_KEY}]"; echo "P=${PATH}"; echo "H=${HOME}"; echo "T=${TMPDIR}"`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Stdout, "SECRET-KEY") {
		t.Error("host environment leaked into the jail")
	}
	// HOME rides the writable work dir, not the (read-only) scratchpad root.
	for _, want := range []string{"P=/usr/bin:/bin", "H=/scratchpad/work", "T=/scratchpad/work"} {
		if !strings.Contains(out.Stdout, want) {
			t.Errorf("env allowlist missing %q: %q", want, out.Stdout)
		}
	}
}

func TestJailLauncherLayersLand(t *testing.T) {
	b := newJail(t)
	out, err := b.Run(t.Context(), "userA", func() runner.Request {
		r := scratchDirs(t).req(`grep NoNewPrivs /proc/self/status
echo "V=$(ulimit -v)"
echo "F=$(ulimit -f)"
echo "C=$(ulimit -c)"
echo "T=$(ulimit -t)"
echo "ENVN=$(env | grep -vE '^(_|PWD)=' | wc -l)"   # '_' and 'PWD' are the shell's own exports
echo "ARG0=$0"
echo "TMPWRITE=$(tmp=$(mktemp) && echo yes || echo no)"`)
		r.Timeout = 7 * time.Second
		r.MaxMemory = 64 << 20
		r.MaxFileSize = 1 << 20
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", out.ExitCode, out.Stderr)
	}
	want := map[string]string{
		"NoNewPrivs:": "1",     // prctl NO_NEW_PRIVS landed (inherited by bwrap + payload)
		"V=":          "65536", // RLIMIT_AS 64 MiB, in KiB
		"F=":          "2048",  // RLIMIT_FSIZE 1 MiB, in 512B blocks
		"C=":          "0",     // no core dumps
		"T=":          "7",     // RLIMIT_CPU = wall timeout
		"ENVN=":       "5",     // --clearenv + exactly the five --setenv vars
		"TMPWRITE=":   "yes",   // TMPDIR (work dir) is writable
	}
	for k, v := range want {
		line := findLine(t, out.Stdout, k)
		if !strings.Contains(line, v) {
			t.Errorf("%s layer: line %q does not carry %q (full stdout %q)", k, line, v, out.Stdout)
		}
	}
	// argv is [resolvedBinary -]: /bin/sh resolves to the real interpreter.
	arg0 := findLine(t, out.Stdout, "ARG0=")
	if !strings.HasSuffix(arg0, "sh") {
		t.Errorf("argv[0] = %q, want the allowlisted interpreter itself", arg0)
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

func TestJailMemoryCapKillsGreedyScript(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	b := newJail(t)
	r := scratchDirs(t).req(`
b = bytearray(1024 * 1024 * 1024)   # 1 GiB under a 256 MiB RLIMIT_AS
print("allocated", len(b))          # must never execute
`)
	r.Binary = py
	r.Timeout = 20 * time.Second
	r.MaxMemory = 256 << 20
	out, err := b.Run(t.Context(), "userA", r)
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

func TestJailOutputCap(t *testing.T) {
	b := newJail(t)
	r := scratchDirs(t).req(`head -c 100000 /dev/zero | tr '\0' 'x'`)
	r.MaxOutput = 1000
	out, err := b.Run(t.Context(), "userA", r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Stdout) != 1000 || !out.Truncated {
		t.Errorf("stdout len = %d trunc = %v, want 1000/true", len(out.Stdout), out.Truncated)
	}
}

// TestJailTimeoutKillsWholeTree proves the §9.9 "process group kill" promise
// through the jail: when Run returns TimedOut, the scripted grandchild's
// heartbeat must stop writing. (The grandchild lives in the jail's PID
// namespace, so its pids are deliberately NOT host-visible — the observable
// effect, an ever-growing file, is what we check.)
func TestJailTimeoutKillsWholeTree(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	start := time.Now()
	r := d.req(`sh -c 'while :; do printf x >> work/beats.log; sleep 0.2; done' & wait`)
	r.Timeout = 1 * time.Second
	out, err := b.Run(t.Context(), "userA", r)
	elapsed := time.Since(start)
	if !out.TimedOut {
		t.Fatalf("TimedOut = false (out %+v, err %v)", out, err)
	}
	if elapsed > 8*time.Second {
		t.Errorf("timeout took %v, want ~1s", elapsed)
	}
	beats := filepath.Join(d.work, "beats.log")
	if _, err := os.Stat(beats); err != nil {
		t.Fatalf("grandchild never started beating: %v", err)
	}
	s1, _ := os.Stat(beats)
	time.Sleep(1200 * time.Millisecond) // five heartbeat intervals
	s2, _ := os.Stat(beats)
	if s2.Size() == s1.Size() {
		return // no further writes: the whole tree died with the kill
	}
	t.Errorf("jail grandchild survived the timeout kill (%d -> %d bytes)", s1.Size(), s2.Size())
}

func TestJailCancelKillsRun(t *testing.T) {
	b := newJail(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := b.Run(ctx, "userCancel", func() runner.Request {
			r := scratchDirs(t).req("sleep 20")
			r.Timeout = 30 * time.Second
			return r
		}())
		done <- err
	}()
	time.Sleep(400 * time.Millisecond)
	cancel() // stop button / abort
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestPerUserBusy uses a marker file so the "slow" run is demonstrably in
// flight before the contention attempts start (a bare sleep race lost).
func TestPerUserBusy(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	ctx := t.Context()

	marker := filepath.Join(d.work, "running.marker")
	first := make(chan error, 1)
	go func() {
		r := d.req("echo started > work/running.marker; sleep 1.5")
		r.Timeout = 15 * time.Second
		_, err := b.Run(ctx, "userBusy", r)
		first <- err
	}()
	waitForFile(t, marker, 8*time.Second) // generous: includes jail startup

	// Same user while their run is in flight ⇒ refused, never queued (§9.9).
	deadline := time.Now().Add(time.Second) // safely inside the 1.5 s sleep
	for time.Now().Before(deadline) {
		r := d.req("true")
		r.Timeout = 1 * time.Second
		if _, err := b.Run(ctx, "userBusy", r); !errors.Is(err, runner.ErrBusy) {
			t.Fatalf("same-user run during in-flight run: err = %v, want ErrBusy", err)
		}
	}

	// A different user is unaffected by userBusy's slot.
	if _, err := b.Run(ctx, "userOther", d.req("true")); err != nil {
		t.Errorf("other user blocked by userBusy's run: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("slow run failed: %v", err)
	}
	// The slot frees when the run finishes.
	if _, err := b.Run(ctx, "userBusy", d.req("true")); err != nil {
		t.Errorf("user still busy after their run completed: %v", err)
	}
}

// --- input validation -------------------------------------------------------

func TestRunRefusesBinaryOutsideUsr(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	// Copy a working interpreter somewhere off /usr AND outside the
	// scratchpad: even though it runs fine on the host, the jail only
	// mounts /usr, so it must be refused with a clear error instead of a
	// dangling-path failure inside bwrap.
	hostile := filepath.Join(t.TempDir(), "pocket-interpreter")
	src, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostile, src, 0o755); err != nil {
		t.Fatal(err)
	}
	r := d.req("true")
	r.Binary = hostile
	if _, err := b.Run(t.Context(), "userA", r); err == nil || !strings.Contains(err.Error(), "/usr") {
		t.Fatalf("runtime outside /usr must be refused mentioning /usr, got %v", err)
	}
}

func TestRunRefusesMissingWorkDir(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	r := d.req("true")
	r.WorkDir = filepath.Join(d.scratch, "nope")
	if _, err := b.Run(t.Context(), "userA", r); err == nil || !strings.Contains(err.Error(), "work dir") {
		t.Fatalf("missing work dir must be refused, got %v", err)
	}
}

func TestRunRefusesWorkDirOutsideScratch(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	r := d.req("true")
	r.WorkDir = t.TempDir() // sibling, not inside the scratch mount
	if _, err := b.Run(t.Context(), "userA", r); err == nil ||
		!strings.Contains(err.Error(), "inside the scratch") {
		t.Fatalf("work dir outside scratch must be refused, got %v", err)
	}
}

func TestRunRefusesMissingScratchDir(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	r := d.req("true")
	r.ScratchDir = filepath.Join(t.TempDir(), "gone")
	if _, err := b.Run(t.Context(), "userA", r); err == nil ||
		!strings.Contains(err.Error(), "scratch dir") {
		t.Fatalf("missing scratch dir must be refused, got %v", err)
	}
}

// --- ProbeBwrap + NewForConfig startup validation (§9.9, fail closed) -------

func TestProbeBwrapSucceedsWhereAvailable(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap not installed here")
	}
	if err := runner.ProbeBwrap(); err != nil {
		t.Errorf("probe failed on an installed bwrap: %v", err)
	}
}

func codeExecConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Tools.CodeRun = config.CodeExecution{
		Enabled:         true,
		AllowedRuntimes: []config.Runtime{{ID: "sh", Binary: "/bin/sh"}},
		Timeout:         config.Duration(30 * time.Second),
		MaxOutput:       config.Size(32 << 10),
		MaxMemory:       config.Size(256 << 20),
	}
	cfg.Scratchpad.MaxFileSize = config.Size(1 << 20)
	return cfg
}

func TestNewForConfigDisabled(t *testing.T) {
	cfg := codeExecConfig()
	cfg.Tools.CodeRun.Enabled = false
	r, err := runner.NewForConfig(cfg)
	if r != nil || err != nil {
		t.Fatalf("disabled code exec must yield nothing, got r=%v err=%v", r, err)
	}
}

func TestNewForConfigEnabledBuildsJail(t *testing.T) {
	requireBwrap(t)
	r, err := runner.NewForConfig(codeExecConfig())
	if errors.Is(err, runner.ErrNoCgroup) {
		if r != nil {
			t.Fatal("refused startup must not yield a runner")
		}
		// Fail closed at startup IS the correct behaviour where no cgroup
		// subtree can be metered (plain login session). The full wiring is
		// exercised wherever a root exists: `sudo go test`, or any
		// Delegate=yes unit.
		t.Skipf("startup correctly refused: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("enabled code exec must yield a runner")
	}
	out, err := r.Run(t.Context(), "userA", scratchDirs(t).req(`ls / | tr '\n' ' '`))
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("production-built runner failed: %v %+v", err, out)
	}
	if strings.Contains(out.Stdout, "etc/mullsmith") || strings.Contains(out.Stdout, "var") {
		t.Errorf("unexpected host paths in jail root: %q", out.Stdout)
	}
}

// A caller asking for WorkDir == the scratch root must be refused: the
// work overlay is the jail's only writable mount, so that request would
// re-expose the whole tree.
func TestRunRefusesWorkDirEqualToRoot(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	r := d.req(`true`)
	r.WorkDir = d.scratch
	if _, err := b.Run(t.Context(), "userA", r); err == nil ||
		!strings.Contains(err.Error(), "subdirectory") {
		t.Errorf("Run(WorkDir == scratch root) = %v, want refusal", err)
	}
}

// Short-lived children may exit before either output reader is scheduled.
// Both streams must still be drained completely, without claiming truncation.
func TestJailDrainsOutputFromShortLivedChildren(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)
	for i := 0; i < 20; i++ {
		out, err := b.Run(t.Context(), "userA", d.req("printf 'complete stdout'; printf 'complete stderr' >&2"))
		if err != nil {
			t.Fatal(err)
		}
		if out.ExitCode != 0 || out.Stdout != "complete stdout" || out.Stderr != "complete stderr" || out.Truncated {
			t.Fatalf("run %d lost output: %+v", i, out)
		}
	}
}
