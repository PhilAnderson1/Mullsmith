// Executable client-lifecycle coverage (review2 findings 3 and 4). Unlike
// ownership.test.mjs — which proves the LatestWins helper alone and, as
// finding 3 demonstrated, CANNOT catch integration errors in its callers —
// this suite imports the REAL app.js and drives the actual
// selectConversation / startGeneration functions against a stub DOM and a
// scripted fetch. The defects lived in the wiring (an early return that
// skipped begin(), catch paths that mutated state without an ownership
// check), so only the wiring itself gets tested here.
//
// app.js skips boot() when __MULLSMITH_TEST__ is preset and exports the
// functions under test; everything else is the production module.
import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";
import { ApiError } from "../assets/app/api.js";

// app.js keeps the absolute /assets/app/*.js import style (the server's
// cache-busting stamp rewriter requires it, finding 10), so Node needs a
// resolution hook to follow that graph.
register("./resolver.mjs", import.meta.url);

// --- stub DOM ----------------------------------------------------------------
// Universal elements: every property read returns a benign value, every
// method is a no-op. The assertions below look at state, never at this.
function makeEl() {
  const el = {
    tagName: "DIV",
    style: {},
    dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    children: [],
    hidden: false,
    disabled: false,
    value: "",
    // <select> surface: options ARE the children (syncThinkingSelect spreads
    // sel.options; the app only ever appends <option> elements to selects).
    get options() { return this.children; },
    textContent: "",
    offsetWidth: 40,
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    // Listeners are RECORDED, not ignored: the C5 tests drive earlierButton
    // and assert on the effects its click handler commits.
    listeners: {},
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    removeEventListener() {},
    // Children AND parentage are tracked (both append and appendChild):
    // "the page rendered X" and "the optimistic bubble was taken back
    // out" are the same observation — a mutation of #thread's children.
    append(...cs) { for (const c of cs) if (c && typeof c === "object") { c._in = this; this.children.push(c); } },
    appendChild(c) { if (c && typeof c === "object") c._in = this; this.children.push(c); return c; },
    appendChildren(...cs) { for (const c of cs) if (c && typeof c === "object") { c._in = this; this.children.push(c); } },
    // Children are TRACKED so paint is observable: renderThread's replace
    // plus appends are what "the page rendered" means for these tests.
    prepend(c) { if (c && typeof c === "object") c._in = this; this.children.unshift(c); return c; },
    insertBefore(c) { if (c && typeof c === "object") c._in = this; this.children.push(c); return c; },
    replaceChildren(...els) { this.children = els; for (const c of els) if (c && typeof c === "object") c._in = this; },
    replaceWith() {},
    remove() {
      const p = this._in;
      if (p) {
        const i = p.children.indexOf(this);
        if (i >= 0) p.children.splice(i, 1);
      }
    },
    setAttribute() {}, removeAttribute() {}, getAttribute() { return null; },
    focus() {}, blur() {},
    querySelector() { return makeEl(); },
    querySelectorAll() { return []; },
    closest() { return makeEl(); },
    cloneNode() { return makeEl(); },
    getBoundingClientRect() { return { top: 0, left: 0, width: 10, height: 10 }; },
  };
  // <template> support — lazy, or makeEl() would recurse forever.
  Object.defineProperty(el, "content", { get() { return { firstElementChild: makeEl() }; } });
  return el;
}

globalThis.window = {
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
};
globalThis.location = { hash: "" };
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
// #id lookups return the SAME element every time, like a real document —
// that is what lets a test grab "#thread" and watch later renders land in it.
const elsById = new Map();
const elFor = (sel) => {
  let el = elsById.get(sel);
  if (!el) { el = makeEl(); elsById.set(sel, el); }
  return el;
};
globalThis.document = {
  cookie: "",
  body: makeEl(),
  documentElement: makeEl(),
  querySelector: elFor,
  querySelectorAll: () => [],
  createElement: () => makeEl(),
  createDocumentFragment: () => makeEl(), // renderMarkdown builds into one
  createTextNode: (t) => ({ nodeType: 3, data: t, textContent: t }),
  addEventListener() {}, removeEventListener() {},
};

