// The shared projection contract (C7): a live tool_end event and the
// stored tool_calls row that the same call leaves behind must render the
// SAME card, and a round's status note must read the same live and after
// reload. The Go tests prove the server writes the identical payload to
// both; this proves the client renders one projection of it — node, stub
// DOM, no network. Run: make test-js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

register("./resolver.mjs", import.meta.url);

// --- a minimal real element (querySelector by class, append, classList) ----
class El {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this._cls = new Set();
    this.hidden = false;
    this._text = "";
  }
  get classList() {
    return {
      add: (...cs) => cs.forEach((c) => this._cls.add(c)),
      remove: (...cs) => cs.forEach((c) => this._cls.delete(c)),
      contains: (c) => this._cls.has(c),
    };
  }
  set className(v) {
    this._cls = new Set(String(v).split(/\s+/).filter(Boolean));
  }
  get className() {
    return [...this._cls].join(" ");
  }
  append(...els) {
    this.children.push(...els);
    for (const e of els) e._parent = this;
  }
  // before(el) inserts el into THIS element's parent, immediately ahead
  // of this — the real DOM contract setToolSummary/toolCardsHost use.
  before(el) {
    const p = this._parent;
    if (!p) return;
    const i = p.children.indexOf(this);
    p.children.splice(i < 0 ? p.children.length : i, 0, el);
    el._parent = p;
  }
  remove() {
    /* noop */
  }
  querySelector(sel) {
    const cls = sel.startsWith(".") ? sel.slice(1) : null;
    for (const c of this.children) {
      if (cls && c._cls && c._cls.has(cls)) return c;
      const r = c.querySelector ? c.querySelector(sel) : null;
      if (r) return r;
    }
    return null;
  }
  get textContent() {
    return this._text;
  }
  set textContent(v) {
    this._text = String(v);
  }
}
globalThis.document = { createElement: (tag) => new El(tag) };
globalThis.location = { origin: "https://app.example" };

const {
  callFromEvent, callFromStored, buildCallCard, paintCallOutcome, toolCardNode,
  sharedFileFor, roundStatusNote,
} = await import("../assets/app/thread.js");

// digest collapses a rendered card to everything a viewer can read:
// the state label, the summary line and the result links.
function digest(card) {
  const host = card.querySelector(".tool-results");
  return {
    state: card.querySelector(".tool-state").textContent,
    summary: card.querySelector(".tool-summary")?.textContent ?? "",
    links: host ? host.children.filter((c) => c._cls.has("tool-link")).map((a) => a.href) : [],
  };
}

const results = [
  { title: "Kyoto guide", url: "https://example.org/kyoto" },
  { title: "Trains", url: "https://example.org/trains" },
];

// The server guarantees this pair: ClampCard's output goes into the
// tool_end event AND tool_calls.display unchanged.
const event = {
  call_id: "call_1", tool: "web_search", status: "ok",
  summary: "Searched — 2 result(s)", data: results, truncated: false,
};
const storedRow = {
  id: "call_1", tool: "web_search", arguments: '{"query":"kyoto"}',
  status: "ok",
  result: "…middle-elided model-facing text…", // must NOT be what renders
  display: JSON.stringify({ summary: event.summary, data: event.data }),
};

test("a live event and its stored row render the identical card", () => {
  const live = toolCardNode(event.call_id, event.tool);
  paintCallOutcome(live, callFromEvent(event));
  const reload = buildCallCard(callFromStored(storedRow), storedRow.arguments);
  assert.deepEqual(digest(reload), digest(live));
  assert.equal(digest(live).links.length, 2);
});

test("a denial's stored verdict reads like the live denial card", () => {
  const denyEvent = {
    call_id: "c2", tool: "code_run", status: "denied",
    summary: "user denied this action", data: null,
  };
  const live = toolCardNode("c2", "code_run");
  paintCallOutcome(live, callFromEvent(denyEvent));
  const stored = buildCallCard(
    callFromStored({
      id: "c2", tool: "code_run", status: "denied", arguments: "{}",
      result: "", display: JSON.stringify({ summary: "user denied this action", data: null }),
    }),
    "{}",
  );
  assert.deepEqual(digest(stored), digest(live));
  assert.equal(digest(stored).state, "denied");
});

test("an over-budget display drops data and keeps the summary — and still parses", () => {
  const view = callFromStored({
    id: "c3", tool: "web_search", status: "ok", arguments: "{}",
    display: JSON.stringify({ summary: "Searched — 4000 result(s)", data: null, truncated: true }),
  });
  assert.equal(view.truncated, true);
  const card = buildCallCard(view, "{}");
  assert.equal(digest(card).summary, "Searched — 4000 result(s) · Some tool details were omitted.");
  assert.equal(digest(card).links.length, 0);
});

test("files_share resolves its display download payload", () => {
  const file = { name: "r.csv", bytes: 10, url: "/api/v1/files/download?path=artifacts/r.csv" };
  assert.deepEqual(sharedFileFor(callFromEvent({ tool: "files_share", status: "ok", data: { file } })), file);
  assert.deepEqual(sharedFileFor(callFromStored({
    tool: "files_share", status: "ok", display: JSON.stringify({ data: { file } }),
  })), file);
  assert.equal(sharedFileFor({ tool: "files_share", status: "denied", data: { file } }), null);
});

// --- round notes: one function for both views -------------------------------------

test("server-authored notes win, live and on reload", () => {
  const note = "This conversation reached its context window during tool rounds — start a new chat.";
  assert.equal(roundStatusNote("complete", note), note);
  assert.equal(roundStatusNote("complete", note), roundStatusNote("complete", note, "ignored"));
});

test("generic fallbacks match across the two views", () => {
  assert.equal(roundStatusNote("aborted", ""), "Stopped before finishing.");
  assert.equal(roundStatusNote("error", ""), "The model endpoint failed for this reply.");
  assert.equal(roundStatusNote("error", "", "endpoint exploded"), "endpoint exploded");
  assert.equal(roundStatusNote("complete", ""), "");
  assert.equal(roundStatusNote("streaming", ""), "status: streaming");
});
