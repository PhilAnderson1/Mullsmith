package runner

// Per-run cgroups (§9.9, audit2 finding 2): RLIMIT_AS/CPU bound EACH process,
// so a payload's children each received a fresh 256 MiB allowance and a
// multiplying script could balloon past the whole machine. The cgroup v2
// subtree adds the AGGREGATE ceiling the rlimit layer structurally cannot:
// one meter per run — memory (no swap), task count and CPU across the entire
// process tree. Placement is race-free: the launcher is cloned directly into
// the run's cgroup (clone3 CLONE_INTO_CGROUP via SysProcAttr.UseCgroupFD),
// so no payload process can exist before the meter does, and everything the
// jail's namespaces contain still charges to it (namespaces hide paths, not
// accounting).
//
// The meter lives in a DELEGATED cgroup v2 subtree: a systemd unit adds
// Delegate=yes (+ DelegateSubgroup=payload on systemd ≥ 254, which starts the
// service in a child cgroup — the kernel forbids controllers where processes
// live; without it, this file's self-placement does the same move). The
// delegatee — mullsmith — owns its subtree and enables the controllers itself;
// systemd deliberately leaves that to us.
//
// Usability is PROVEN, not assumed: DiscoverCgroupRoot only accepts a root it
// can mkdir in, arm with memory+pids+cpu controllers, and actually clone a
// process into (the migration-permission rule bites even where the directory
// is writable — e.g. a developer's login session). Fail closed: no proven
// root ⇒ Run refuses, and NewForConfig refuses at startup — same contract as
// a missing bwrap. Nothing un-metered runs.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// JailPidsMax caps TASKS per run. Legitimate payloads (interpreter +
	// a handful of subprocesses) stay far below; a fork bomb hits the wall
	// inside its own cgroup instead of the unit's TasksMax.
	JailPidsMax = "128"
	// JailCPUQuota caps CPU per run at one core (100 ms period, 100 ms
	// quota). With GlobalCap 4 that bounds all jails at 4 cores.
	JailCPUQuota = "100000 100000"
	// JailDirPrefix names the per-run child cgroups (tests and the stale
	// sweep look for it).
	JailDirPrefix = "gk-jail-"
	// selfSubgroup is where mullsmith moves its OWN process when the
	// delegation root is its unit cgroup and the kernel's
	// no-internal-processes rule blocks enabling controllers there
	// (systemd < 254 with no DelegateSubgroup). Sibling of the jails.
	selfSubgroup = "gk-payload"
)

// jailControllers are the controllers a root must expose on child cgroups.
// All three or nothing: a half-metered jail contradicts the fail-closed
// doctrine.
var jailControllers = []string{"cpu", "memory", "pids"}

func randHex() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil { // crypto/rand never fails on linux
		return "000000000000"
	}
	return hex.EncodeToString(b)
}

// ownCgroup returns our v2 cgroup path ("0::" line), "" when there is none
// or we sit at the hierarchy root.
func ownCgroup() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok { // v2 hierarchy entry
			if rest == "" || rest == "/" {
				return ""
			}
			return rest
		}
	}
	return ""
}

// enableControllers idempotently delegates jailControllers one level down
// (the contract systemd hands to a delegated payload: the write is OURS to
// make). Missing controllers are ignored — probeUsable checks the outcome.
func enableControllers(dir string) {
	want := map[string]bool{}
	// #nosec G304 -- dir is a cgroupfs path assembled by this package.
	if b, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control")); err == nil {
		for _, c := range strings.Fields(string(b)) {
			want[c] = true
		}
	}
	var missing []string
	for _, c := range jailControllers {
		if !want[c] {
			missing = append(missing, "+"+c)
		}
	}
	if len(missing) == 0 {
		return
	}
	// One batch write; the kernel is all-or-nothing per write, so retry
	// individually to keep whatever subset works on odd kernels.
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"),
		[]byte(strings.Join(missing, " ")), 0o600); err != nil {
		for _, c := range missing {
			_ = os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"),
				[]byte(c), 0o600)
		}
	}
}

