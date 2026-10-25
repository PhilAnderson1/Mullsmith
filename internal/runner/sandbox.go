package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Sandbox support: Go's child-setup hook (SysProcAttr.WrapProcess) was
// removed in Go 1.26, so isolated mode re-executes this very binary through
// an internal subcommand that applies the §9.9 layers in the child BEFORE
// the interpreter is exec'd: cwd, RLIMIT_*, PR_SET_NO_NEW_PRIVS, then the
// gid/uid drop. Stdin (the script), stdout and stderr pass through
// untouched. This keeps the single-binary promise intact.

const (
	// SandboxArg is the hidden internal subcommand; users are not told
	// about it, and it does real work only when fed a config on fd 3.
	SandboxArg = "__sandbox-exec"

	// sandboxConfigFD is where os/exec.ExtraFiles lands (fd 3 = first
	// entry after stdin/stdout/stderr).
	sandboxConfigFD = 3
)

// sandboxSpec is the parent→launcher contract. All fields additive-safe:
// zero values mean "leave alone".
type sandboxSpec struct {
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	UID    int      `json:"uid"` // -1 keep current
	GID    int      `json:"gid"` // -1 keep current
	As     uint64   `json:"as"`  // RLIMIT_AS
	CPU    uint64   `json:"cpu"` // RLIMIT_CPU seconds
	FSize  uint64   `json:"fsize"`
	Nproc  uint64   `json:"nproc"` // only set when the uid actually drops
	NoFile uint64   `json:"nofile"`
}

// MaybeSandboxMain runs the launcher path when args[1] is SandboxArg;
// returns false for normal invocations. Call it as the FIRST thing in
// main(). It never returns on success (execve); on failure it exits
// non-zero with the reason on stderr (surfaced through the tool result).
func MaybeSandboxMain(args []string) bool {
	if len(args) < 2 || args[1] != SandboxArg {
		return false
	}
	if err := runSandbox(); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		os.Exit(126)
	}
	os.Exit(127) // unreachable
	return true
}

func runSandbox() error {
	f := os.NewFile(sandboxConfigFD, "sandbox-spec")
	if f == nil {
		return errors.New("spec fd missing (fd 3)")
	}
	var s sandboxSpec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return fmt.Errorf("bad spec: %w", err)
	}
	_ = f.Close() // before execve, so the interpreter never sees it
	if s.Binary == "" || len(s.Args) == 0 {
		return errors.New("spec missing binary/args")
	}

	if s.Cwd != "" {
		if err := os.Chdir(s.Cwd); err != nil {
			return fmt.Errorf("chdir: %w", err)
		}
	}
	// RLIMIT_NPROC is missing from the syscall package's Linux constants.
	const rlimitNproc = 6

	// No core dumps from model code, ever (0 is meaningful here, so it is
	// not part of the "0 = leave alone" loop below).
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return fmt.Errorf("setrlimit(core): %w", err)
	}

	limits := []struct {
		res int
		max uint64
	}{
		{syscall.RLIMIT_CPU, s.CPU},     // soft: SIGXCPU; hard: SIGKILL
		{syscall.RLIMIT_AS, s.As},       // memory ceiling (§9.9)
		{syscall.RLIMIT_FSIZE, s.FSize}, // writes capped per file
		{rlimitNproc, s.Nproc},          // only when uid changes
		{syscall.RLIMIT_NOFILE, s.NoFile},
	}
	for _, l := range limits {
		if l.max == 0 {
			continue
		}
		rl := syscall.Rlimit{Cur: l.max, Max: l.max}
		if l.res == syscall.RLIMIT_CPU {
			rl.Max = l.max + 1 // soft: SIGXCPU, hard: SIGKILL one second later
		}
		if err := syscall.Setrlimit(l.res, &rl); err != nil {
			// Lowering our own limits is root/unprivileged-safe, but an
			// existing lower hard limit (inherited) can refuse: refuse to
			// run UNLIMITED instead of ignoring the request.
			return fmt.Errorf("setrlimit(%d, %d): %w", l.res, l.max, err)
		}
	}
	// §9.9: no new privileges, ever.
	if err := noNewPrivileges(); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if s.GID >= 0 || s.UID >= 0 {
		if syscall.Geteuid() != 0 {
			return fmt.Errorf("cannot drop to uid %d: launcher is not root", s.UID)
		}
		if s.GID >= 0 {
			if err := syscall.Setgroups([]int{}); err != nil {
				return fmt.Errorf("setgroups: %w", err)
			}
			if err := syscall.Setgid(s.GID); err != nil {
				return fmt.Errorf("setgid: %w", err)
			}
		}
		if s.UID >= 0 {
			if err := syscall.Setuid(s.UID); err != nil {
				return fmt.Errorf("setuid: %w", err)
			}
		}
	}
	// #nosec G204 -- this IS the sandbox hand-off: s.Binary/s.Args were
	// validated by the parent (allowlist + fixed argv shape "bin -").
	return syscall.Exec(s.Binary, s.Args, s.Env)
}

func noNewPrivileges() error {
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 38 /*PR_SET_NO_NEW_PRIVS*/, 1, 0, 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
