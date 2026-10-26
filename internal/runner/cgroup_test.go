package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gabkeep/internal/runner"
)

// --- audit2 finding 2: aggregate (per-run, not per-process) resource caps ---

// Discovery must never return a root that later fails at Run time: it
// clone-probes what it hands back. And it must leave no probe dirs behind.
func TestDiscoverCgroupRootIsArmedAndClean(t *testing.T) {
	root := runner.DiscoverCgroupRoot()
	if root == "" {
		t.Skip("no meterable cgroup subtree here (undelegated login session) — startup would refuse too")
	}
	rc, fd, err := runner.StartRunCgroup(root, "discover-test", 32<<20)
	if err != nil {
		t.Fatalf("discovered root %s cannot arm a meter: %v", root, err)
	}
	defer fd.Close()
	defer rc.Finish()
	if rc.OOMKilled() {
		t.Errorf("fresh meter reports oom_kill")
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "gk-jail-probe-") {
			t.Errorf("discovery left probe cgroup %q under %s", e.Name(), root)
		}
	}
}

func jailWithMemory(t *testing.T, mem uint64) *runner.Bwrap {
	t.Helper()
	requireBwrap(t)
	opts := bwrapOptions(t)
	opts.MaxMemory = mem
	b, err := runner.NewBwrap(opts)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 not installed")
	}
}

// The audit's repro made concrete: RLIMIT_AS resets for every child, so
// children that each FIT their allowance blow the total. Six pythons ×
// 12 MiB anon demand (~72 MiB+, plus interpreter heft each) against a
// 64 MiB run budget: the cgroup meter has no per-process loophole.
func TestJailAggregateMemoryBudgetKillsMultiplyingChildren(t *testing.T) {
	requirePython3(t)
	b := jailWithMemory(t, 64<<20)
	d := scratchDirs(t)

	script := `for i in 1 2 3 4 5 6; do
  python3 -c 'import time; b = bytearray(12*1024*1024); print("ok", flush=True); time.sleep(1)' &
done
wait
echo reaped`
	out, err := b.Run(t.Context(), "userA", d.req(script))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Stderr, "exceeded its total memory budget") {
		t.Errorf("multiplying children blew 64 MiB without the cgroup objecting: exit=%d stdout=%q stderr=%q",
			out.ExitCode, out.Stdout, out.Stderr)
	}
}

// Layering pin: a SINGLE fat process is caught by its own RLIMIT_AS first —
// clean MemoryError, no kernel OOM. The cgroup exists for the aggregate,
// not to replace the cheap per-process guard.
func TestJailSingleProcessHitsRlimitNotCgroup(t *testing.T) {
	requirePython3(t)
	b := jailWithMemory(t, 64<<20)
	d := scratchDirs(t)

	out, err := b.Run(t.Context(), "userA", d.req(
		`python3 -c 'b = bytearray(200*1024*1024)' 2>&1 | tail -1`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Stdout, "MemoryError") {
		t.Errorf("expected RLIMIT MemoryError, got exit=%d out=%q", out.ExitCode, out.Stdout)
	}
	if strings.Contains(out.Stderr, "exceeded its total memory budget") {
		t.Errorf("cgroup OOM fired where RLIMIT should have: stderr=%q", out.Stderr)
	}
}

// pids.max: 300 background spawns inside a 128-task meter must produce
// fork refusals inside the jail rather than 300 tasks on the host.
func TestJailPidsMaxRefusesFanout(t *testing.T) {
	b := newJail(t)
	d := scratchDirs(t)

	out, err := b.Run(t.Context(), "userA", d.req(
		`i=0; while [ $i -lt 300 ]; do sleep 5 & i=$((i+1)); done; echo tried-$i`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Stdout, "tried-300") && !strings.Contains(out.Stderr, "available") {
		t.Errorf("300 background spawns succeeded under pids.max=%s: stderr=%q",
			runner.JailPidsMax, out.Stderr)
	}
}

// Every successful run must leave no gk-jail-* cgroups behind.
func TestJailCgroupsCleanedUpAfterRun(t *testing.T) {
	root := bwrapOptions(t).CgroupRoot
	b := newJail(t)
	d := scratchDirs(t)
	if _, err := b.Run(t.Context(), "userA", d.req("echo clean")); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), runner.JailDirPrefix) {
			t.Errorf("leftover jail cgroup %q under %s", e.Name(), root)
		}
	}
}

// Fail closed: a broken meter parent means NOTHING runs — not even the
// script's first side effect.
func TestRunRefusedWithoutCgroupMeter(t *testing.T) {
	requireBwrap(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := runner.NewBwrap(runner.Options{
		LauncherPath: exe, CgroupRoot: "/proc/self/definitely-not-a-cgroup",
	})
	if err != nil {
		t.Fatal(err)
	}
	d := scratchDirs(t)
	marker := filepath.Join(d.scratch, "ran") // payload cwd is /scratchpad
	out, err := b.Run(t.Context(), "userA", d.req("touch ran"))
	if err == nil {
		t.Fatalf("run succeeded without metering: %+v", out)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("script executed despite refusal — fail-closed violated")
	}
}
