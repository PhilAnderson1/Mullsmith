// SSE wire-semantics executed, not pattern-matched (finding 8): the event
// taxonomy a multi-round tool stream produces, the spec §6.2 leniency for
// malformed frames, ping comments, chunk-boundary splits and the trailing
// EOF frame — the behaviours app.js's live cards and finish transitions
// rest on. Run: make test-js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { handleFrame, splitFrames } from "../assets/app/sseparse.js";

const frame = (ev, obj) => `event: ${ev}\ndata: ${JSON.stringify(obj)}`;

function recorder() {
  const seen = [];
  const push = (kind) => (arg) => seen.push([kind, arg]);
  const h = {
    onStart: push("start"),
    onToken: push("token"),
    onReasoning: push("reasoning"),
    onToolStart: push("tool_start"),
    onToolArgs: push("tool_args"),
    onToolEnd: push("tool_end"),
    onAwaitingApproval: push("awaiting_approval"),
    onError: push("error"),
    onTitle: push("title"),
    onDone: push("done"),
  };
  return { h, seen };
}

test("a multi-round tool stream dispatches every event in order", () => {
  const { h, seen } = recorder();
  const transcript = [
    frame("start", { generation_id: "g1", message_id: "m1", conversation_id: "c1" }),
    frame("reasoning", { t: "think " }),
    frame("reasoning", { t: "hard" }),
    frame("token", { t: "Let me check." }),
    frame("tool_start", { call_id: "c1", tool: "web_search" }),
    frame("tool_args", { call_id: "c1", t: '{"q"' }),
    frame("tool_args", { call_id: "c1", t: ':"go"}' }),
    frame("awaiting_approval", { approval_id: "a1", call_id: "c2", tool: "code_run" }),
    frame("tool_end", { call_id: "c1", status: "ok", summary: "3 results" }),
    frame("token", { t: "Done." }),
    frame("title", { conversation_id: "c1", title: "Go facts" }),
    frame("done", { message_id: "m2", status: "complete", usage_tokens: 120 }),
  ];
  for (const f of transcript) assert.equal(handleFrame(f, h), true, f);
  assert.deepEqual(seen.map(([k]) => k), [
    "start", "reasoning", "reasoning", "token",
    "tool_start", "tool_args", "tool_args",
    "awaiting_approval", "tool_end", "token", "title", "done",
  ]);
  // The two arg deltas arrive verbatim: the UI concatenates them itself.
  assert.equal(seen[5][1].t, '{"q"');
  assert.equal(seen[6][1].t, ':"go"}');
});

test("malformed frames are skipped without ending the stream (spec §6.2 leniency)", () => {
  const { h, seen } = recorder();
  assert.equal(handleFrame('event: token\ndata: {oops not json', h), false);
  assert.equal(handleFrame(frame("token", { t: "still here" }), h), true);
  assert.deepEqual(seen, [["token", "still here"]]);
});

test("ping comments dispatch nothing, alone or beside a real event", () => {
  const { h, seen } = recorder();
  assert.equal(handleFrame(": ping", h), false);
  assert.equal(handleFrame(`: ping\n${frame("token", { t: "x" })}`, h), true);
  assert.deepEqual(seen, [["token", "x"]]);
});

test("data without an event is 'message': no handler fires, nothing crashes", () => {
  const { h, seen } = recorder();
  assert.equal(handleFrame('data: {"t":"orphan"}', h), false);
  assert.equal(seen.length, 0);
});

test("frames split across chunk boundaries reassemble; EOF tail flushes", () => {
  const { h, seen } = recorder();
  const f1 = frame("token", { t: "a" });
  const f2 = frame("done", { status: "complete" });
  const cut = 8; // mid-frame, the way TCP chunks arrive
  const first = splitFrames(`${f1}\n\n${f2.slice(0, cut)}`);
  assert.deepEqual(first.frames, [f1]);
  assert.equal(first.rest, f2.slice(0, cut));
  for (const f of first.frames) handleFrame(f, h);
  const second = splitFrames(first.rest + f2.slice(cut) + "\n\n");
  assert.deepEqual(second.frames, [f2]);
  assert.equal(second.rest, "");
  for (const f of second.frames) handleFrame(f, h);
  // An EOF tail with no terminating blank line: splitFrames leaves it in
  // rest and streamChat hands that remainder to handleFrame directly.
  const tail = splitFrames(frame("token", { t: "eof" }));
  assert.equal(tail.frames.length, 0);
  assert.equal(handleFrame(tail.rest, h), true);
  assert.deepEqual(seen, [
    ["token", "a"],
    ["done", { status: "complete" }],
    ["token", "eof"],
  ]);
});

test("CRLF-terminated lines parse", () => {
  const { h, seen } = recorder();
  assert.equal(handleFrame('event: token\r\ndata: {"t":"crlf"}\r', h), true);
  assert.deepEqual(seen, [["token", "crlf"]]);
});

test("terminal error events reach the handler (context_full, storage_error, upstream_error)", () => {
  const { h, seen } = recorder();
  for (const code of ["context_full", "storage_error", "upstream_error"]) {
    assert.equal(handleFrame(frame("error", { code, message: `m:${code}` }), h), true);
  }
  assert.deepEqual(seen.map(([, d]) => d.code), ["context_full", "storage_error", "upstream_error"]);
});