// probeUsable answers the whole question for one candidate dir: we may
// create children here, those children carry memory/pids/cpu interface
// files, AND we may clone a process into one (the kernel requires write
// access to source and destination for unprivileged migration — a merely
// world-listable ancestor fails, which is exactly the distinction the walk
// needs). The probe child is /bin/true: no budget risk, zero side effects.
func probeUsable(dir string) bool {
	probe := filepath.Join(dir, JailDirPrefix+"probe-"+randHex())
	if err := os.Mkdir(probe, 0o700); err != nil {
		return false
	}
	defer syscall.Rmdir(probe)
	enableControllers(dir)
	for _, f := range []string{"memory.max", "pids.max", "cpu.max"} {
		if _, err := os.Stat(filepath.Join(probe, f)); err != nil {
			return false
		}
	}
	trueBin, err := exec.LookPath("true")
	if err != nil {
		if _, statErr := os.Stat("/bin/true"); statErr != nil {
			return false // a system without /bin/true is not our target
		}
		trueBin = "/bin/true"
	}
	// #nosec G304 -- probe is a cgroupfs path this function just mkdir'ed.
	fd, err := os.Open(probe)
	if err != nil {
		return false
	}
	defer fd.Close()
	// #nosec G204 -- trueBin is LookPath output / the literal /bin/true.
	cmd := exec.Command(trueBin)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    int(fd.Fd()),
		Pdeathsig:   syscall.SIGKILL,
	}
	if err := cmd.Run(); err != nil {
		return false
	}
	// The probe child must be fully gone or rmdir would have raced; cmd.Run
	// waited, and exit removes it from cgroup.procs synchronously.
	return true
}

// DiscoverCgroupRoot finds the cgroup dir where per-run jail children can be
// created, armed and cloned into. Walks our own cgroup and its ancestors:
// the shapes this must cover are a Delegate=yes systemd unit (empty, chowned
// to us — either the delegation root itself on older systemd, or its
// DelegateSubgroup/payload child on newer), a delegated developer login
// session, and a root-run service on a bare cgroup v2 box. A login session
// fails the CLONE side of the probe even though user@.service accepts
// mkdir — correct, because runs there would die at fork time; startup
// refusing is the honest answer.
//
// As a last resort, if our own cgroup is delegated to us but holds our
// process (older systemd without DelegateSubgroup), we move OURSELF into a
// gk-payload child — the same maneuver systemd 254+ performs — and retry:
// only an empty inner node may carry controllers for its children.
func DiscoverCgroupRoot() string {
	own := ownCgroup()
	if own != "" {
		dir := filepath.Join("/sys/fs/cgroup", own)
		for {
			if probeUsable(dir) {
				sweepStaleJails(dir)
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if probeUsable("/sys/fs/cgroup") { // root-run on a bare v2 box
		sweepStaleJails("/sys/fs/cgroup")
		return "/sys/fs/cgroup"
	}
	if own != "" { // self-placement fallback (systemd < 254 shapes)
		dir := filepath.Join("/sys/fs/cgroup", own)
		if relocateSelfInto(dir) && probeUsable(dir) {
			sweepStaleJails(dir)
			return dir
		}
	}
	return ""
}

// relocateSelfInto creates root/gk-payload and moves our whole thread group
// there, emptying the delegation root so it can carry controllers.
func relocateSelfInto(root string) bool {
	sub := filepath.Join(root, selfSubgroup)
	if err := os.Mkdir(sub, 0o700); err != nil && !os.IsExist(err) {
		return false
	}
	return os.WriteFile(filepath.Join(sub, "cgroup.procs"),
		[]byte(strconv.Itoa(os.Getpid())), 0o600) == nil
}

// sweepStaleJails removes empty gk-jail-* dirs from a previous mullsmith
// lifetime (crash between the last run and rmdir). Busy ones stay — they
// still meter whatever survived, and Run never reuses names.
func sweepStaleJails(root string) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), JailDirPrefix) {
			_ = syscall.Rmdir(filepath.Join(root, e.Name()))
		}
	}
}