// --- scripted fetch -----------------------------------------------------------
let fetcher = () => Promise.reject(new Error("no fetcher set for this test"));
globalThis.fetch = (url, opts) => fetcher(url, opts);
const ok = (data) => ({ ok: true, status: 200, json: async () => data });

// sseStream is a chat response whose SSE body the test feeds frame by
// frame: push() releases one buffered chunk to the pending read, close()
// ends the stream — so "events arrive after the client moved on" is a
// controlled fact rather than a race.
function sseStream() {
  const queue = [];
  const enc = new TextEncoder();
  let closed = false;
  let waiting = null;
  const release = () => {
    if (!waiting) return;
    const f = waiting;
    waiting = null;
    f(queue.length
      ? { value: enc.encode(queue.shift()), done: false }
      : { value: undefined, done: true });
  };
  return {
    response: {
      ok: true,
      status: 200,
      body: {
        getReader: () => ({
          read() {
            if (queue.length) return Promise.resolve({ value: enc.encode(queue.shift()), done: false });
            if (closed) return Promise.resolve({ value: undefined, done: true });
            return new Promise((resolve) => { waiting = resolve; });
          },
          cancel() {},
        }),
      },
    },
    push(s) { queue.push(s); release(); },
    close() { closed = true; release(); },
  };
}

globalThis.__MULLSMITH_TEST__ = true;
const {
  state, navigation, generation, navOwner, selectConversation, startGeneration,
  sendFromComposer, regenerateFrom,
  onHashChange, onModelChange, earlierButton, newChat, renderConversation,
} = await import("../assets/app/app.js");

// reset gives each test a clean, logged-in-looking session.
function reset(currentId = "") {
  generation.abandonInflight();
  navigation.reset(currentId ? {id:currentId}:null);
  state.pending = [];
  state.meta = { models: [], default_model: "", show_reasoning: false };
  state.filesOpen = false;
  state.settingsOpen = false;
  // A previous test's 401 may have shown the auth screen (showAuth hides
  // #view-chat on the CACHED element); routing must start from the chat.
  elFor("#view-chat").hidden = false;
  elFor("#thread").children = [];
  elFor("#composer-input").value = "";
  elFor("#btn-send").disabled = false;
  location.hash = "";
}

const CHAT_OK = (url) =>
  url.includes("/messages") ? ok({ items: [], next_cursor: "" }) : ok({ id: url.split("/").pop() });

// tick drains the microtask queue AND one macrotask round, so chained
// awaits inside the code under test (fetch → json → next fetch) progress.
const tick = () => new Promise((r) => setImmediate(r));

test("new chat carries the model currently selected in the picker", async () => {
  reset("A");
  elFor("#model-picker").value = "model-two";
  let createBody;
  fetcher = async (url, opts = {}) => {
    if (url.endsWith("/api/v1/conversations") && opts.method === "POST") {
      createBody = JSON.parse(opts.body);
      return ok({ id: "NEW", model: "model-two", title: "New chat" });
    }
    return CHAT_OK(url);
  };
  await newChat();
  assert.equal(createBody.model, "model-two");
  assert.equal(navigation.currentId, "NEW");
});

test("rendering a conversation rebuilds thinking choices for its model", () => {
  reset("A");
  state.meta = {
    default_model: "model-one",
    thinking: { choices_by_model: {
      "model-one": [{ id: "default", label: "Default" }],
      "model-two": [
        { id: "default", label: "Default" },
        { id: "high", label: "High" },
      ],
    } },
  };
  navigation.reset({ id: "A", model: "model-two", thinking: "high" });
  renderConversation();
  const select = elFor("#thinking-select");
  assert.deepEqual(select.options.map((o) => o.value), ["default", "high"]);
  assert.equal(select.value, "high");
  assert.equal(select.disabled, false);
});

// --- finding 3: navigation ownership -----------------------------------------

