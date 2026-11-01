package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mullsmith/internal/agent"
	"mullsmith/internal/config"
	"mullsmith/internal/runner"
)

// TestMain lets this package's test binary double as the sandbox launcher:
// the code-exec tests run the real runner, which re-execs os.Executable()
// with __sandbox-exec; without this shim the child would start the test
// suite instead of handing off to bwrap.
func TestMain(m *testing.M) {
	if runner.MaybeSandboxMain(os.Args) {
		return
	}
	os.Exit(m.Run())
}

// newCodeChatServer is production-shaped: server.New builds the registry
// from config, code_run is the real tool backed by a real bubblewrap runner
// (built through the production runner.NewForConfig path) executing
// /bin/sh. Approval policy follows config exactly (§9.9 layer 6).
func newCodeChatServer(t *testing.T, replies [][]string) (*authTest, *Server, *toolLLM, *config.Config) {
	t.Helper()
	prev := approvalTimeout
	approvalTimeout = 5 * time.Second // a stuck test denies fast instead of hanging 10 min
	t.Cleanup(func() { approvalTimeout = prev })

	script := &toolLLM{replies: replies}
	fake := newFakeLLM(t, script.handler)
	cfg := chatCfg(t, fake.URL)
	cfg.Scratchpad.MaxFileSize = 1 << 20
	cfg.Scratchpad.UserQuota = 1 << 20
	cfg.Scratchpad.ArtifactTTL = config.Duration(168 * time.Hour)
	cfg.Tools.Agent.MaxToolRounds = 6
	cfg.Tools.Agent.Approval = map[string]string{
		"fs_list": "auto", "fs_read": "auto", "fs_write": "auto",
		"fs_delete": "auto", "files_share": "auto",
		"code_run": "confirm",
	}
	cfg.Scratchpad.CodeExec = config.CodeExecution{
		Enabled:         true,
		AllowedRuntimes: []config.Runtime{{ID: "sh", Binary: "/bin/sh"}},
		Timeout:         config.Duration(10 * time.Second),
		MaxOutput:       config.Size(32 << 10),
		MaxMemory:       config.Size(256 << 20),
	}

	codeRunner, err := runner.NewForConfig(cfg)
	if errors.Is(err, runner.ErrNoCgroup) {
		// Metered jails cannot be built in an undelegated session — the
		// product itself refuses to start here. Run with sudo -E to
		// exercise these tests on a developer box.
		t.Skipf("no per-run cgroup metering available (%v)", err)
	}
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testStore(t)
	s := New(cfg, db, logger, "test", codeRunner, nil)
	ts := httptest.NewServer(s.Handler())

	a := newAuthTestOn(t, ts, db)
	a.invite("gk-code-1")
	if resp := a.register("code@test.io", "uncommon-password-9", false); resp.StatusCode != 201 {
		t.Fatalf("register: %d %s", resp.StatusCode, dumpBody(resp))
	}
	return a, s, script, cfg
}

type decision struct{ approve, remember bool }

// chatRun drives one generation over SSE, answering every awaiting_approval
// from the decisions queue in order (§6.1 step 5). Extra prompts or unused
// decisions fail — that IS the remember/gate assertion surface.
type chatRun struct {
	events    []string
	approvals []map[string]any
	toolEnds  []map[string]any
	done      map[string]any
}

func driveChat(t *testing.T, a *authTest, rd *sseReader, decisions []decision) chatRun {
	t.Helper()
	var cr chatRun
	genID := ""
	next := 0
	for {
		ev, d := rd.next()
		cr.events = append(cr.events, ev)
		switch ev {
		case "start":
			genID, _ = d["generation_id"].(string)
		case "awaiting_approval":
			if next >= len(decisions) {
				t.Fatalf("unexpected awaiting_approval %v (all %d decisions used); events %v",
					d, len(decisions), cr.events)
			}
			dec := decisions[next]
			next++
			cr.approvals = append(cr.approvals, d)
			if genID == "" {
				t.Fatal("awaiting_approval before start event")
			}
			resp := a.do(http.MethodPost, "/api/v1/generations/"+genID+"/approvals", map[string]any{
				"approval_id": d["approval_id"], "approve": dec.approve, "remember": dec.remember,
			})
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("approval POST = %d, want 204", resp.StatusCode)
			}
		case "tool_end":
			cr.toolEnds = append(cr.toolEnds, d)
		case "done":
			if next != len(decisions) {
				t.Fatalf("%d queued approvals never asked; events %v", len(decisions)-next, cr.events)
			}
			cr.done = d
			return cr
		case "error":
			t.Fatalf("error event mid-stream: %v", d)
		}
	}
}

func startCodeChat(t *testing.T, a *authTest, title, content string) (*sseReader, convResp) {
	t.Helper()
	c := createConv(t, a, map[string]any{"title": title})
	resp := sendChat(t, a, c.ID, content)
	return newSSEReader(t, resp), c
}

func workMarker(cfg *config.Config, a *authTest, name string) string {
	return filepath.Join(cfg.Scratchpad.Root, meID(a), "work", name)
}

