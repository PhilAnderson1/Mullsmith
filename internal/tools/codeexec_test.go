package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/managed"
	"mullsmith/internal/runner"
	"mullsmith/internal/scratch"
)

// fakeRunner records the last request and replays a canned outcome — the
// sandbox itself is tested in internal/runner; here we exercise the tool
// adapter: validation, allowlist resolution, staging, payload shape.
type fakeRunner struct {
	gotReq  runner.Request
	gotUser string
	calls   int
	out     runner.Outcome
	err     error
	// side simulates what a script does to the tree: it runs inside the
	// run window, before the outcome is returned (audit2 finding 3 repro).
	side func(req runner.Request)
}

func (f *fakeRunner) Run(ctx context.Context, userID string, req runner.Request) (runner.Outcome, error) {
	f.calls++
	f.gotUser = userID
	f.gotReq = req
	if f.side != nil {
		f.side(req)
	}
	return f.out, f.err
}

func codeTool(r runner.Runner, quota int64) CodeRun {
	return CodeRun{
		Runner: r,
		Runtimes: []config.Runtime{
			{ID: "python3", Binary: "/usr/bin/python3"},
			{ID: "sh", Binary: "/bin/sh"},
		},
		Timeout:   30 * time.Second,
		MaxOutput: 32 << 10,
		Files:     managed.New(quota, 1000),
	}
}

func TestCodeRunDescriptionMatchesNetworkPolicy(t *testing.T) {
	off := codeTool(&fakeRunner{}, 0)
	if d := off.Description(); !strings.Contains(d, "NO network") {
		t.Errorf("allow_network=false should say NO network, got %q", d)
	}
	on := codeTool(&fakeRunner{}, 0)
	on.AllowNetwork = true
	d := on.Description()
	if strings.Contains(d, "NO network") {
		t.Errorf("allow_network=true must not claim the jail is closed, got %q", d)
	}
	if !strings.Contains(d, "network access ENABLED") {
		t.Errorf("allow_network=true should advertise network access, got %q", d)
	}
}

func TestCodeRunValidatesInputs(t *testing.T) {
	root := toolRoot(t)
	fr := &fakeRunner{}

	cases := []struct {
		name, args, wantErr string
	}{
		{"unknown runtime", `{"runtime":"ruby","script":"x"}`, "not allowlisted"},
		{"empty runtime", `{"runtime":"","script":"x"}`, "not allowlisted"},
		{"empty script", `{"runtime":"sh","script":"   "}`, "script is empty"},
		{"bad json", `{`, "invalid arguments"},
	}
	for _, tc := range cases {
		_, err := run(t, codeTool(fr, 0), root, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}

	huge := make([]byte, maxScriptBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	_, err := run(t, codeTool(fr, 0), root,
		argsJSON(t, map[string]any{"runtime": "sh", "script": string(huge)}))
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("oversized script: err = %v", err)
	}
	if fr.calls != 0 {
		t.Errorf("runner must never see invalid calls, got %d", fr.calls)
	}

	_, err = run(t, codeTool(nil, 0), root, `{"runtime":"sh","script":"true"}`)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("nil runner: err = %v", err)
	}
}

