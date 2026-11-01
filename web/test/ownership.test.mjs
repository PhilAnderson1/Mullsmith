// Executable checks for the LATEST-WINS HELPER (finding 8) — pure token
// sequencing in ownership.js, no DOM, no bundler, no dependencies. Scope,
// stated honestly (review2): these cases do NOT execute selectConversation,
// startGeneration or the composer lifecycle, so they cannot catch callers
// that mis-use the helper — finding 3 (an early return skipping begin())
// slipped through exactly that way. The application-level wiring is covered
// separately by lifecycle.test.mjs, which drives the real functions.
// Run: make test-js (or node --test web/test/*.test.mjs).
import { test } from "node:test";
import assert from "node:assert/strict";
import { LatestWins } from "../assets/app/ownership.js";

test("youngest navigation wins regardless of response order (finding 1)", () => {
  const nav = new LatestWins();
  // selectConversation(A) then selectConversation(B): tokens are captured
  // BEFORE the first await, so A holds the older token.
  const tokA = nav.begin();
  const tokB = nav.begin();
  // B's response lands first: it still owns the screen and may commit.
  assert.equal(nav.isCurrent(tokB), true);
  // A lands late. The screen belongs to B: committing A's payload is the
  // confirmed bug — the visible chat differed from the user's choice and
  // sends follow the committed id. The guard must reject it.
  assert.equal(nav.isCurrent(tokA), false);
});

test("begin supersedes every outstanding token", () => {
  const nav = new LatestWins();
  const first = nav.begin();
  const second = nav.begin();
  const third = nav.begin();
  assert.equal(nav.isCurrent(first), false);
  assert.equal(nav.isCurrent(second), false);
  assert.equal(nav.isCurrent(third), true);
});

test("sign-out invalidates outstanding work (bump owns nothing)", () => {
  const nav = new LatestWins();
  const tok = nav.begin(); // in-flight open of a conversation
  nav.bump(); // onLogout / delete-account
  assert.equal(nav.isCurrent(tok), false); // the response lands on no screen
  const nextIdentity = nav.begin();
  assert.equal(nav.isCurrent(nextIdentity), true);
  assert.equal(nav.isCurrent(tok), false);
});

test("a list page JOINS its sequence: the base load never supersedes it, a newer search does", () => {
  const list = new LatestWins();
  list.begin(); // loadConversations()
  const pageTok = list.current(); // loadMoreConversations() extends that load
  assert.equal(list.isCurrent(pageTok), true); // the base load did not supersede its own page
  list.begin(); // …and meanwhile the user typed a new search
  assert.equal(list.isCurrent(pageTok), false); // stale page: never appended
});

test("a fresh list load supersedes the older one", () => {
  const list = new LatestWins();
  const staleSearch = list.begin();
  const freshSearch = list.begin();
  assert.equal(list.isCurrent(staleSearch), false);
  assert.equal(list.isCurrent(freshSearch), true);
});