test("re-selecting the open conversation invalidates a pending navigation", async () => {
  reset("A");
  let settleB; // B's GET stays in flight until the test releases it
  fetcher = (url) =>
    new Promise((resolve) => {
      if (url.includes("/B")) settleB = () => resolve(ok({ id: "B", title: "later" }));
    });
  const pB = selectConversation("B"); // request pending: B does not own the screen yet
  await selectConversation("A");      // user clicks the visible chat again
  assert.equal(navOwner.isCurrent(navOwner.current()), true); // A's intent owns the sequence
  settleB();
  await pB;
  // B's late response must NOT take over: the latest intent is still A.
  assert.equal(navigation.currentId, "A");
});

test("control: a completed navigation to another conversation does commit", async () => {
  reset("A");
  fetcher = async (url) => CHAT_OK(url);
  await selectConversation("C");
  assert.equal(navigation.currentId, "C");
});

// --- finding 4: generation ownership on exception paths -----------------------

async function startDeferred() {
  // startGeneration whose request hangs until the returned reject() fires.
  let rejectWith;
  fetcher = () => new Promise((_, reject) => { rejectWith = (e) => reject(e); });
  let rollbackRan = false;
  const p = startGeneration(
    "/api/v1/conversations/A/messages",
    { content: "hi" },
    () => { rollbackRan = true; },
    null,
  );
  await Promise.resolve(); // let startGeneration reach its await
  return { p, reject: (e) => rejectWith(e), rollbackRan: () => rollbackRan };
}

test("a late pre-start failure from an ABANDONED generation must not roll back the newer one", async () => {
  reset("A");
  const gen = await startDeferred();
  const newerOperation=await startDeferred();
  const newer=generation.current;
  gen.reject(new ApiError(500, "boom", "server exploded"));
  await gen.p;
  assert.equal(generation.current, newer, "abandoned rollback cleared the newer generation");
  assert.equal(gen.rollbackRan(), false, "abandoned rollback restored an obsolete draft");
  newerOperation.reject(new ApiError(409,"history_limit","stopped"));await newerOperation.p;
});

test("control: a pre-start failure while STILL owner rolls back and clears inflight", async () => {
  reset("A");
  const gen = await startDeferred();
  gen.reject(new ApiError(409, "history_limit", "no more messages")); // no timer, no toast
  await gen.p;
  assert.equal(generation.current, null);
  assert.equal(gen.rollbackRan(), true);
});

test("a late 401 from an ABANDONED request must not clear the current generation", async () => {
  reset("A");
  let release401;
  fetcher = () => new Promise((resolve) => {
    release401 = () => resolve({
      ok: false, status: 401,
      json: async () => ({ error: { code: "unauthenticated", message: "signed out" } }),
    });
  });
  const p = startGeneration("/api/v1/conversations/A/messages", { content: "hi" }, null, null);
  await Promise.resolve(); // the request is in flight; its token is live
  const newerOperation=await startDeferred();
  const newer=generation.current;
  release401();
  await p;
  assert.equal(generation.current, newer, "a superseded 401 must not log out a live generation");
  newerOperation.reject(new ApiError(409,"history_limit","stopped"));await newerOperation.p;
});

test("control: a 401 while STILL owner ends the generation", async () => {
  reset("A");
  fetcher = async () => ({
    ok: false, status: 401,
    json: async () => ({ error: { code: "unauthenticated", message: "signed out" } }),
  });
  await startGeneration("/api/v1/conversations/A/messages", { content: "hi" }, null, null);
  assert.equal(generation.current, null);
});

// --- C5: conversation identity commits WITH its rendered messages --------------

// The reproduced defect: navigate A → B, B's metadata lands, its page is
// still in flight, B is selected again. Under the old code the metadata had
// ALREADY committed currentId, so the repeat click advanced the ownership
// token and returned — stranding the one loader. Identity stayed B while
// its thread never rendered. Under the atomic commit the repeat click
// coalesces into the pending navigation, and that loader paints.
test("re-selecting during the message load still paints the thread (C5 repro)", async () => {
  reset("A");
  let settleMsgs;
  fetcher = (url) =>
    url.includes("/messages")
      ? new Promise((resolve) => { settleMsgs = () => resolve(ok({ items: [], next_cursor: "" })); })
      : Promise.resolve(ok({ id: "B", title: "B" }));
  const pB = selectConversation("B");
  await tick(); // metadata has landed; the page is the one thing still in flight
  assert.equal(navigation.navPendingId, "B");
  assert.equal(navigation.currentId, "A"); // metadata alone must not commit identity
  await selectConversation("B"); // repeat intent: must NOT orphan the sole loader
  settleMsgs();
  await pB;
  assert.equal(navigation.currentId, "B");
  assert.equal(navigation.navPendingId, "");
  assert.equal(elFor("#thread").children.length, 1); // the destination actually rendered
});

