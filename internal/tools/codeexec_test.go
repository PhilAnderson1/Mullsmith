package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/runner"
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
}

func (f *fakeRunner) Run(ctx context.Context, userID string, req runner.Request) (runner.Outcome, error) {
	f.calls++
	f.gotUser = userID
	f.gotReq = req
	return f.out, f.err
}

func (f *fakeRunner) Mode() string { return "fake" }

func codeTool(r runner.Runner, quota int64) CodeRun {
	return CodeRun{
		Runner: r,
		Runtimes: []config.Runtime{
			{ID: "python3", Binary: "/usr/bin/python3"},
			{ID: "sh", Binary: "/bin/sh"},
		},
		Timeout:   30 * time.Second,
		MaxOutput: 32 << 10,
		UserQuota: quota,
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
	data, ok := res.Data.(map[string]any)
	if !ok || data["exit_code"] != 0 { // Data carries native types pre-serialization
		t.Errorf("Data = %#v", res.Data)
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
	if res.Summary != "timed out (killed)" {
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
	if _, ok := BuildFor(cfg, discardLogger(), nil, nil, fr).Get("code_run"); ok {
		t.Error("code_run registered while code execution is disabled (§9.9 layer 1)")
	}
	if _, ok := BuildFor(base(), discardLogger(), nil, nil, nil).Get("code_run"); ok {
		t.Error("code_run registered without a runner")
	}
	if _, ok := BuildFor(base(), discardLogger(), nil, nil, fr).Get("code_run"); !ok {
		t.Error("code_run missing when enabled with a runner")
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