func TestCodeRunResolvesRuntimeFromConfig(t *testing.T) {
	root := toolRoot(t)
	fr := &fakeRunner{out: runner.Outcome{ExitCode: 0, Stdout: "hi\n"}}
	res, err := run(t, codeTool(fr, 0), root,
		`{"runtime":"python3","script":"print('hi')"}`)
	if err != nil {
		t.Fatal(err)
	}
	if fr.gotReq.Binary != "/usr/bin/python3" {
		t.Errorf("binary = %q, want the allowlisted path", fr.gotReq.Binary)
	}
	if fr.gotUser != toolUser {
		t.Errorf("user = %q", fr.gotUser)
	}
	wantDir := filepath.Join(root, toolUser, "work")
	if got := fr.gotReq.WorkDir; got != wantDir {
		t.Errorf("work dir = %q, want %q", got, wantDir)
	}
	if got, want := fr.gotReq.ScratchDir, filepath.Join(root, toolUser); got != want {
		t.Errorf("scratch dir = %q, want the user root %q", got, want)
	}
	if fi, err := os.Stat(wantDir); err != nil || !fi.IsDir() {
		t.Errorf("work dir not provisioned: %v", err)
	}
	if fr.gotReq.Script != "print('hi')" {
		t.Errorf("script = %q", fr.gotReq.Script)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatalf("content not JSON: %v", err)
	}
	if payload["exit_code"].(float64) != 0 || payload["stdout"] != "hi\n" {
		t.Errorf("payload = %v", payload)
	}
	if s, _ := payload["note"].(string); !strings.Contains(s, "untrusted") {
		t.Errorf("missing untrusted-data note: %v", payload["note"])
	}
	if res.Summary != "exit 0" {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.Data != nil {
		t.Errorf("code output should only travel in the model result: %#v", res.Data)
	}
}

func TestCodeRunTimeoutAndTruncationFlags(t *testing.T) {
	root := toolRoot(t)
	fr := &fakeRunner{out: runner.Outcome{ExitCode: -1, TimedOut: true, Truncated: true, Stdout: "lots"}}
	res, err := run(t, codeTool(fr, 0), root, `{"runtime":"sh","script":"sleep 99"}`)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["timed_out"] != true || payload["truncated"] != true {
		t.Errorf("flags missing: %v", payload)
	}
	if res.Summary != "timed out (killed) · output truncated" {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestCodeRunPropagatesBusy(t *testing.T) {
	root := toolRoot(t)
	fr := &fakeRunner{err: runner.ErrBusy}
	_, err := run(t, codeTool(fr, 0), root, `{"runtime":"sh","script":"true"}`)
	if !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("err = %v, want wrapped ErrBusy", err)
	}
}

func TestCodeRunStagesFiles(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "uploads/sales.csv", "name,qty\nwidget,3\n")
	fr := &fakeRunner{out: runner.Outcome{ExitCode: 0}}

	res, err := run(t, codeTool(fr, 0), root,
		`{"runtime":"python3","script":"print(open('sales.csv').read())","files":["uploads/sales.csv"]}`)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(root, toolUser, "work", "sales.csv")
	b, err := os.ReadFile(staged)
	if err != nil || !strings.Contains(string(b), "widget,3") {
		t.Errorf("staged file missing or wrong: %q %v", b, err)
	}
	var payload map[string]any
	json.Unmarshal([]byte(res.Content), &payload)
	sf, _ := payload["staged_files"].([]any)
	if len(sf) != 1 || sf[0] != "sales.csv" {
		t.Errorf("staged_files = %v", payload["staged_files"])
	}
}

func TestCodeRunStagingCannotEscapeSandbox(t *testing.T) {
	root := toolRoot(t)
	fr := &fakeRunner{}
	_, err := run(t, codeTool(fr, 0), root,
		`{"runtime":"sh","script":"true","files":["../../../../etc/passwd"]}`)
	if err == nil {
		t.Fatal("escaping stage path must fail")
	}
	if fr.calls != 0 {
		t.Error("runner ran despite staging failure")
	}
	// A symlink through the sandbox is refused too (§9.7).
	link := filepath.Join(root, toolUser, "uploads", "evil")
	if err := os.Symlink("/etc/passwd", link); err == nil {
		_, err := run(t, codeTool(fr, 0), root,
			`{"runtime":"sh","script":"true","files":["uploads/evil"]}`)
		if err == nil {
			t.Error("symlinked stage target must fail")
		}
	}
}

func TestCodeRunStagingLimit(t *testing.T) {
	root := toolRoot(t)
	files := make([]string, maxStagedFiles+1)
	for i := range files {
		files[i] = "uploads/whatever.csv"
	}
	fr := &fakeRunner{}
	_, err := run(t, codeTool(fr, 0), root,
		argsJSON(t, map[string]any{"runtime": "sh", "script": "true", "files": files}))
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("err = %v, want staging-limit refusal", err)
	}
	if fr.calls != 0 {
		t.Error("runner ran despite staging-limit refusal")
	}
}

func TestCodeRunQuotaRefusal(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "work/filler.bin", strings.Repeat("x", 4096))
	fr := &fakeRunner{}
	_, err := run(t, codeTool(fr, 1), root, `{"runtime":"sh","script":"true"}`) // 1-byte quota
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("err = %v, want quota refusal", err)
	}
	if fr.calls != 0 {
		t.Error("runner ran despite quota refusal")
	}
}