// Walking away to the blank route must retire an in-flight navigation:
// without the bump its response commits an identity the URL no longer names.
test("leaving to the blank route invalidates a pending navigation (C5)", async () => {
  reset("A");
  location.hash = "#/c/B";
  let settleB;
  fetcher = () => new Promise((resolve) => { settleB = () => resolve(ok({ id: "B", title: "B" })); });
  onHashChange(); // routes into selectConversation("B") — pending
  await tick();
  assert.equal(navigation.navPendingId, "B");
  location.hash = "";
  onHashChange(); // walk away while B is still loading
  settleB();
  await tick();
  assert.equal(navigation.currentId, ""); // B must not commit into the blank route
  assert.equal(navigation.navPendingId, "");
});

// A→B→A restores ID equality, so the old `navigation.currentId !== convId` guard
// admitted a stale settings response over the fresh conversation row. The
// click-time epoch moves on every navigation, return included.
test("a settings PATCH from a previous visit to the same ID does not commit (C5)", async () => {
  reset("A");
  let settlePatch;
  fetcher = (url, opts) => {
    if (opts && opts.method === "PATCH")
      return new Promise((resolve) => { settlePatch = () => resolve(ok({ id: "A", model: "late-m2" })); });
    return CHAT_OK(url);
  };
  const p = onModelChange({ target: { value: "m2" } });
  await tick(); // the PATCH is on the wire
  await selectConversation("B"); // navigate away…
  await selectConversation("A"); // …and back — the ID matches the PATCH target again
  settlePatch(); // the stale response lands now
  await p;
  assert.notEqual(navigation.currentConv && navigation.currentConv.model, "late-m2");
});

// Unqueued, two quick picks race: the second PATCH can overcomplete the
// first, and the older FULLER row landing last silently reverts the newer
// choice. The single queue makes request order AND commit order click order.
test("model picks send and commit in click order, not response order (C5)", async () => {
  reset("A");
  const seen = [];
  let settleFirst;
  fetcher = (url, opts) => {
    if (opts && opts.method === "PATCH") {
      const { model } = JSON.parse(opts.body);
      seen.push(model);
      if (model === "slow")
        return new Promise((resolve) => { settleFirst = () => resolve(ok({ id: "A", model: "slow" })); });
      return Promise.resolve(ok({ id: "A", model: "fast" })); // would OVERTAKE if unqueued
    }
    return CHAT_OK(url);
  };
  const p1 = onModelChange({ target: { value: "slow" } });
  await tick();
  const p2 = onModelChange({ target: { value: "fast" } });
  await tick();
  assert.deepEqual(seen, ["slow"]); // 'fast' waits behind 'slow'
  settleFirst();
  await Promise.all([p1, p2]);
  assert.deepEqual(seen, ["slow", "fast"]); // server writes follow clicks
  assert.equal(navigation.currentConv.model, "fast"); // and the newest pick stands
});

// Earlier-message pages: accepted only while the SAME visit still owns the
// view. The pair below pins both directions — control first, so a stub that
// could never insert cannot make the stale-page test pass vacuously.
const ASSISTANT_MSG = { id: "old1", role: "assistant", content: "echo", status: "complete", created_at: 1 };

function gatedEarlierPage() {
  let settlePage;
  fetcher = (url) =>
    url.includes("cursor=")
      ? new Promise((resolve) => { settlePage = () => resolve(ok({ items: [ASSISTANT_MSG], next_cursor: "" })); })
      : CHAT_OK(url);
  return () => settlePage();
}

