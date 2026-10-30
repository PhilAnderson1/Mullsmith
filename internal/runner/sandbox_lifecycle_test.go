// Astra finding 3 regressions: syscall.Exec takes argv[0] as the program
// name, so feeding bwrapArgs directly as argv made bwrap read
// "--die-with-parent" as its own name and silently drop it. The launcher
// must prepend the binary; the jail's lifecycle safeguard must then really
// kill descendants when the jail's parent dies.
package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runLauncher re-executes the test binary as the launcher with spec on
// fd 3 (exactly the production contract) and returns the child's stdout.
func runLauncher(t *testing.T, exe string, spec sandboxSpec) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = json.NewEncoder(pw).Encode(spec)
		_ = pw.Close()
	}()
	cmd := exec.Command(exe, SandboxArg)
	cmd.ExtraFiles = []*os.File{pr}
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	_ = pr.Close()
	if err != nil {
		t.Fatalf("launcher failed: %v (%s)", err, out)
	}
	return string(out)
}

// TestLauncherArgvIsProgramNameFirst is the deterministic capture: the
// launcher execs a tiny script that reports $0 and $#, proving argv[0] is
// the program name and spec.Args arrive as the options the spec intended.
func TestLauncherArgvIsProgramNameFirst(t *testing.T) {
	dir := t.TempDir()
	cap := filepath.Join(dir, "argv.sh")
	script := "#!/bin/sh\nprintf 'argv0=%s\\n' \"$0\"\nprintf 'argc=%d\\n' \"$#\"\nprintf 'arg=%s\\n' \"$1\"\n"
	if err := os.WriteFile(cap, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := runLauncher(t, exe, sandboxSpec{
		Binary: cap,
		Args:   []string{"--die-with-parent", "--unshare-all"},
		Env:    []string{"PATH=/usr/bin:/bin"},
		UID:    -1,
		GID:    -1,
	})
	if got := wantLine(out, "argv0="); got != cap {
		t.Errorf("argv0 = %q, want %q — the program name must lead argv, not an option", got, cap)
	}
	if got := wantLine(out, "argc="); got != "2" {
		t.Errorf("argc = %s, want 2 (spec.Args verbatim):\n%s", got, out)
	}
	if got := wantLine(out, "arg="); got != "--die-with-parent" {
		t.Errorf("first option = %q, want --die-with-parent to arrive as an argument", got)
	}
}

// TestDieWithParentKillsTheJail is the end-to-end counterpart. Process
// shape: test → sh → launcher (execs into bwrap) → jail. SIGKILLing sh is
// the "parent vanished" case: with the option actually parsed, bwrap (and,
// through it, the whole PID namespace) dies; without it, bwrap reparents to
// init and the payload outlives the chain. Requires a working bwrap — the
// launcher path needs no cgroup placement, so this runs wherever user
// namespaces do.
func TestDieWithParentKillsTheJail(t *testing.T) {
	if err := ProbeBwrap(); err != nil {
		t.Skipf("bubblewrap unavailable: %v", err)
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep unavailable")
	}
	if _, err := exec.LookPath("pkill"); err != nil {
		t.Skip("pkill unavailable")
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skipf("bwrap not found: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	scratchDir := filepath.Join(dir, "sp")
	workDir := filepath.Join(scratchDir, "work")
	if err := os.Mkdir(scratchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"uploads", "artifacts", "work"} {
		if err := os.Mkdir(filepath.Join(scratchDir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spec := sandboxSpec{
		Binary: bin,
		Args:   bwrapArgs(scratchDir, workDir, "/usr/bin/sh", false),
		Env:    []string{"PATH=/usr/bin:/bin"},
		Cwd:    scratchDir,
		UID:    -1,
		GID:    -1,
		As:     128 << 20,
		NoFile: 64,
	}
	specFile := filepath.Join(dir, "spec.json")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	const magic = "sleep 31337"
	// No exec in the wrapper: sh must STAY the parent of the launcher so
	// its death is what --die-with-parent reacts to.
	cmd := exec.Command("/bin/sh", "-c", `exec 3< "$1"; "$2" __sandbox-exec`, "sh", specFile, exe)
	cmd.Stdin = strings.NewReader(magic + "\n")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = exec.Command("pkill", "-9", "-f", magic).Run() // best effort, failure paths
	}()

	visible := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", magic).Run() == nil {
			visible = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !visible {
		t.Skip("jail payload never became visible to pgrep")
	}

	_ = cmd.Process.Kill() // abrupt: exactly the daemon-death case
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", magic).Run() != nil {
			return // payload gone: --die-with-parent reached bwrap
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("jail payload outlived a SIGKILLed parent — --die-with-parent was not processed")
}

func wantLine(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix)
		}
	}
	return ""
}