// --- audit2 finding 3: final weigh-in, staging pre-check, shared lock -------

// The audit's repro: a script finishes INSIDE the first watchdog interval
// with the tree past quota. The poller can never see it, so the run-end
// weigh-in must report the overrun truthfully — while keeping the partial
// output on disk (§9.9 arrears policy).
func TestCodeRunShortOverrunReported(t *testing.T) {
	root := toolRoot(t)
	fr := &fakeRunner{
		out: runner.Outcome{ExitCode: 0, Stdout: "done\n"},
		side: func(req runner.Request) {
			if err := os.WriteFile(filepath.Join(req.WorkDir, "overrun.bin"),
				[]byte(strings.Repeat("x", 2048)), 0o600); err != nil {
				t.Error(err)
			}
		},
	}
	tool := codeTool(fr, 1024) // 2,048 written under a 1,024 quota
	tool.WatchInterval = time.Hour
	_, err := run(t, tool, root, `{"runtime":"sh","script":"true"}`)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded during run") {
		t.Errorf("err = %v, want a truthful overrun report", err)
	}
	if _, serr := os.Stat(filepath.Join(root, toolUser, "work", "overrun.bin")); serr != nil {
		t.Errorf("partial output must be kept, not deleted: %v", serr)
	}
}

// Staging doubles bytes in the user tree; the pre-check must refuse BEFORE
// any copy lands, so mullsmith's own copies can never be the overrun.
func TestCodeRunStagingRefusedBeforeCopy(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "uploads/big.bin", strings.Repeat("a", 5000))
	writeDirect(t, root, "work/filler.bin", strings.Repeat("b", 3000))
	fr := &fakeRunner{}
	_, err := run(t, codeTool(fr, 10_000), root,
		`{"runtime":"sh","script":"true","files":["uploads/big.bin"]}`)
	if err == nil || !strings.Contains(err.Error(), "staging") ||
		!strings.Contains(err.Error(), "quota") {
		t.Errorf("err = %v, want a staging quota refusal", err)
	}
	if fr.calls != 0 {
		t.Error("runner ran despite staging refusal")
	}
	if _, serr := os.Stat(filepath.Join(root, toolUser, "work", "big.bin")); serr == nil {
		t.Error("staged copy landed despite refusal")
	}
}

// Uploads, fs_write and staging share one quota lock, so individually-passing
// writes cannot jointly overrun. Concurrent fs_write under -race is the
// cheap stand-in for the three-way mix: final usage must never exceed quota.
func TestSharedQuotaLockHoldsUnderConcurrentWrites(t *testing.T) {
	root := toolRoot(t)
	const quota = 1000
	files := managed.New(quota, 1000)
	content := strings.Repeat("z", 100)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := FsWrite{Files: files}
			args := argsJSON(t, map[string]string{
				"path": fmt.Sprintf("work/f%02d.txt", i), "content": content,
			})
			_, _ = w.Execute(context.Background(), inv(root, args))
		}(i)
	}
	wg.Wait()
	used, err := scratch.DirSize(context.Background(), root, toolUser, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if used > quota {
		t.Errorf("joint writers overran: %d used under a %d-byte quota", used, quota)
	}
}

