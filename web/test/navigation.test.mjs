import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";
register("./resolver.mjs", import.meta.url);
const { createNavigationController } = await import("../assets/app/navigation.js");

function navigationFixture(fetcher) {
  globalThis.location = { hash: "" };
  globalThis.document = { cookie: "" };
  globalThis.fetch = fetcher;
  const painted = [];
  const noop = () => {};
  const navigation = createNavigationController({
    view: { renderThread: (data) => painted.push(data) },
    getGeneration: () => null, abandon: noop, renderConversation: noop,
    syncComposerButtons: noop, markActive: noop, focusComposer: noop,
    showAuth: noop, toast: noop, handleErr: (e) => { throw e; },
  });
  return { navigation, painted };
}
const ok = (data) => ({ ok: true, status: 200, json: async () => data });

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