func codeRunArgs(script string) string {
	b, _ := json.Marshal(map[string]any{"runtime": "sh", "script": script})
	return string(b)
}

// --- the confirm gate (§9.9 layer 6) ----------------------------------------

func TestChatCodeRunDeniedDoesNotExecute(t *testing.T) {
	a, _, script, cfg := newCodeChatServer(t, [][]string{
		{toolCallChunk(0, "c1", "code_run", codeRunArgs("echo ran > work/gate.txt")), finishChunk("tool_calls")},
		{answerChunk("as you wish"), finishChunk("stop")},
	})
	rd, _ := startCodeChat(t, a, "gate", "run something")
	cr := driveChat(t, a, rd, []decision{{approve: false}})

	if len(cr.approvals) != 1 || cr.approvals[0]["tool"] != "code_run" {
		t.Fatalf("approvals = %v", cr.approvals)
	}
	if cr.approvals[0]["rememberable"] != true {
		t.Errorf("code_run approval must offer rememberable, got %v", cr.approvals[0]["rememberable"])
	}
	if len(cr.toolEnds) != 1 || cr.toolEnds[0]["status"] != "denied" {
		t.Fatalf("tool_end = %v, want one denied", cr.toolEnds)
	}
	if _, err := os.Stat(workMarker(cfg, a, "gate.txt")); !os.IsNotExist(err) {
		t.Errorf("denied run must have no side effect, marker err = %v", err)
	}
	if cr.done["status"] != "complete" {
		t.Errorf("done = %v", cr.done)
	}
	// The model learns about the denial in the next round (§6.1).
	toolMsgs := payloadRole(t, script.roundBody(1), "tool")
	if len(toolMsgs) != 1 || !strings.Contains(toJSON(t, toolMsgs[0]), "denied") {
		t.Errorf("round-2 replay = %+v", toolMsgs)
	}
}