// RunCgroup is one run's meter. Created before the launcher starts; removed
// by Finish(), which also sweeps anything killpg could have missed.
type RunCgroup struct {
	dir string
}

// StartRunCgroup creates and arms the per-run cgroup, and hands back an open
// (CLOEXEC) fd for SysProcAttr.CgroupFD: clone3 consumes it at clone time,
// and execve then closes it, so the jail itself never sees the fd.
func StartRunCgroup(root, id string, memBytes uint64) (*RunCgroup, *os.File, error) {
	dir := filepath.Join(root, JailDirPrefix+id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, nil, err
	}
	rc := &RunCgroup{dir: dir}
	for _, w := range []struct{ file, val string }{
		{"memory.max", strconv.FormatUint(memBytes, 10)},
		{"memory.swap.max", "0"}, // spill-free jails; file absent when no swap exists
		{"pids.max", JailPidsMax},
		{"cpu.max", JailCPUQuota},
	} {
		err := os.WriteFile(filepath.Join(dir, w.file), []byte(w.val), 0o600)
		if err != nil && (w.file == "memory.swap.max" && os.IsNotExist(err)) {
			continue
		}
		if err != nil {
			rc.remove()
			return nil, nil, fmt.Errorf("write %s: %w", w.file, err)
		}
	}
	// #nosec G304 -- dir is a cgroupfs path created by this function.
	f, err := os.Open(dir)
	if err != nil {
		rc.remove()
		return nil, nil, err
	}
	return rc, f, nil
}

// KillAll SIGKILLs everything charged to the cgroup. cgroup.kill (Linux
// 5.14+) is atomic and reaches processes that re-grouped themselves —
// killpg cannot; the per-pid loop is the fallback for 5.7–5.13 kernels.
func (rc *RunCgroup) KillAll() {
	if err := os.WriteFile(filepath.Join(rc.dir, "cgroup.kill"), []byte("1"), 0o200); err == nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(rc.dir, "cgroup.procs"))
	if err != nil {
		return
	}
	for _, pid := range strings.Fields(string(data)) {
		if n, err := strconv.Atoi(pid); err == nil {
			_ = syscall.Kill(n, syscall.SIGKILL)
		}
	}
}

// OOMKilled reports whether the kernel memory-killed anything in this cgroup
// (the memory.peak never lies, but memory.events is the yes/no question).
func (rc *RunCgroup) OOMKilled() bool {
	data, err := os.ReadFile(filepath.Join(rc.dir, "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(rest))
			if n > 0 {
				return true
			}
		}
	}
	return false
}

// Finish sweeps stragglers (bounded) and removes the directory. A cgroup
// that still refuses to die after 300 ms of SIGKILL is a kernel curiosity:
// leave it and let the next run meter on, rather than wedge Run.
func (rc *RunCgroup) Finish() {
	for i := 0; i < 30; i++ {
		procs, err := os.ReadFile(filepath.Join(rc.dir, "cgroup.procs"))
		if err != nil || len(strings.TrimSpace(string(procs))) == 0 {
			break
		}
		rc.KillAll()
		time.Sleep(10 * time.Millisecond)
	}
	rc.remove()
}

func (rc *RunCgroup) remove() { _ = syscall.Rmdir(rc.dir) }

// ErrNoCgroup is what Run answers when per-run metering is unavailable —
// deliberately loud, so nobody mistakes "sandboxed" for "contained".
var ErrNoCgroup = errors.New(
	"runner: no usable cgroup v2 subtree for per-run resource limits — refusing to run un-metered " +
		"(a systemd unit needs Delegate=yes, ideally with DelegateSubgroup=payload and no ProtectControlGroups; " +
		"see spec §9.9) or set tools.code_run.enabled: false")