func TestCodeRunRegistrationGating(t *testing.T) {
	fr := &fakeRunner{}
	base := func() *config.Config {
		cfg := &config.Config{}
		cfg.Scratchpad.CodeExec.Enabled = true
		cfg.Scratchpad.CodeExec.AllowedRuntimes = []config.Runtime{{ID: "sh", Binary: "/bin/sh"}}
		return cfg
	}

	cfg := base()
	cfg.Scratchpad.CodeExec.Enabled = false
	if _, ok := BuildFor(cfg, discardLogger(), nil, fr, nil).Get("code_run"); ok {
		t.Error("code_run registered while code execution is disabled (§9.9 layer 1)")
	}
	if _, ok := BuildFor(base(), discardLogger(), nil, nil, nil).Get("code_run"); ok {
		t.Error("code_run registered without a runner")
	}
	if _, ok := BuildFor(base(), discardLogger(), nil, fr, nil).Get("code_run"); !ok {
		t.Error("code_run missing when enabled with a runner")
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// --- audit finding 5: storage watchdog --------------------------------------

// writerRunner simulates a runaway script: it keeps appending chunks to
// work/ until the watchdog kills the run context.
type writerRunner struct{}

func (writerRunner) Run(ctx context.Context, _ string, req runner.Request) (runner.Outcome, error) {
	f, err := os.Create(filepath.Join(req.WorkDir, "blob"))
	if err != nil {
		return runner.Outcome{}, err
	}
	defer f.Close()
	chunk := make([]byte, 64<<10)
	for {
		select {
		case <-ctx.Done():
			return runner.Outcome{}, ctx.Err()
		default:
		}
		if _, err := f.Write(chunk); err != nil {
			return runner.Outcome{}, err
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCodeRunWatchdogKillsQuotaBreach(t *testing.T) {
	root := toolRoot(t)
	c := codeTool(writerRunner{}, 256<<10) // 256 KiB quota
	c.WatchInterval = 5 * time.Millisecond

	_, err := run(t, c, root, `{"runtime":"sh","script":"fill it"}`)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v, want watchdog quota kill", err)
	}
	// The kill bounds the overrun: one 64 KiB chunk per millisecond with a
	// 5 ms poll means anything past a few MiB is an unfired watchdog.
	info, err := os.Stat(filepath.Join(root, toolUser, "work", "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 8<<20 {
		t.Errorf("runaway wrote %d bytes before the kill — watchdog never fired?", info.Size())
	}
}

// napRunner does nothing until its context dies or d elapses.
type napRunner struct{ d time.Duration }

func (n napRunner) Run(ctx context.Context, _ string, _ runner.Request) (runner.Outcome, error) {
	select {
	case <-time.After(n.d):
		return runner.Outcome{ExitCode: 0}, nil
	case <-ctx.Done():
		return runner.Outcome{}, ctx.Err()
	}
}

// The global free-space floor is a trigger even when this user is nowhere
// near quota — several users' growth must not push the host past the line.
func TestCodeRunWatchdogKillsOnDiskFloor(t *testing.T) {
	root := toolRoot(t)
	c := codeTool(napRunner{d: 5 * time.Second}, 1<<30) // quota far out of reach
	c.WatchInterval = 5 * time.Millisecond
	c.DiskFloor = func(string) bool { return false }

	start := time.Now()
	_, err := run(t, c, root, `{"runtime":"sh","script":"sleep"}`)
	if err == nil || !strings.Contains(err.Error(), "nearly full") {
		t.Fatalf("err = %v, want floor kill", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("kill took %v — floor check is not polling", time.Since(start))
	}
}

// A clean run must pass through untouched, watchdog ticking and all.
func TestCodeRunWatchdogPassesCleanRun(t *testing.T) {
	root := toolRoot(t)
	fr := &fakeRunner{out: runner.Outcome{ExitCode: 0, Stdout: "fine"}}
	c := codeTool(fr, 1<<20)
	c.WatchInterval = 5 * time.Millisecond
	res, err := run(t, c, root, `{"runtime":"sh","script":"echo fine"}`)
	if err != nil {
		t.Fatalf("clean run killed: %v", err)
	}
	if !strings.Contains(res.Content, `"exit_code":0`) {
		t.Errorf("payload = %s", res.Content)
	}
}

// --- audit2 finding 3 (accounting bypass): unverifiable ⇒ fail closed ------

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; unreadable-tree tests need an unprivileged reader")
	}
}

// scanBlinder is the script from the audit: it hides its directory from
// every byte-walk (chmod 000), then runs until the watchdog kills it.
type scanBlinder struct{}

func (scanBlinder) Run(ctx context.Context, _ string, req runner.Request) (runner.Outcome, error) {
	if err := os.Chmod(req.WorkDir, 0o000); err != nil {
		return runner.Outcome{}, err
	}
	<-ctx.Done()
	return runner.Outcome{}, ctx.Err()
}

func TestWatchdogCancelsOnUnverifiableScan(t *testing.T) {
	skipIfRoot(t)
	root := toolRoot(t)
	work := filepath.Join(root, toolUser, "work")
	t.Cleanup(func() { _ = os.Chmod(work, 0o700) })

	c := codeTool(scanBlinder{}, 1<<20)
	c.WatchInterval = 5 * time.Millisecond
	_, err := run(t, c, root, `{"runtime":"sh","script":"hide and wait"}`)
	if err == nil || !strings.Contains(err.Error(), "terminated") ||
		!strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("err = %v, want watchdog cancel for unverifiable usage", err)
	}
}

// A run that hides its output and finishes before any poll must not report
// success: the final weigh-in gets an ERROR, not a false "under quota".
// Bytes stay on disk (no silent deletion), and restoring access restores
// ordinary service.
func TestCodeRunShortRunCannotReportUnverifiableSuccess(t *testing.T) {
	skipIfRoot(t)
	root := toolRoot(t)
	work := filepath.Join(root, toolUser, "work")
	t.Cleanup(func() { _ = os.Chmod(work, 0o700) })

	fr := &fakeRunner{
		out: runner.Outcome{ExitCode: 0, Stdout: "all done\n"},
		side: func(req runner.Request) {
			if err := os.WriteFile(filepath.Join(req.WorkDir, "stash.bin"),
				[]byte(strings.Repeat("s", 100)), 0o600); err != nil {
				t.Error(err)
			}
			if err := os.Chmod(req.WorkDir, 0o000); err != nil {
				t.Error(err)
			}
		},
	}
	tool := codeTool(fr, 1<<20)
	tool.WatchInterval = time.Hour // the poller can never catch this one
	_, err := run(t, tool, root, `{"runtime":"sh","script":"write and hide"}`)
	if err == nil || !strings.Contains(err.Error(), "run finished but storage usage could not be verified") {
		t.Fatalf("err = %v, want final weigh-in refusal", err)
	}
	if err := os.Chmod(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(filepath.Join(work, "stash.bin")); serr != nil {
		t.Errorf("output must be kept, not auto-deleted: %v", serr)
	}
	// Access restored → accurate accounting → runs are authorized again.
	fr2 := &fakeRunner{out: runner.Outcome{ExitCode: 0}}
	if _, err := run(t, codeTool(fr2, 1<<20), root, `{"runtime":"sh","script":"true"}`); err != nil {
		t.Errorf("after restore: %v, want ordinary success", err)
	}
}

// Application-managed writes and new runs are refused BEFORE touching the
// tree or the runner while usage is unverifiable — and permitted again,
// without repair, once read access is restored.
func TestUnreadableTreeRefusesWritesUntilRestored(t *testing.T) {
	skipIfRoot(t)
	root := toolRoot(t)
	work := filepath.Join(root, toolUser, "work")
	t.Cleanup(func() { _ = os.Chmod(work, 0o700) })
	if err := os.Chmod(work, 0o000); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, FsWrite{Files: managed.New(1<<20, 1000)}, root,
		argsJSON(t, map[string]string{"path": "work/note.txt", "content": "hi"})); err == nil ||
		!strings.Contains(err.Error(), "could not be verified") {
		t.Errorf("fs_write over unreadable tree: %v, want refusal", err)
	}
	fr := &fakeRunner{}
	if _, err := run(t, codeTool(fr, 1<<20), root,
		`{"runtime":"sh","script":"true","files":["uploads/whatever.csv"]}`); err == nil ||
		!strings.Contains(err.Error(), "could not be verified") {
		t.Errorf("staged code_run over unreadable tree: %v, want refusal", err)
	}
	if fr.calls != 0 {
		t.Error("runner reached despite unverifiable usage")
	}

	os.Chmod(work, 0o700)
	if _, err := run(t, FsWrite{Files: managed.New(1<<20, 1000)}, root,
		argsJSON(t, map[string]string{"path": "work/note.txt", "content": "hi"})); err != nil {
		t.Errorf("fs_write after restore: %v, want success", err)
	}
}

func TestCodeRunStagingPreservesExistingSource(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "work/input.csv", "valuable,data\n")
	fr := &fakeRunner{}
	if _, err := run(t, codeTool(fr, 100), root, `{"runtime":"sh","script":"true","files":["work/input.csv"]}`); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, toolUser, "work/input.csv"))
	if err != nil || string(got) != "valuable,data\n" {
		t.Fatalf("source changed: %q (%v)", got, err)
	}
	if fr.calls != 1 {
		t.Fatal("runner did not execute")
	}
}
