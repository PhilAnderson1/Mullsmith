import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";
register("./resolver.mjs", import.meta.url);
const { createNavigationController } = await import("../assets/app/navigation.js");

function navigationFixture(fetcher, options = {}) {
  globalThis.location = { hash: "" };
  globalThis.document = { cookie: "" };
  globalThis.fetch = fetcher;
  const painted = [];
  const noop = () => {};
  const navigation = createNavigationController({
    view: { renderThread: (data) => painted.push(data) },
    getGeneration: () => null, abandon: noop, renderConversation: noop,
    syncComposerButtons: noop, markActive: noop, focusComposer: noop,
    showAuth: noop, toast: options.toast || noop, handleErr: (e) => { throw e; },
    recoverMissingConversation: options.recoverMissingConversation,
  });
  return { navigation, painted };
}
const ok = (data) => ({ ok: true, status: 200, json: async () => data });
const missing = {
  ok: false,
  status: 404,
  json: async () => ({ error: { code: "not_found", message: "conversation not found" } }),
};

test("navigation owns identity until metadata and history commit together", async () => {
  let releasePage;
  const { navigation, painted } = navigationFixture((url) => {
    if (url.includes("/messages")) return new Promise((resolve) => { releasePage = resolve; });
    return Promise.resolve(ok({ id: "new" }));
  });
  navigation.reset({ id: "old" });
  const opening = navigation.selectConversation("new");
  while (!releasePage) await new Promise((r) => setImmediate(r));
  assert.equal(navigation.currentId, "old");
  assert.equal(navigation.navPendingId, "new");
  releasePage(ok({ items: ["new page"], next_cursor: "" }));
  await opening;
  assert.equal(navigation.currentId, "new");
  assert.equal(navigation.navPendingId, "");
  assert.deepEqual(painted, [{ items: ["new page"], next_cursor: "" }]);
});

test("reset invalidates pending navigation and its late history", async () => {
  let releasePage;
  const { navigation, painted } = navigationFixture((url) => {
    if (url.includes("/messages")) return new Promise((resolve) => { releasePage = resolve; });
    return Promise.resolve(ok({ id: "late" }));
  });
  const opening = navigation.selectConversation("late");
  while (!releasePage) await new Promise((r) => setImmediate(r));
  navigation.reset();
  releasePage(ok({ items: ["stale page"], next_cursor: "" }));
  await opening;
  assert.equal(navigation.currentId, "");
  assert.equal(navigation.navPendingId, "");
  assert.deepEqual(painted, []);
});

test("missing conversation refreshes the list and opens its first result", async () => {
  const toasts = [];
  let refreshes = 0;
  const { navigation, painted } = navigationFixture((url) => {
    if (url.endsWith("/gone")) return Promise.resolve(missing);
    if (url.includes("/first/messages")) {
      return Promise.resolve(ok({ items: ["replacement page"], next_cursor: "" }));
    }
    return Promise.resolve(ok({ id: "first" }));
  }, {
    toast: (message) => toasts.push(message),
    recoverMissingConversation: async () => {
      refreshes++;
      return "first";
    },
  });

  navigation.reset({ id: "old" });
  await navigation.selectConversation("gone");

  assert.equal(refreshes, 1);
  assert.equal(navigation.currentId, "first");
  assert.equal(location.hash, "#/c/first");
  assert.deepEqual(painted, [{ items: ["replacement page"], next_cursor: "" }]);
  assert.deepEqual(toasts, ["That conversation no longer exists."]);
});
