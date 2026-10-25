package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Docker implements mode "container" (§9.9 layer 4): one throwaway
// container per run, network off unless configured, the user's work dir
// bind-mounted read-write, everything else read-only. The interpreter must
// exist inside the image under the basename of the allowlisted binary.
type Docker struct {
	opts        Options
	gate        *gate
	image       string
	networkNone bool
	dockerBin   string
}

// ProbeDocker is the startup check (§9.9: "Detected at startup; config
// error if docker unavailable").
func ProbeDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "version").Run(); err != nil {
		return fmt.Errorf("docker version failed: %w", err)
	}
	return nil
}

func NewDocker(opts Options, image string, networkNone bool) (*Docker, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("docker binary not found: %w", err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxOutput <= 0 {
		opts.MaxOutput = 32 << 10
	}
	return &Docker{opts: opts, gate: newGate(opts.GlobalCap), image: image,
		networkNone: networkNone, dockerBin: bin}, nil
}

func (d *Docker) Mode() string { return "container" }

func (d *Docker) Run(ctx context.Context, userID string, req Request) (Outcome, error) {
	release, err := d.gate.acquire(ctx, userID)
	if err != nil {
		return Outcome{}, err
	}
	defer release()

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = d.opts.Timeout
	}
	maxOut := req.MaxOutput
	if maxOut <= 0 {
		maxOut = d.opts.MaxOutput
	}
	mem := req.MaxMemory
	if mem == 0 {
		mem = d.opts.MaxMemory
	}

	// Container id file so cancellation can reach the container even when
	// only the docker client is our direct child.
	cidDir, err := os.MkdirTemp("", "gk-cid-")
	if err != nil {
		return Outcome{}, err
	}
	defer os.RemoveAll(cidDir)
	cidFile := filepath.Join(cidDir, "id")

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"run", "--rm", "-i",
		"--cidfile", cidFile,
		"--name", "gabkeep-run-" + shortID(userID),
		"--memory", strconv.FormatUint(mem/1024/1024, 10) + "m",
		"--memory-swap", strconv.FormatUint(mem/1024/1024, 10) + "m",
		"--cpus", "1.0",
		"--pids-limit", "128",
		"--read-only",
		"--tmpfs", "/tmp:rw,size=64m,noexec",
		"--security-opt", "no-new-privileges",
		"-v", req.WorkDir + ":/work",
		"-w", "/work",
	}
	if d.networkNone {
		args = append(args, "--network=none") // §9.6/§9.9: exfil off by default
	}
	if d.opts.DropUID >= 0 {
		args = append(args, "--user",
			strconv.Itoa(d.opts.DropUID)+":"+strconv.Itoa(d.opts.DropGID))
	}
	args = append(args, d.image, filepath.Base(req.Binary), "-")

	cmd := exec.CommandContext(runCtx, d.dockerBin, args...)
	cmd.Stdin = strings.NewReader(req.Script)
	cmd.Cancel = func() error {
		// Kill the CONTAINER (the docker client dying does not kill it).
		if cid := readCID(cidFile); cid != "" {
			kctx, kcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer kcancel()
			_ = exec.CommandContext(kctx, d.dockerBin, "kill", cid).Run()
		}
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var outBuf, errBuf cappedBuffer
	outBuf.max = maxOut + 1<<10 // docker's own noise shares the budget
	errBuf.max = maxOut
	outPipe, _ := cmd.StdoutPipe()
	errPipe, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return Outcome{}, err
	}
	var wg sync.WaitGroup
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
	case runCtx.Err() != nil:
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

func readCID(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func shortID(userID string) string {
	if len(userID) > 12 {
		return strings.ToLower(userID[:12])
	}
	return strings.ToLower(userID)
}