test("control: an earlier page from the CURRENT visit inserts", async () => {
  reset("A");
  const settlePage = gatedEarlierPage();
  earlierButton("cur1").listeners.click[0]();
  await tick();
  settlePage();
  await tick();
  await tick();
  assert.equal(elFor("#thread").children.length, 1); // the row landed
});

test("a failed earlier-page request can be retried", async () => {
  reset("A");
  let attempts = 0;
  fetcher = async () => ++attempts === 1
    ? new Response(JSON.stringify({ error: { code: "temporary", message: "try again" } }), { status: 503 })
    : ok({ items: [ASSISTANT_MSG], next_cursor: "" });
  const button = earlierButton("cur1");
  await button.listeners.click[0]();
  assert.equal(button.disabled, false);
  assert.equal(elFor("#thread").children.length, 0);
  await button.listeners.click[0]();
  assert.equal(attempts, 2);
  assert.equal(elFor("#thread").children.length, 1);
});

test("an earlier page from a PREVIOUS visit to the same ID is dropped (C5)", async () => {
  reset("A");
  const settlePage = gatedEarlierPage();
  earlierButton("cur1").listeners.click[0]();
  await tick();
  await selectConversation("B"); // leave — repaints the thread
  await selectConversation("A"); // return — ID equality restored, epoch moved
  settlePage(); // the page from the earlier visit arrives
  await tick();
  await tick();
  // Only A's own empty-state render is there; the stale row was dropped.
  assert.equal(elFor("#thread").children.length, 1);
});

// --- review3 issue 2: navigation owns the screen, generations belong to it -----

test("Send and regenerate are inert while a navigation is pending; the draft survives", async () => {
  reset("A");
  const calls = [];
  let settleB;
  fetcher = (url, opts) => {
    calls.push([(opts && opts.method) || "GET", url]);
    if (url.includes("/conversations/B") && !url.includes("/messages"))
      return new Promise((resolve) => { settleB = () => resolve(ok({ id: "B", title: "B" })); });
    return CHAT_OK(url);
  };
  const pB = selectConversation("B"); // in flight: A is still the committed thread
  await tick();
  assert.equal(navigation.navPendingId, "B");
  assert.equal(elFor("#btn-send").disabled, true, "Send stayed enabled into a pending navigation");

  const input = elFor("#composer-input");
  input.value = "typed while navigating";
  await sendFromComposer();
  assert.equal(input.value, "typed while navigating", "the draft was clobbered by a rejected send");
  await regenerateFrom(document.createElement("div"), "msg-7");
  assert.equal(generation.current, null);
  assert.deepEqual(calls.filter(([m]) => m === "POST"), [], "a send or regenerate raced the pending navigation");

  settleB();
  await pB;
  assert.equal(navigation.currentId, "B");
  assert.equal(elFor("#btn-send").disabled, false, "the committed thread must re-enable Send");
});

test("a generation left behind by a navigation cannot paint into the new thread", async () => {
  reset("A");
  const s = sseStream();
  fetcher = (url, opts) =>
    opts && opts.method === "POST" && url.includes("/conversations/A/messages")
      ? Promise.resolve(s.response)
      : CHAT_OK(url);
  elFor("#composer-input").value = "go";
  const pSend = sendFromComposer();
  await tick();
  s.push('event: start\ndata: {"generation_id":"g1","message_id":"m1"}\n\n');
  await tick();
  assert.equal(generation.current && generation.current.genId, "g1");

  await selectConversation("B"); // abandon + repaint
  assert.equal(generation.current, null, "the navigation must dispose the generation it replaces");
  assert.equal(navigation.currentId, "B");
  const painted = elFor("#thread").children.slice();

  // The abandoned stream keeps talking (this reader ignores the abort —
  // stronger than the browser): none of it may land in B's thread.
  s.push('event: token\ndata: {"t":"late"}\n\n');
  s.push('event: done\ndata: {"status":"complete","rounds":[{"message_id":"m1","status":"complete"}]}\n\n');
  s.close();
  await pSend;
  await tick();
  assert.deepEqual(elFor("#thread").children, painted, "a dead generation painted into the new thread");
});

// --- review3 issue 4: uncertain acceptance reconciles, definite refusals roll back

