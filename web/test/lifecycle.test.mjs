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
    textContent: "",
    offsetWidth: 40,
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    addEventListener() {}, removeEventListener() {},
    append() {}, appendChild(c) { return c; }, appendChildren() {}, replaceChildren() {},
    remove() {}, setAttribute() {}, removeAttribute() {}, getAttribute() { return null; },
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
globalThis.document = {
  cookie: "",
  body: makeEl(),
  documentElement: makeEl(),
  querySelector: () => makeEl(),
  querySelectorAll: () => [],
  createElement: () => makeEl(),
  createTextNode: (t) => ({ nodeType: 3, data: t, textContent: t }),
  addEventListener() {}, removeEventListener() {},
};

// --- scripted fetch -----------------------------------------------------------
let fetcher = () => Promise.reject(new Error("no fetcher set for this test"));
globalThis.fetch = (url, opts) => fetcher(url, opts);
const ok = (data) => ({ ok: true, status: 200, json: async () => data });

globalThis.__MULLSMITH_TEST__ = true;
const { state, navOwner, selectConversation, startGeneration } =
  await import("../assets/app/app.js");

// reset gives each test a clean, logged-in-looking session.
function reset(currentId = "") {
  state.currentId = currentId;
  state.currentConv = currentId ? { id: currentId } : null;
  state.inflight = null;
  state.meta = { models: [], default_model: "", show_reasoning: false };
}

const CHAT_OK = (url) =>
  url.includes("/messages") ? ok({ items: [], next_cursor: "" }) : ok({ id: url.split("/").pop() });

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
  assert.equal(state.currentId, "A");
});

test("control: a completed navigation to another conversation does commit", async () => {
  reset("A");
  fetcher = async (url) => CHAT_OK(url);
  await selectConversation("C");
  assert.equal(state.currentId, "C");
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
  const newer = { genId: "newer" }; // a newer generation owns the screen now
  state.inflight = newer;
  gen.reject(new ApiError(500, "boom", "server exploded"));
  await gen.p;
  assert.equal(state.inflight, newer, "abandoned rollback cleared the newer generation");
  assert.equal(gen.rollbackRan(), false, "abandoned rollback restored an obsolete draft");
});

test("control: a pre-start failure while STILL owner rolls back and clears inflight", async () => {
  reset("A");
  const gen = await startDeferred();
  gen.reject(new ApiError(409, "history_limit", "no more messages")); // no timer, no toast
  await gen.p;
  assert.equal(state.inflight, null);
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
  const newer = { genId: "newer" };
  state.inflight = newer; // a newer generation takes over while the 401 is still on the wire
  release401();
  await p;
  assert.equal(state.inflight, newer, "a superseded 401 must not log out a live generation");
});

test("control: a 401 while STILL owner ends the generation", async () => {
  reset("A");
  fetcher = async () => ({
    ok: false, status: 401,
    json: async () => ({ error: { code: "unauthenticated", message: "signed out" } }),
  });
  await startGeneration("/api/v1/conversations/A/messages", { content: "hi" }, null, null);
  assert.equal(state.inflight, null);
});
