package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"mullsmith/internal/config"
)

const (
	mcpMaxStdioFrameBytes    = 1 << 20
	mcpMaxStderrBytes        = 64 << 10
	mcpProcessExitGrace      = 2 * time.Second
	mcpRestartBackoffInitial = time.Second
	mcpRestartBackoffMax     = 30 * time.Second
)

// restartingMCPClient supervises a stdio session after successful discovery.
// A transport failure retires the dead process; the next call reconnects after
// bounded exponential backoff. Calls are never replayed because a failed call
// may already have caused an external side effect before the connection died.
type restartingMCPClient struct {
	server config.MCPServer
	logger *slog.Logger

	mu         sync.Mutex
	session    *mcpsdk.ClientSession
	close      func() error
	retryAfter time.Time
	backoff    time.Duration
	closed     bool
}

func newRestartingMCPClient(server config.MCPServer, logger *slog.Logger, session *mcpsdk.ClientSession, closeSession func() error) *restartingMCPClient {
	return &restartingMCPClient{server: server, logger: logger, session: session, close: closeSession, backoff: mcpRestartBackoffInitial}
}

func (c *restartingMCPClient) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	session, err := c.getSession(ctx)
	if err != nil {
		return nil, err
	}
	result, err := session.CallTool(ctx, params)
	if err != nil && mcpConnectionFailed(err) {
		c.retire(session)
	}
	return result, err
}

func (c *restartingMCPClient) getSession(ctx context.Context) (*mcpsdk.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("MCP stdio server is closed")
	}
	if c.session != nil {
		return c.session, nil
	}
	if wait := time.Until(c.retryAfter); wait > 0 {
		return nil, fmt.Errorf("MCP stdio server is restarting; retry after %s", wait.Round(time.Second))
	}
	connectCtx, cancel := context.WithTimeout(ctx, mcpStartupTimeout)
	session, _, closeSession, err := connectMCP(connectCtx, c.logger, c.server)
	cancel()
	if err != nil {
		c.retryAfter = time.Now().Add(c.backoff)
		c.backoff = min(c.backoff*2, mcpRestartBackoffMax)
		return nil, err
	}
	c.session = session
	c.close = closeSession
	c.retryAfter = time.Time{}
	c.backoff = mcpRestartBackoffInitial
	c.logger.Info("MCP stdio server restarted", "server", c.server.ID)
	return session, nil
}

func (c *restartingMCPClient) retire(failed *mcpsdk.ClientSession) {
	c.mu.Lock()
	if c.session != failed {
		c.mu.Unlock()
		return
	}
	closeSession := c.close
	c.session = nil
	c.close = nil
	c.retryAfter = time.Now().Add(c.backoff)
	c.backoff = min(c.backoff*2, mcpRestartBackoffMax)
	c.mu.Unlock()
	if closeSession != nil {
		_ = closeSession()
	}
}

func (c *restartingMCPClient) Close() error {
	c.mu.Lock()
	c.closed = true
	closeSession := c.close
	c.session = nil
	c.close = nil
	c.mu.Unlock()
	if closeSession != nil {
		return closeSession()
	}
	return nil
}

func mcpConnectionFailed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, mcpsdk.ErrConnectionClosed) || strings.Contains(err.Error(), "unexpected EOF")
}

// mcpStdioTransport runs one administrator-configured child directly, without
// a shell. The SDK's IO transport supplies bounded newline-delimited framing;
// processCloser adds process-group cleanup that CommandTransport does not.
type mcpStdioTransport struct {
	cmd     *exec.Cmd
	stderr  *boundedBuffer
	mu      sync.Mutex
	process *processCloser
}

func newMCPStdioTransport(server config.MCPServer) (*mcpStdioTransport, error) {
	stateDir := server.StateDir
	if stateDir == "" {
		return nil, fmt.Errorf("MCP stdio state directory is not configured")
	}
	for _, dir := range []string{stateDir, filepath.Join(stateDir, "config"), filepath.Join(stateDir, "cache"), filepath.Join(stateDir, "data"), filepath.Join(stateDir, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create MCP stdio state directory: %w", err)
		}
	}
	cmd := exec.Command(server.Command, server.Args...) // #nosec G204 -- administrator config, absolute path, no shell
	cmd.Dir = stateDir
	cmd.Env = mcpStdioEnvironment(server.Environment, stateDir)
	stderr := &boundedBuffer{max: mcpMaxStderrBytes}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return &mcpStdioTransport{cmd: cmd, stderr: stderr}, nil
}

func (t *mcpStdioTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}
	if err := t.cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		return nil, err
	}
	process := newProcessCloser(t.cmd, stdin)
	t.mu.Lock()
	t.process = process
	t.mu.Unlock()
	return (&mcpsdk.IOTransport{Reader: stdout, Writer: process, MaxLineLength: mcpMaxStdioFrameBytes}).Connect(ctx)
}

func (t *mcpStdioTransport) Close() error {
	t.mu.Lock()
	process := t.process
	t.mu.Unlock()
	if process == nil {
		return nil
	}
	return process.Close()
}

func mcpStdioEnvironment(extra map[string]string, stateDir string) []string {
	env := []string{
		"HOME=" + stateDir,
		"LANG=C.UTF-8",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR=" + filepath.Join(stateDir, "tmp"),
		"XDG_CACHE_HOME=" + filepath.Join(stateDir, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(stateDir, "config"),
		"XDG_DATA_HOME=" + filepath.Join(stateDir, "data"),
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+extra[key])
	}
	return env
}

type boundedBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	max     int
	dropped int64
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(p)
	remaining := w.max - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = w.buf.Write(p[:remaining])
		} else {
			_, _ = w.buf.Write(p)
		}
	}
	if written > remaining {
		if remaining < 0 {
			remaining = 0
		}
		w.dropped += int64(written - remaining)
	}
	return written, nil
}

type processCloser struct {
	stdin io.WriteCloser
	pid   int
	wait  <-chan error
	once  sync.Once
	err   error
}

func newProcessCloser(cmd *exec.Cmd, stdin io.WriteCloser) *processCloser {
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	return &processCloser{stdin: stdin, pid: cmd.Process.Pid, wait: wait}
}

func (p *processCloser) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *processCloser) Close() error {
	p.once.Do(func() {
		stdinErr := p.stdin.Close()
		if errors.Is(stdinErr, syscall.EPIPE) {
			stdinErr = nil
		}
		if err, exited := waitProcess(p.wait, mcpProcessExitGrace); exited {
			_ = signalProcessGroup(p.pid, syscall.SIGTERM)
			p.err = errors.Join(stdinErr, normalizeProcessExit(err))
			return
		}
		_ = signalProcessGroup(p.pid, syscall.SIGTERM)
		if err, exited := waitProcess(p.wait, mcpProcessExitGrace); exited {
			p.err = errors.Join(stdinErr, normalizeProcessExit(err))
			return
		}
		_ = signalProcessGroup(p.pid, syscall.SIGKILL)
		err, _ := waitProcess(p.wait, mcpProcessExitGrace)
		p.err = errors.Join(stdinErr, normalizeProcessExit(err))
	})
	return p.err
}

func waitProcess(wait <-chan error, timeout time.Duration) (error, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-wait:
		return err, true
	case <-timer.C:
		return nil, false
	}
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func normalizeProcessExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			switch status.Signal() {
			case syscall.SIGPIPE, syscall.SIGTERM, syscall.SIGKILL:
				return nil
			}
		}
	}
	return err
}