test("control: a coded refusal pre-start rolls back and reconciles nothing", async () => {
  reset("A");
  const calls = [];
  fetcher = (url, opts) => {
    calls.push([(opts && opts.method) || "GET", url]);
    return { ok: false, status: 409, json: async () => ({ error: { code: "already_generating", message: "one at a time" } }) };
  };
  let rolled = false;
  let uncertain = false;
  await startGeneration(
    "/api/v1/conversations/A/messages", { content: "x" },
    () => { rolled = true; }, null, () => { uncertain = true; },
  );
  assert.equal(rolled, true, "a definite refusal must restore the draft");
  assert.equal(uncertain, false, "a coded envelope is not an uncertain outcome");
  assert.equal(generation.current, null);
  assert.deepEqual(calls, [["POST", "/api/v1/conversations/A/messages"]], "a refusal must not trigger a reload");
});

test("a network death before start reconciles: bubble out, draft back, thread reloaded", async () => {
  reset("A");
  const calls = [];
  fetcher = (url, opts) => {
    calls.push([(opts && opts.method) || "GET", url]);
    if (opts && opts.method === "POST") return Promise.reject(new TypeError("Failed to fetch"));
    return CHAT_OK(url);
  };
  const input = elFor("#composer-input");
  input.value = "half-sent thought";
  await sendFromComposer();
  assert.equal(generation.current, null);
  assert.equal(input.value, "half-sent thought", "the draft was not given back");
  assert.equal(elFor("#thread").children.length, 0, "the optimistic bubble outlived the uncertainty");
  assert.ok(
    calls.some(([m, u]) => m === "GET" && u.includes("/conversations/A/messages")),
    "no reconcile reload was attempted",
  );
});

test("a clean stream end before start is UNCERTAIN too, not a silent rollback", async () => {
  reset("A");
  const calls = [];
  fetcher = (url, opts) => {
    calls.push([(opts && opts.method) || "GET", url]);
    if (opts && opts.method === "POST") {
      const s = sseStream();
      s.close(); // EOF with no frames at all: nothing was announced
      return Promise.resolve(s.response);
    }
    return CHAT_OK(url);
  };
  const input = elFor("#composer-input");
  input.value = "eof before start";
  await sendFromComposer();
  assert.equal(generation.current, null);
  assert.equal(input.value, "eof before start", "EOF was treated as a definite refusal");
  assert.ok(
    calls.some(([m, u]) => m === "GET" && u.includes("/conversations/A/messages")),
    "EOF reconciled nothing",
  );
});

const RECONCILED_MSG = { id: "rec1", role: "user", content: "ARRIVED LATE", status: "complete", created_at: 2 };

test("an uncertain-acceptance reload does not paint across a navigation", async () => {
  reset("A");
  let rejectPost, settleReload;
  fetcher = (url, opts) => {
    if (opts && opts.method === "POST" && url.includes("/conversations/A/"))
      return new Promise((_, rej) => { rejectPost = () => rej(new TypeError("Failed to fetch")); });
    if (url.includes("/conversations/A/messages"))
      return new Promise((resolve) => { settleReload = () => resolve(ok({ items: [RECONCILED_MSG], next_cursor: "" })); });
    return CHAT_OK(url);
  };
  elFor("#composer-input").value = "gone abroad mid-send";
  const pSend = sendFromComposer();
  await tick();
  rejectPost(); // the uncertain outcome lands: the reload goes out, gated
  await pSend;
  await selectConversation("B"); // navigate away while it is still on the wire
  const painted = elFor("#thread").children.length;
  settleReload();
  await tick();
  await tick();
  assert.equal(elFor("#thread").children.length, painted, "the stale reload repainted the B screen");
});

// --- review3 finding 4 (follow-up): the recovery state itself ---------------
// The reproduced Chromium sequence: after an uncertain send, Send became
// available while history was still reloading; a second send completed and
// the OLDER reload then replaced its answer on screen. Two defences: the
// composer stays in recovery until reconciliation finishes, and a history
// snapshot can never paint over newer thread activity.