func TestChatCodeRunApprovedExecutes(t *testing.T) {
	a, _, _, cfg := newCodeChatServer(t, [][]string{
		{toolCallChunk(0, "c1", "code_run", codeRunArgs("echo ran > work/gate.txt; echo hi")), finishChunk("tool_calls")},
		{answerChunk("done"), finishChunk("stop")},
	})
	rd, _ := startCodeChat(t, a, "gate", "run it")
	cr := driveChat(t, a, rd, []decision{{approve: true}})

	if len(cr.toolEnds) != 1 || cr.toolEnds[0]["status"] != "ok" {
		t.Fatalf("tool_end = %v, want ok", cr.toolEnds)
	}
	b, err := os.ReadFile(workMarker(cfg, a, "gate.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "ran" {
		t.Errorf("approved run produced marker %q err %v", b, err)
	}
	data, _ := cr.toolEnds[0]["data"].(map[string]any)
	if data == nil || data["exit_code"] != float64(0) || data["stdout"] != "hi\n" {
		t.Errorf("tool_end card data = %v", cr.toolEnds[0]["data"])
	}
}

// --- remember per conversation (§6.1 step 5, §9.9 layer 6) -------------------

func TestChatCodeRunRememberSkipsLaterPrompts(t *testing.T) {
	a, s, _, cfg := newCodeChatServer(t, [][]string{
		{toolCallChunk(0, "c1", "code_run", codeRunArgs("echo one > work/m1.txt")), finishChunk("tool_calls")},
		{toolCallChunk(0, "c2", "code_run", codeRunArgs("echo two > work/m2.txt")), finishChunk("tool_calls")},
		{answerChunk("both ran"), finishChunk("stop")},
		// Second conversation replays from the top: it must re-prompt.
		{toolCallChunk(0, "c3", "code_run", codeRunArgs("echo three > work/m3.txt")), finishChunk("tool_calls")},
		{answerChunk("ran again"), finishChunk("stop")},
	})
	rd, c := startCodeChat(t, a, "remember", "run two scripts")
	// ONE decision for TWO code_runs: the second must not prompt (§9.9).
	cr := driveChat(t, a, rd, []decision{{approve: true, remember: true}})

	if len(cr.approvals) != 1 {
		t.Fatalf("prompts = %d, want 1 with remember", len(cr.approvals))
	}
	if len(cr.toolEnds) != 2 {
		t.Fatalf("tool_ends = %v", cr.toolEnds)
	}
	for _, name := range []string{"m1.txt", "m2.txt"} {
		if _, err := os.Stat(workMarker(cfg, a, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	if !s.remembered.remembered(c.ID, "code_run") {
		t.Error("remembered map not set after remember approval")
	}

	// Consent is per conversation: a fresh chat prompts again.
	rd2, _ := startCodeChat(t, a, "fresh", "run again")
	cr2 := driveChat(t, a, rd2, []decision{{approve: true}}) // no remember
	if len(cr2.approvals) != 1 {
		t.Fatalf("new conversation must re-prompt; approvals = %d", len(cr2.approvals))
	}
	if _, err := os.Stat(workMarker(cfg, a, "m3.txt")); err != nil {
		t.Errorf("approved run in fresh conversation: %v", err)
	}

	// Deleting a conversation forgets its consent (§9.9).
	del := a.do(http.MethodDelete, "/api/v1/conversations/"+c.ID, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("conversation delete = %d", del.StatusCode)
	}
	if s.remembered.remembered(c.ID, "code_run") {
		t.Error("consent must die with the conversation")
	}
}

func TestChatCodeRunRememberFalseReprompts(t *testing.T) {
	a, _, _, _ := newCodeChatServer(t, [][]string{
		{toolCallChunk(0, "c1", "code_run", codeRunArgs("true")), finishChunk("tool_calls")},
		{toolCallChunk(0, "c2", "code_run", codeRunArgs("true")), finishChunk("tool_calls")},
		{answerChunk("ok"), finishChunk("stop")},
	})
	rd, _ := startCodeChat(t, a, "twice", "run two scripts")
	cr := driveChat(t, a, rd, []decision{{approve: true, remember: false}, {approve: true}})

	if len(cr.approvals) != 2 {
		t.Fatalf("prompts = %d, want 2 without remember", len(cr.approvals))
	}
	if len(cr.toolEnds) != 2 {
		t.Errorf("tool_ends = %v", cr.toolEnds)
	}
}

// --- bypass attempts (M6 exit criterion) --------------------------------------

func TestChatApprovalBypassAndTimeoutDeny(t *testing.T) {
	// The queue has NO decisions: a prompt must time out and deny (§6.1).
	a, _, _, cfg := newCodeChatServer(t, [][]string{
		{toolCallChunk(0, "c1", "code_run", codeRunArgs("echo ran > work/gate.txt")), finishChunk("tool_calls")},
		{answerChunk("no run then"), finishChunk("stop")},
	})
	// Shrink after construction so the helper's cleanup still restores the
	// original value (cleanups run LIFO).
	approvalTimeout = 300 * time.Millisecond
	t.Cleanup(func() { approvalTimeout = 5 * time.Second })
	rd, _ := startCodeChat(t, a, "timeout", "run it, say nothing")

	genID := ""
	approvalID := ""
	var cr chatRun
	for {
		ev, d := rd.next()
		cr.events = append(cr.events, ev)
		switch ev {
		case "start":
			genID, _ = d["generation_id"].(string)
		case "awaiting_approval":
			approvalID, _ = d["approval_id"].(string)
			// Attacker moves: wrong approval id on the right generation,
			// and a guess on the right id against a bogus generation.
			bad1 := a.do(http.MethodPost, "/api/v1/generations/"+genID+"/approvals",
				map[string]any{"approval_id": "01NXWRONG", "approve": true})
			bad1.Body.Close()
			if bad1.StatusCode != http.StatusNotFound {
				t.Errorf("wrong approval id = %d, want 404", bad1.StatusCode)
			}
			bad2 := a.do(http.MethodPost, "/api/v1/generations/01H4XN0TS3N3SG3N3R5/approvals",
				map[string]any{"approval_id": approvalID, "approve": true})
			bad2.Body.Close()
			if bad2.StatusCode != http.StatusNotFound {
				t.Errorf("wrong generation id = %d, want 404", bad2.StatusCode)
			}
		case "tool_end":
			cr.toolEnds = append(cr.toolEnds, d)
		case "done":
			cr.done = d
			goto finished
		case "error":
			t.Fatalf("error event: %v", d)
		}
	}
finished:
	if approvalID == "" {
		t.Fatal("never saw awaiting_approval")
	}
	if len(cr.toolEnds) != 1 || cr.toolEnds[0]["status"] != "denied" {
		t.Fatalf("timed-out approval must deny: tool_end = %v", cr.toolEnds)
	}
	if _, err := os.Stat(workMarker(cfg, a, "gate.txt")); !os.IsNotExist(err) {
		t.Error("timed-out run must have no side effect")
	}
	// Late approval after the loop gave up cannot resurrect anything.
	late := a.do(http.MethodPost, "/api/v1/generations/"+genID+"/approvals",
		map[string]any{"approval_id": approvalID, "approve": true})
	late.Body.Close()
	if late.StatusCode != http.StatusNotFound {
		t.Errorf("late approval = %d, want 404", late.StatusCode)
	}
}

// --- prompt surface -----------------------------------------------------------

func TestChatSystemPromptAdvertisesCodeRun(t *testing.T) {
	a, _, script, _ := newCodeChatServer(t, [][]string{
		{answerChunk("hi"), finishChunk("stop")},
	})
	rd, _ := startCodeChat(t, a, "prompt", "hello")
	driveChat(t, a, rd, nil)

	msgs := payloadRole(t, script.roundBody(0), "system")
	if len(msgs) == 0 {
		t.Fatal("no system message")
	}
	sys, _ := msgs[0]["content"].(string)
	if !strings.HasPrefix(sys, agent.ToolGuidance) {
		t.Error("system prompt must keep ToolGuidance as its prefix")
	}
	if !strings.Contains(sys, "code_run") {
		t.Error("code_run guidance missing while the tool is registered")
	}
}