test("an uncertain send holds recovery: Send stays shut while the reload is on the wire", async () => {
  reset("A");
  const calls = [];
  let rejectPost, settleReload;
  fetcher = (url, opts) => {
    calls.push([(opts && opts.method) || "GET", url]);
    if (opts && opts.method === "POST")
      return new Promise((_, rej) => { rejectPost = () => rej(new TypeError("Failed to fetch")); });
    if (url.includes("/conversations/A/messages"))
      return new Promise((resolve) => { settleReload = () => resolve(ok({ items: [RECONCILED_MSG], next_cursor: "" })); });
    return CHAT_OK(url);
  };
  const input = elFor("#composer-input");
  input.value = "half-sent";
  const pSend = sendFromComposer();
  await tick();
  rejectPost();
  await pSend;
  assert.equal(navigation.reconcileId, "A", "the reconcile did not take ownership of the thread");
  assert.equal(elFor("#btn-send").disabled, true, "Send reopened while the reconcile was still on the wire");

  input.value = "second thought";
  await sendFromComposer(); // must be inert — recovery owns the screen
  await regenerateFrom(document.createElement("div"), "msg-9");
  assert.equal(generation.current, null, "regenerate raced the recovery state");
  assert.equal(input.value, "second thought", "a rejected send clobbered the draft");
  assert.equal(calls.filter(([m]) => m === "POST").length, 1, "a second POST escaped the recovery state");

  settleReload(); // the truth lands
  await tick();
  await tick();
  assert.equal(navigation.reconcileId, "");
  assert.equal(elFor("#btn-send").disabled, false, "a settled reconcile must release Send");
});

const STALE_ROW = { id: "stale1", role: "assistant", content: "OLDER SNAPSHOT", status: "complete", created_at: 1 };

test("a reconcile snapshot cannot paint over newer thread activity", async () => {
  reset("A");
  const s = sseStream();
  let firstPost = true;
  let settleReload;
  fetcher = (url, opts) => {
    if (opts && opts.method === "POST") {
      if (firstPost) { firstPost = false; return Promise.reject(new TypeError("Failed to fetch")); }
      return Promise.resolve(s.response);
    }
    return new Promise((resolve) => { settleReload = () => resolve(ok({ items: [STALE_ROW], next_cursor: "" })); });
  };
  elFor("#composer-input").value = "dies uncertain";
  await sendFromComposer(); // recovery state; the reload GET is on the wire
  assert.equal(navigation.reconcileId, "A");

  // Defence in depth: newer activity arriving despite the composer guard —
  // a generation started straight into A (startGeneration is the shared
  // entry point; whatever path let this through, the snapshot must lose).
  const pSecond = startGeneration("/api/v1/conversations/A/messages", { content: "again" }, () => {}, null, () => {});
  await tick();
  s.push('event: start\ndata: {"generation_id":"g2","message_id":"m2"}\n\n');
  s.push('event: done\ndata: {"status":"complete","rounds":[{"message_id":"m2","status":"complete"}]}\n\n');
  s.close();
  await pSecond;
  const painted = elFor("#thread").children.slice();
  assert.ok(painted.length >= 1, "the newer answer never rendered");

  settleReload(); // the older snapshot lands after it
  await tick();
  await tick();
  assert.deepEqual(elFor("#thread").children, painted,
    "an older history snapshot replaced the newer answer");
});

test("a failed reconcile is announced and releases the composer", async () => {
  reset("A");
  elFor("#toast").textContent = "";
  fetcher = (url, opts) => {
    if (opts && opts.method === "POST") return Promise.reject(new TypeError("Failed to fetch"));
    return { ok: false, status: 500, json: async () => ({ error: { code: "storage_error", message: "disk on fire" } }) };
  };
  elFor("#composer-input").value = "uncertain once";
  await sendFromComposer();
  await tick();
  await tick();
  assert.ok(elFor("#toast").textContent.includes("Could not reload"),
    `the reconcile failure was swallowed: "${elFor("#toast").textContent}"`);
  assert.equal(navigation.reconcileId, "", "a failed reconcile must still end the recovery state");
  assert.equal(elFor("#btn-send").disabled, false, "Send stayed shut after an announced failure");
});
