// Real-browser equivalence suite (C7 Stage 6): Chromium drives the SHIPPED
// HTML/JS against the REAL Go server, with only the model endpoint faked
// (web/test/browser/harness.mjs). This is where the things a Node stub
// cannot see get asserted: CSS-gated regenerate affordances (.last-assistant
// gating), template-cloned bubbles, .tool-cards placement outside .msg-body,
// and real network streaming. The core claim — the DOM after a live
// generation equals the DOM after a reload of the same conversation —
// is checked here on real rendering, complementing the Node proof over the
// recorded transcript (web/test/projection.test.mjs).
//
// Run: make test-browser   (requires `make build` and dev-only Playwright;
// the runtime itself stays dependency-free — spec §10).
import { test, before, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { execSync } from "node:child_process";
import { existsSync, readdirSync, mkdirSync, writeFileSync, utimesSync, rmSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  startFakeLLM, startApp, registerAndOpen, waitForConversation, newConversation, threadDigest, sendChat, waitIdle,
  textScript, toolScript, phantomScript,
} from "./harness.mjs";

// Playwright is dev-only: resolve from this folder's node_modules (npm ci)
// or from a global install (the Makefile exports NODE_PATH; createRequire
// honours it), else ask npm directly.
const require = createRequire(import.meta.url);
function loadPlaywright() {
  const attempts = ["playwright"];
  try { attempts.push(join(execSync("npm root -g", { encoding: "utf8" }).trim(), "playwright")); } catch { /* no npm */ }
  let last;
  for (const a of attempts) { try { return require(a); } catch (e) { last = e; } }
  throw new Error(`Playwright not resolvable (tried: ${attempts.join(", ")}): ${last.message}`);
}

// launchChromium: default first, then the usual headless-environment fixes,
// then a browser from the shared ms-playwright cache.
async function launchChromium() {
  const opts = [
    { headless: true },
    { headless: true, args: ["--no-sandbox"] },
    { headless: true, args: ["--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage"] },
  ];
  const cache = join(homedir(), ".cache", "ms-playwright");
  try {
    for (const e of readdirSync(cache)) {
      if (!e.startsWith("chromium-") || e.includes("headless")) continue;
      for (const sub of ["chrome-linux64/chrome", "chrome-linux/chrome", "chrome-mac/Chromium.app/Contents/MacOS/Chromium"]) {
        const p = join(cache, e, sub);
        if (existsSync(p)) opts.push({ headless: true, executablePath: p, args: ["--no-sandbox"] });
      }
    }
  } catch { /* no cache */ }
  let last;
  for (const o of opts) { try { return await chromium.launch(o); } catch (e) { last = e; } }
  throw new Error(`cannot launch Chromium: ${last.message}`);
}

const { chromium } = loadPlaywright();

const repoRoot = fileURLToPath(new URL("../../..", import.meta.url));
const binPath = join(repoRoot, "bin", "mullsmith");

let llm, app, browser;
let userSeq = 0;
let sessionState;

before(async () => {
  assert.ok(existsSync(binPath), `binary missing: run \`make build\` first (looked in ${binPath})`);
  llm = await startFakeLLM();
  // web_fetch needs confirm so the denial scenario has a prompt to deny;
  // everything the other scenarios call is auto.
  app = await startApp(llm.url, { repoRoot, binPath, approval: { web_fetch: "confirm" } });
  browser = await launchChromium();
});

after(async () => {
  await browser?.close();
  await app?.stop();
  await llm?.close();
});

// The shared fake queue must never leak across scenarios: one stuck test
// would otherwise misalign every script that follows.
test.test.afterEach(() => { llm.state.queue.length = 0; });

// Isolate each scenario in a fresh browser context and conversation, while
// reusing the authenticated account to respect the real registration limit.
async function newSession(contextOptions = {}) {
  const ctx = await browser.newContext({ ...(sessionState ? { storageState: sessionState } : {}), ...contextOptions });
  const page = await ctx.newPage();
  if (!sessionState) {
    await registerAndOpen(page, app.base, `browser${++userSeq}@example.io`);
    sessionState = await ctx.storageState();
  } else {
    await page.goto(`${app.base}/`, { waitUntil: "domcontentloaded" });
    await waitForConversation(page);
  }
  if (await page.locator("#btn-menu").isVisible()) await page.locator("#btn-menu").click();
  await newConversation(page);
  return { ctx, page };
}
const digest = (page) => page.evaluate(threadDigest);
const assistants = (d) => d.filter((m) => m.role === "assistant");
// The equivalence projection: everything about the assistant bubbles that
// both views must agree on. User bubbles are excluded only because the
// reload stamps real created_at times (the live view uses the local clock).
const comparable = (d) => assistants(d).map((m) => ({
  id: m.id, text: m.text, bodyVisible: m.bodyVisible, note: m.note, lastAssistant: m.lastAssistant,
  regenVisible: m.regenVisible, cards: m.cards, artifacts: m.artifacts,
}));

async function reloadAndWait(page) {
  // Same-view reload (bfcache off): the page must come back exactly as the
  // live stream left it — every bubble's identity included, not a recount.
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForFunction(() =>
    !document.querySelector(".msg-body.streaming") &&
    document.querySelectorAll("#thread .msg").length > 0, null, { timeout: 15_000 });
}

test("a full deep-link load waits for the requested older conversation", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    const older = await page.evaluate(() => location.hash.slice(4));
    await newConversation(page);
    await page.route(`**/api/v1/conversations/${older}`, async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 500));
      await route.continue();
    });
    await page.goto("about:blank");
    await page.goto(`${app.base}/#/c/${older}`, { waitUntil: "domcontentloaded" });
    await waitForConversation(page, older);
    assert.equal(await page.evaluate(() => location.hash), `#/c/${older}`);
  } finally { await ctx.close(); }
});

test("files load more preserves tabs and supports retries after a failed page", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    const me = await (await page.request.get(`${app.base}/api/v1/auth/me`)).json();
    const work = join(app.dir, "scratch", me.id, "work");
    mkdirSync(work, { recursive: true });
    for (let i = 0; i < 55; i++) writeFileSync(join(work, `pagination-${String(i).padStart(2, "0")}.txt`), "notes");
    const newest = join(work, "z-pagination-newest.txt");
    writeFileSync(newest, "newest");
    const stamp = new Date(Date.now() + 60_000);
    utimesSync(newest, stamp, stamp);
    assert.equal(await page.locator("#topbar-title").isVisible(), true);
    await page.click("#btn-files");
    await page.waitForSelector("#files-panel:not([hidden])");
    assert.equal(await page.locator("#topbar-title").isVisible(), false,
      "the hidden conversation must not label the Files view");
    await page.click('.files-filter[data-kind="work"]');
    await page.waitForFunction(() => document.querySelectorAll("#files-list .file-row").length === 50 && !document.querySelector("#btn-files-more").hidden);
    assert.equal(await page.locator("#files-list .f-name").first().textContent(), "z-pagination-newest.txt");
    let failed = false;
    await page.route("**/api/v1/files?*", async (route) => {
      if (new URL(route.request().url()).searchParams.has("cursor") && !failed) {
        failed = true;
        await route.fulfill({ status: 503, json: { error: { code: "temporary", message: "temporary file page failure" } } });
      } else await route.continue();
    });
    await page.click("#btn-files-more");
    await page.waitForFunction(() => document.body.textContent.includes("temporary file page failure"));
    await page.click("#btn-files-more");
    await page.waitForFunction(() => document.querySelector("#btn-files-more").hidden);
    const names = await page.locator("#files-list .f-name").allTextContents();
    assert.equal(names.filter((name) => name.startsWith("pagination-")).length, 55);
    assert.equal(new Set(names).size, names.length);
    await page.click('.files-filter[data-kind="uploads"]');
    await page.waitForFunction(() => !document.querySelector("#btn-files-more").disabled);
    assert.equal(await page.locator("#files-list .f-name").filter({ hasText: "pagination-" }).count(), 0);
    await page.click('.files-filter[data-kind="work"]');
    await page.waitForFunction(() => document.querySelectorAll("#files-list .file-row").length === 50 && !document.querySelector("#btn-files-more").hidden);
    await page.click("#btn-files-close");
    await page.waitForSelector("#files-panel[hidden]", { state: "attached" });
    assert.equal(await page.locator("#topbar-title").isVisible(), true,
      "closing Files should restore the conversation title");
  } finally { await ctx.close(); }
});

test("file purge confirms and stays scoped to the active tab", async () => {
  const { ctx, page } = await newSession();
  try {
    const me = await (await page.request.get(`${app.base}/api/v1/auth/me`)).json();
    const work = join(app.dir, "scratch", me.id, "work", "purge-old.txt");
    const upload = join(app.dir, "scratch", me.id, "uploads", "keep-old.txt");
    writeFileSync(work, "old work");
    writeFileSync(upload, "old upload");
    const old = new Date(Date.now() - 2 * 24 * 60 * 60 * 1000);
    utimesSync(work, old, old);
    utimesSync(upload, old, old);

    await page.click("#btn-files");
    await page.click('.files-filter[data-kind="work"]');
    await page.selectOption("#files-purge-select", "1d");
    await page.waitForSelector("#confirm-dialog[open]");
    assert.match(await page.locator("#confirm-text").textContent(), /Delete files in Work older than/);
    await page.click("#confirm-ok");
    const deadline = Date.now() + 5000;
    while (existsSync(work) && Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    assert.equal(existsSync(work), false);
    assert.equal(existsSync(upload), true, "a Work purge touched Uploads");
  } finally { await ctx.close(); }
});

test("account deletion cannot proceed without confirmation", async () => {
  const { ctx, page } = await newSession();
  try {
    await page.click("#btn-settings");
    await page.waitForSelector("#settings-panel:not([hidden])");
    assert.equal(await page.locator("#topbar-title").isVisible(), false,
      "the hidden conversation must not label the Settings view");
    await page.click("#btn-delete-account");
    await page.waitForSelector("#confirm-dialog[open]");
    assert.match(await page.locator("#confirm-text").textContent(), /Delete your account permanently/);
    await page.click('#confirm-dialog button[value="cancel"]');
    assert.equal((await page.request.get(`${app.base}/api/v1/auth/me`)).status(), 200);
    assert.equal(await page.locator("#settings-panel").isVisible(), true);
    await page.click("#btn-settings-close");
    await page.waitForSelector("#settings-panel[hidden]", { state: "attached" });
    assert.equal(await page.locator("#topbar-title").isVisible(), true,
      "closing Settings should restore the conversation title");
  } finally { await ctx.close(); }
});

test("tool turn with a late call ID: live DOM equals reloaded DOM", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    llm.queueScripts(
      phantomScript("fs_write", { path: "work/note.md", content: "# note\nhello browser\n" }, "call_w1"),
      textScript("Kyoto in autumn."),
    );
    await sendChat(page, "write a note, then answer");
    await waitIdle(page);
    const live = await digest(page);

    const asst = assistants(live);
    assert.equal(asst.length, 2, "a tool round and an answer round paint TWO bubbles");
    assert.equal(asst[0].text, "", "the tool-only round has no prose of its own");
    assert.equal(asst[0].bodyVisible, false, "a tool-only round must not paint an empty answer bubble");
    assert.equal(asst[0].cards.length, 1);
    assert.equal(asst[0].cards[0].tool, "fs_write");
    assert.equal(asst[0].cards[0].state, "done");
    assert.match(asst[0].cards[0].summary, /^Wrote/);
    assert.equal(asst[1].text, "Kyoto in autumn.");
    assert.ok(asst[0].id && asst[1].id && asst[0].id !== asst[1].id,
      "each round is keyed by its own row id");
    // Real CSS: only the newest assistant bubble shows its regenerate button.
    assert.equal(asst[1].lastAssistant, true);
    assert.equal(asst[1].regenVisible, true);
    assert.equal(asst[0].regenVisible, false);

    await reloadAndWait(page);
    const again = await digest(page);
    assert.deepEqual(comparable(again), comparable(live),
      "reload disagrees with the live stream (ids, prose split, cards, statuses, regen gating)");
    assert.deepEqual(again.map((m) => m.role), live.map((m) => m.role),
      "row ORDER survives: user turn, tool round, answer round");
  } finally { await ctx.close(); }
});

test("tool-only and whitespace rounds have no blank bubble while the answer streams or after reload", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  const proseBeforeTools = () => {
    const round = document.querySelectorAll(".from-assistant")[2];
    const body = round.querySelector(".msg-body");
    const cards = round.querySelector(".tool-cards");
    return Boolean(body.compareDocumentPosition(cards) & Node.DOCUMENT_POSITION_FOLLOWING) &&
      body.getBoundingClientRect().bottom <= cards.getBoundingClientRect().top;
  };
  try {
    llm.queueScripts(
      toolScript("empty_write", "fs_write", { path: "work/empty-card.txt", content: "hello" }),
      [
        { delta: { content: " \n\t" } },
        ...toolScript("whitespace_read", "fs_read", { path: "work/empty-card.txt" }),
      ],
      [
        { delta: { content: "The file is ready." } },
        ...toolScript("prose_list", "fs_list", { path: "work" }),
      ],
      textScript("Here is the final answer.", { pieces: 2, delayMs: 800 }),
    );
    await sendChat(page, "check the file and answer");
    await page.waitForFunction(() =>
      document.querySelectorAll("#thread .tool-card").length === 3 &&
      document.querySelectorAll("#thread .from-assistant").length === 4);

    const active = assistants(await digest(page));
    assert.equal(active[0].bodyVisible, false, "completed empty round left a bordered body during the next round");
    assert.equal(active[1].bodyVisible, false, "whitespace is not visible prose");
    assert.equal(active[2].bodyVisible, true, "a tool round's actual prose must stay visible");
    assert.equal(active[3].bodyVisible, true, "the active answer keeps its waiting spinner");
    assert.equal(await page.locator(".from-assistant .tool-card:visible").count(), 3,
      "hiding empty prose must not hide the tool cards");
    assert.equal(await page.evaluate(proseBeforeTools), true,
      "a round's introductory prose must precede its tool cards while streaming");

    await waitIdle(page);
    const live = await digest(page);
    const settled = assistants(live);
    assert.deepEqual(settled.map((m) => m.bodyVisible), [false, false, true, true]);
    assert.equal(settled[2].text, "The file is ready.");
    assert.equal(settled[3].text, "Here is the final answer.");
    assert.equal(await page.evaluate(proseBeforeTools), true,
      "settling a round must preserve prose before its tools");

    await reloadAndWait(page);
    assert.deepEqual(comparable(await digest(page)), comparable(live),
      "the visibility of answer bodies must agree live and after reload");
    assert.equal(await page.evaluate(proseBeforeTools), true,
      "reloaded prose must precede the calls it introduced");
  } finally { await ctx.close(); }
});

test("useful card mode removes the empty assistant shell live and after reload", { timeout: 120_000 }, async () => {
  const quietApp = await startApp(llm.url, { repoRoot, binPath, toolCards: "useful" });
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  try {
    await registerAndOpen(page, quietApp.base, `quiet${++userSeq}@example.io`);
    await page.evaluate(() => {
      window.__quietToolCardsAdded = 0;
      window.__quietToolCardObserver = new MutationObserver((mutations) => {
        for (const mutation of mutations) {
          for (const node of mutation.addedNodes) {
            if (!(node instanceof Element)) continue;
            if (node.matches(".tool-card")) window.__quietToolCardsAdded++;
            window.__quietToolCardsAdded += node.querySelectorAll(".tool-card").length;
          }
        }
      });
      window.__quietToolCardObserver.observe(document.querySelector("#thread"), {
        childList: true,
        subtree: true,
      });
    });
    llm.queueScripts(
      toolScript("quiet_write", "fs_write", { path: "work/quiet.txt", content: "hello" }),
      textScript("Finished."),
    );
    await sendChat(page, "write a small file");
    await waitIdle(page);
    assert.equal(await page.evaluate(() => window.__quietToolCardsAdded), 0,
      "a successful status-only tool card appeared transiently");
    assert.equal(await page.locator(".from-assistant:visible").count(), 1,
      "the completed tool-only round left its role/timestamp shell visible");
    await page.reload({ waitUntil: "domcontentloaded" });
    await waitForConversation(page);
    assert.equal(await page.locator(".from-assistant:visible").count(), 1,
      "reload restored the empty role/timestamp shell");
  } finally {
    await ctx.close();
    await quietApp.stop();
  }
});

test("denied call: 'denied' card with its verdict, live and after reload", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    llm.queueScripts(
      toolScript("call_f1", "web_fetch", { url: "https://example.com" }),
      textScript("I could not fetch that."),
    );
    await sendChat(page, "fetch example.com");
    // web_fetch is confirm-policy: the prompt appears, and the USER denies.
    await page.waitForSelector(".approval-row button");
    await page.click(".approval-row button:not(.primary)"); // "Deny"
    await waitIdle(page);
    const live = await digest(page);

    const asst = assistants(live);
    assert.equal(asst.length, 2);
    assert.equal(asst[0].cards.length, 1);
    assert.equal(asst[0].cards[0].tool, "web_fetch");
    assert.equal(asst[0].cards[0].state, "denied",
      "the policy verdict is visible on the card, not swallowed");
    assert.ok(asst[0].cards[0].summary.length > 0, "the denial carries its reason");

    await reloadAndWait(page);
    assert.deepEqual(comparable(await digest(page)), comparable(live),
      "the denial renders identically after reload");
  } finally { await ctx.close(); }
});

test("useful mode reveals a status-only tool when it requests approval", { timeout: 120_000 }, async () => {
  const quietApp = await startApp(llm.url, {
    repoRoot,
    binPath,
    toolCards: "useful",
    approval: { web_fetch: "confirm" },
  });
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  try {
    await registerAndOpen(page, quietApp.base, `quietapproval${++userSeq}@example.io`);
    llm.queueScripts(
      toolScript("quiet_approval", "web_fetch", { url: "https://example.com" }),
      textScript("The fetch was denied."),
    );
    await sendChat(page, "fetch example.com");
    await page.locator(".approval-row button").first().waitFor({ state: "visible" });
    assert.equal(await page.locator(".from-assistant:visible .tool-card.st-awaiting").count(), 1,
      "the approval card remained inside a hidden assistant shell");
    await page.click(".approval-row button:not(.primary)");
    await waitIdle(page);
  } finally {
    await ctx.close();
    await quietApp.stop();
  }
});

test("ID before name and two calls to the same tool retain distinct coherent cards", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    const script=[];
    for (const index of [0,1]) {
      script.push({delta:{tool_calls:[{index,id:`early_${index}`,function:{arguments:'{"path":'}}]}});
      script.push({delta:{tool_calls:[{index,function:{name:"fs_list",arguments:'"work"}'}}]}});
    }
    script.push({delta:{},finish:"tool_calls"});
    llm.queueScripts(script,textScript("Both listed."));
    await sendChat(page,"list twice with IDs before names");
    await waitIdle(page);
    const live=await digest(page);
    const cards=assistants(live)[0].cards;
    assert.equal(cards.length,2);
    for(const card of cards) {assert.equal(card.state,"done");assert.equal(card.tool,"fs_list");}
    await reloadAndWait(page);
    assert.deepEqual(comparable(await digest(page)),comparable(live));
  } finally {await ctx.close();}
});

test("abort mid-answer: the stopped note is the same string after reload", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    llm.queueScripts(textScript("The quick brown fox jumps over the lazy dog, over and over.",
      { pieces: 12, delayMs: 150 }));
    await sendChat(page, "tell me something long");
    await page.waitForFunction(() => {
      const b = document.querySelector(".msg-body.streaming");
      return b && b.innerText.trim().length > 0;
    });
    await page.click("#btn-stop");
    await waitIdle(page);
    const live = await digest(page);
    const last = assistants(live).at(-1);
    assert.ok(last.text.length > 0, "partial prose survives the abort");
    assert.equal(last.note, "Stopped before the answer finished.",
      "live shows the server's durable note (done.rounds[].note)");

    await reloadAndWait(page);
    const again = assistants(await digest(page)).at(-1);
    assert.equal(again.id, last.id, "the bubble keeps its row id across the reload");
    assert.equal(again.note, last.note, "the terminal reason is durable, not stream-only");
  } finally { await ctx.close(); }
});

test("regenerate retires the whole turn: old rounds and cards are gone", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    llm.queueScripts(textScript("First answer."));
    await sendChat(page, "answer me briefly");
    await waitIdle(page);
    const before = assistants(await digest(page));
    assert.equal(before.length, 1);
    assert.equal(before[0].text, "First answer.");

    const requestsBefore = llm.state.requests.length;
    llm.queueScripts(
      toolScript("call_w3", "fs_write", { path: "work/new.md", content: "fresh\n" }),
      textScript("Regenerated answer."),
    );
    await page.click(".last-assistant .regen-btn");
    await waitIdle(page);
    const after = assistants(await digest(page));
    assert.equal(after.length, 2, "the regenerated turn streams its tool round as its own bubble");
    assert.equal(after[0].cards.length, 1);
    assert.equal(after[0].cards[0].state, "done");
    assert.equal(after[1].text, "Regenerated answer.");
    assert.notEqual(after[1].id, before[0].id, "the new generation writes new rows");

    // The retry prompt is built from the POST-retire history: the retired
    // ANSWER must not speak again (the fake records every request body).
    assert.ok(llm.state.requests.length >= requestsBefore + 2, "the new model calls ran");
    for (const r of llm.state.requests.slice(requestsBefore)) {
      assert.ok(!JSON.stringify(r).includes("First answer."),
        "a retry prompt quotes the retired answer");
    }
    assert.ok(JSON.stringify(llm.state.requests[requestsBefore]).includes("answer me briefly"),
      "the retry prompt lost the user's own turn");

    await reloadAndWait(page); // user turn, tool round, answer round
    const reloaded = await digest(page);
    assert.deepEqual(comparable(reloaded), comparable(after),
      "the regenerated turn renders identically after reload");
  } finally { await ctx.close(); }
});

test("output-limit and filtering explanations survive reload", { timeout: 120_000 }, async () => {
  for (const [finish, note] of [
    ["length", "The model reached its output limit before finishing this reply."],
    ["content_filter", "The model stopped this reply because of its content filter."],
  ]) {
    const { ctx, page } = await newSession();
    try {
      llm.queueScripts(textScript("Partially written answer.", { finish }));
      await sendChat(page, "answer briefly");
      await waitIdle(page);
      const live = await digest(page);
      assert.equal(assistants(live).at(-1).note, note);
      await reloadAndWait(page);
      assert.deepEqual(comparable(await digest(page)), comparable(live));
    } finally { await ctx.close(); }
  }
});

test("markdown styles apply to the prose wrapper live and after reload", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession({ viewport: { width: 360, height: 800 } });
  const styles = () => {
    const prose = document.querySelector(".from-assistant .stream-text.md");
    const css = (selector) => getComputedStyle(prose.querySelector(selector));
    const tableWrap = prose.querySelector(".md-table-wrap");
    return {
      whiteSpace: getComputedStyle(prose).whiteSpace,
      paragraphMargin: parseFloat(css("p").marginBottom),
      quoteBorder: parseFloat(css("blockquote").borderInlineStartWidth),
      codeFont: css("code").fontFamily,
      linkColor: css("a").color,
      accent: getComputedStyle(document.documentElement).getPropertyValue("--accent").trim(),
      table: {
        headings: [...prose.querySelectorAll("th")].map((cell) => cell.textContent),
        rows: prose.querySelectorAll("tbody tr").length,
        firstCellStrong: prose.querySelector("tbody td strong")?.textContent,
        codeWithPipe: prose.querySelector("tbody td code")?.textContent,
        rightAligned: css("tbody td:nth-child(2)").textAlign,
        overflow: getComputedStyle(tableWrap).overflowX,
        scrolls: tableWrap.scrollWidth > tableWrap.clientWidth,
      },
      extras: {
        strike: prose.querySelector("del")?.textContent,
        ruleWidth: parseFloat(css("hr").borderTopWidth),
        nestedItems: prose.querySelectorAll("ul > li > ol > li").length,
        nestedTaskItems: prose.querySelectorAll("ol > li > ul > li.task-list-item").length,
        checkedTasks: prose.querySelectorAll('.task-list-item input[type="checkbox"]:checked').length,
        disabledTasks: prose.querySelectorAll('.task-list-item input[type="checkbox"]:disabled').length,
        orderedStart: prose.querySelector("ul > li > ol")?.start,
        orderedValues: [...prose.querySelectorAll("ul > li > ol > li")].map((li) => li.value),
        continuation: prose.querySelector("ul > li br")?.nextSibling?.textContent,
        deepestHeadings: prose.querySelectorAll("h6").length,
      },
    };
  };
  try {
    llm.queueScripts(textScript("# Heading\n\n##### Deep heading\n\n###### Deepest heading\n\nA paragraph with `code`, [a link](https://example.com), and ~~old text~~.\n\n---\n\n- Parent\n  3. Third nested item\n     - [x] Complete\n     - [ ] Pending\n  5. Fifth nested item\n- Continued item\n  on another line\n\n> A quotation\n\n| Option | Est. cost | Status |\n|:-------|----------:|:------:|\n| **Bravo** | $55k | Blocked |\n| Alpha-with-a-name-too-long-for-a-phone | $61k | `Draft|review` |"));
    await sendChat(page, "format an answer");
    await waitIdle(page);
    const live = await page.evaluate(styles);
    assert.equal(live.whiteSpace, "normal");
    assert.ok(live.paragraphMargin > 0);
    assert.ok(live.quoteBorder > 0);
    assert.match(live.codeFont, /monospace/);
    assert.deepEqual(live.table.headings, ["Option", "Est. cost", "Status"]);
    assert.equal(live.table.rows, 2);
    assert.equal(live.table.firstCellStrong, "Bravo");
    assert.equal(live.table.codeWithPipe, "Draft|review");
    assert.equal(live.table.rightAligned, "right");
    assert.equal(live.table.overflow, "auto");
    assert.equal(live.table.scrolls, true);
    assert.equal(live.extras.strike, "old text");
    assert.ok(live.extras.ruleWidth > 0);
    assert.equal(live.extras.nestedItems, 2);
    assert.equal(live.extras.nestedTaskItems, 2);
    assert.equal(live.extras.checkedTasks, 1);
    assert.equal(live.extras.disabledTasks, 2);
    assert.equal(live.extras.orderedStart, 3);
    assert.deepEqual(live.extras.orderedValues, [3, 5]);
    assert.equal(live.extras.continuation, "on another line");
    assert.equal(live.extras.deepestHeadings, 2);
    // CSS resolves the custom property's color to rgb() for the anchor.
    const expectedColor = await page.evaluate(() => {
      const el = document.createElement("span");
      el.style.color = "var(--accent)";
      document.body.append(el);
      const color = getComputedStyle(el).color;
      el.remove();
      return color;
    });
    assert.equal(live.linkColor, expectedColor);
    await reloadAndWait(page);
    assert.deepEqual(await page.evaluate(styles), live);
  } finally { await ctx.close(); }
});

test("an output-limited tool proposal leaves no phantom card", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    const proposal = toolScript("incomplete_write", "fs_write", { path: "work/never-written.txt", content: "do not execute" });
    proposal.at(-1).finish = "length";
    llm.queueScripts(proposal);
    await sendChat(page, "write a file");
    await waitIdle(page);
    const live = await digest(page);
    const answer = assistants(live).at(-1);
    assert.equal(answer.cards.length, 0, "an unexecuted proposal remained visible");
    assert.match(answer.note, /output limit/);
    assert.equal(answer.bodyVisible, false);
    await reloadAndWait(page);
    assert.deepEqual(comparable(await digest(page)), comparable(live));
  } finally { await ctx.close(); }
});

test("mobile add-menu preserves composer focus without opening the keyboard unnecessarily", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession({ isMobile: true, hasTouch: true, viewport: { width: 390, height: 844 } });
  try {
    const input = page.locator("#composer-input");
    const add = page.locator("#btn-add");
    await input.fill("A draft to keep editing");
    await input.evaluate((el) => {
      el.setSelectionRange(7, 7);
      window.composerBlurCount = 0;
      el.addEventListener("blur", () => { window.composerBlurCount++; });
    });
    for (const open of [true, false]) {
      await add.tap();
      assert.deepEqual(await page.evaluate(() => ({
        focused: document.activeElement === document.querySelector("#composer-input"),
        blurs: window.composerBlurCount,
        selection: document.querySelector("#composer-input").selectionStart,
        open: !document.querySelector("#add-menu").hidden,
      })), { focused: true, blurs: 0, selection: 7, open });
    }
    assert.equal(await input.inputValue(), "A draft to keep editing");
    // A tap with no active editor must not bring up the keyboard.
    await input.blur();
    await add.tap();
    assert.equal(await input.evaluate((el) => document.activeElement === el), false);
    assert.equal(await page.locator("#add-menu").isVisible(), true);
    // Focus and activation by keyboard remain available.
    await add.focus();
    await add.press("Space");
    assert.equal(await page.locator("#add-menu").isVisible(), false);
    assert.equal(await add.evaluate((el) => document.activeElement === el), true);
  } finally { await ctx.close(); }
});

test("floating composer preserves scroll clearance as drafts and attachments grow", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    llm.queueScripts(textScript("A readable line in a longer answer.\n\n".repeat(35)));
    await sendChat(page, "give me a longer answer");
    await waitIdle(page);
    for (const width of [1280, 320]) {
      await page.setViewportSize({ width, height: 800 });
      await page.fill("#composer-input", "A draft line\n".repeat(14));
      await page.locator("#attach-input").setInputFiles(
        Array.from({ length: 4 }, (_, i) => ({
          name: `supporting-notes-${width}-${i}.txt`, mimeType: "text/plain", buffer: Buffer.from("notes"),
        })),
      );
      await page.waitForFunction(() => document.querySelectorAll(".attach-chip:not(.uploading)").length === 4);
      await page.waitForFunction(() => {
        const main = document.querySelector(".chat-main");
        const stack = document.querySelector("#composer-stack");
        return Math.abs(parseFloat(main.style.getPropertyValue("--composer-clearance")) - stack.getBoundingClientRect().height) < 1;
      });
      const geometry = await page.evaluate(() => {
        const main = document.querySelector(".chat-main");
        const thread = document.querySelector("#thread");
        const stack = document.querySelector("#composer-stack");
        thread.scrollTop = thread.scrollHeight;
        return {
          mainBottom: main.getBoundingClientRect().bottom,
          threadBottom: thread.getBoundingClientRect().bottom,
          stackTop: stack.getBoundingClientRect().top,
          lastBottom: thread.lastElementChild.getBoundingClientRect().bottom,
          overflow: document.documentElement.scrollWidth > innerWidth,
          attachmentParent: document.querySelector("#pending-files").parentElement.id,
          meterTop: document.querySelector("#ctx-meter").getBoundingClientRect().top,
          meterHeight: document.querySelector("#ctx-meter").getBoundingClientRect().height,
          composerBottom: document.querySelector(".composer").getBoundingClientRect().bottom,
        };
      });
      assert.ok(Math.abs(geometry.mainBottom - geometry.threadBottom) < 1, "thread must extend behind the composer");
      assert.ok(geometry.lastBottom <= geometry.stackTop - 8, "end of answer was hidden beneath the floating controls");
      assert.equal(geometry.overflow, false);
      assert.equal(geometry.attachmentParent, "composer-stack");
      assert.ok(geometry.meterTop >= geometry.composerBottom, "context footnote must sit below the message box");
      assert.ok(geometry.meterHeight > 0 && geometry.meterHeight < 20, "context footnote should stay compact");
      // Leave the reader's scroll position alone when a draft changes.
      await page.evaluate(() => { document.querySelector("#thread").scrollTop = 0; });
      await page.fill("#composer-input", "short draft");
      await page.waitForFunction(() => {
        const main = document.querySelector(".chat-main");
        return Math.abs(parseFloat(main.style.getPropertyValue("--composer-clearance")) - document.querySelector("#composer-stack").getBoundingClientRect().height) < 1;
      });
      assert.equal(await page.locator("#thread").evaluate((el) => el.scrollTop), 0);
      while (await page.locator(".chip-remove").count()) await page.locator(".chip-remove").first().click();
    }
    await page.click("#btn-add");
    await page.waitForSelector("#add-menu:visible");
    assert.equal(await page.locator("#btn-attach").isVisible(), true);
  } finally { await ctx.close(); }
});

test("older-chat pagination follows the list without covering rows or skipping loaded chats", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    const cookies = await ctx.cookies(app.base);
    const csrf = cookies.find((c) => c.name === "mullsmith_csrf").value;
    for (let i = 0; i < 35; i++) {
      const response = await page.request.post(`${app.base}/api/v1/conversations`, {
        headers: { "X-Mullsmith-CSRF": csrf }, data: { title: `Pagination check ${i}` },
      });
      assert.equal(response.status(), 201);
    }
    for (const width of [1280, 900, 744, 390]) {
      await page.setViewportSize({ width, height: 800 });
      await page.reload({ waitUntil: "domcontentloaded" });
      await page.waitForFunction(() => !document.querySelector("#composer-input").disabled);
      if (width < 1024) await page.click("#btn-menu");
      await Promise.all([
        page.waitForResponse((response) => {
          const url = new URL(response.url());
          return url.pathname === "/api/v1/conversations" && url.searchParams.get("q") === "Pagination check" && !url.searchParams.get("cursor");
        }),
        page.fill("#conv-search", "Pagination check"),
      ]);
      await page.waitForFunction(() => document.querySelectorAll("#conv-list .conv-item").length === 30);
      const before = await page.evaluate(() => {
        const scroll = document.querySelector(".conv-window");
        const more = document.querySelector("#btn-more");
        const last = document.querySelector("#conv-list").lastElementChild;
        const initiallyBelow = more.getBoundingClientRect().top >= scroll.getBoundingClientRect().bottom;
        scroll.scrollTop = scroll.scrollHeight;
        return {
          initiallyBelow, scrollTop: scroll.scrollTop,
          lastBottom: last.getBoundingClientRect().bottom, buttonTop: more.getBoundingClientRect().top,
          buttonHeight: more.getBoundingClientRect().height,
        };
      });
      if (width >= 1024) {
        const shell = await page.evaluate(() => ({
          sidebarTop: document.querySelector("#sidebar").getBoundingClientRect().top,
          sidebarBottom: document.querySelector("#sidebar").getBoundingClientRect().bottom,
          headerHeight: document.querySelector(".topbar").getBoundingClientRect().height,
          chatHeight: document.querySelector(".chat-main").getBoundingClientRect().height,
          viewportHeight: innerHeight,
        }));
        assert.equal(shell.sidebarTop, 0, "desktop sidebar must start at the top of the page");
        assert.ok(Math.abs(shell.sidebarBottom - shell.viewportHeight) < 1);
        assert.ok(shell.headerHeight < 100, "long history stretched the header row");
        assert.ok(shell.chatHeight > shell.viewportHeight * 0.6, "long history collapsed the chat area");
      }
      assert.equal(await page.locator("#topbar-title").count(), 1);
      assert.equal(await page.locator("#topbar-title").isVisible(), width >= 740,
        "conversation title should be visible at tablet and desktop widths");
      if (width === 744 || width === 900) {
        const tabletHeader = await page.evaluate(() => {
          const topbar = document.querySelector(".topbar");
          const title = document.querySelector("#topbar-title").getBoundingClientRect();
          const menu = document.querySelector("#btn-menu").getBoundingClientRect();
          const probe = document.createElement("div");
          probe.className = "msg";
          document.querySelector(".thread").append(probe);
          const conversationLeft = probe.getBoundingClientRect().left;
          probe.remove();
          const burgerClearance = menu.right + parseFloat(getComputedStyle(topbar).columnGap);
          return {
            centreOffset: (title.top + title.height / 2) - (menu.top + menu.height / 2),
            leftOffset: title.left - Math.max(burgerClearance, conversationLeft),
          };
        });
        assert.ok(Math.abs(tabletHeader.centreOffset) < 1, "tablet title should share the controls row");
        assert.ok(Math.abs(tabletHeader.leftOffset) < 1,
          "tablet title should follow the greater of the burger and conversation edges");
      }
      assert.equal(before.initiallyBelow, true, "pagination floated over the visible list");
      assert.ok(before.lastBottom <= before.buttonTop, "pagination covered the oldest visible chat");
      assert.ok(before.buttonHeight >= 44, "pagination lost its touch target");
      await page.getByRole("button", { name: "Load older chats", exact: true }).click();
      await page.waitForFunction(() => document.querySelectorAll("#conv-list .conv-item").length === 35);
      assert.equal(await page.locator("#btn-more").isVisible(), false);
      const after = await page.locator(".conv-window").evaluate((el) => el.scrollTop);
      assert.ok(Math.abs(before.scrollTop - after) < 2, "pagination jumped past newly loaded chats");
    }
  } finally { await ctx.close(); }
});

test("slow chat creation and acceptance cannot masquerade as readiness or completion", { timeout: 120_000 }, async () => {
  const { ctx, page } = await newSession();
  try {
    await page.route("**/api/v1/conversations", async (route) => {
      if (route.request().method() === "POST") await new Promise((resolve) => setTimeout(resolve, 600));
      await route.continue();
    });
    const previous = await page.evaluate(() => location.hash);
    const created = await newConversation(page);
    assert.equal(await page.evaluate(() => location.hash), `#/c/${created}`);
    assert.notEqual(`#/c/${created}`, previous);
    assert.equal(await page.locator("#thread .msg").count(), 0);

    await page.route("**/api/v1/conversations/*/messages", async (route) => {
      if (route.request().method() === "POST") await new Promise((resolve) => setTimeout(resolve, 600));
      await route.continue();
    });
    llm.queueScripts(textScript("The delayed reply finished."));
    await sendChat(page, "wait for acceptance and the actual reply");
    await waitIdle(page);
    const replies = assistants(await digest(page));
    assert.equal(replies.length, 1);
    assert.equal(replies[0].text, "The delayed reply finished.");
    assert.equal(replies[0].streaming, false);
    assert.equal(replies[0].lastAssistant, true);
  } finally { await ctx.close(); }
});

test("settings email changes require a conditional password field", async () => {
  const { ctx, page } = await newSession();
  try {
    const openSettings = async () => {
      await page.locator("#btn-settings").click();
      await page.waitForSelector("#settings-panel:not([hidden])");
    };
    await openSettings();
    const email = page.locator("#settings-email");
    const password = page.locator("#settings-email-password");
    const original = await email.inputValue();
    assert.equal(await password.isVisible(), false);
    await page.locator("#settings-name").fill("Profile without password");
    const saved = page.waitForResponse((r) => r.url().endsWith("/api/v1/auth/me") && r.request().method() === "PATCH");
    await page.locator("#form-profile button[type=submit]").click();
    const metadata = await saved;
    assert.equal(metadata.status(), 200);
    assert.equal("current_password" in metadata.request().postDataJSON(), false);
    await page.waitForSelector("#settings-panel[hidden]", { state: "attached" });
    await openSettings();
    await email.fill("changed-browser@example.io");
    assert.equal(await password.isVisible(), true);
    const emailBox = await email.boundingBox();
    const passwordBox = await password.boundingBox();
    const promptBox = await page.locator("#settings-prompt").boundingBox();
    assert.ok(passwordBox.y > emailBox.y && passwordBox.y < promptBox.y);
    await password.fill("discard-this-secret");
    await email.fill(original.toUpperCase());
    assert.equal(await password.isVisible(), false);
    assert.equal(await password.inputValue(), "");
    await email.fill("changed-browser@example.io");
    await page.locator("#form-profile button[type=submit]").click();
    assert.equal(await password.evaluate((el) => document.activeElement === el), true);
    await password.fill("wrong-password");
    const refused = page.waitForResponse((r) => r.url().endsWith("/api/v1/auth/me") && r.request().method() === "PATCH");
    await page.locator("#form-profile button[type=submit]").click();
    assert.equal((await refused).status(), 403);
    await page.waitForFunction(() => document.querySelector("#settings-email-password").value === "");
    assert.equal(await page.locator("#settings-panel").isVisible(), true);
    await password.fill("uncommon-password-9");
    const changed = page.waitForResponse((r) => r.url().endsWith("/api/v1/auth/me") && r.request().method() === "PATCH");
    await page.locator("#form-profile button[type=submit]").click();
    const response = await changed;
    assert.equal(response.status(), 200);
    assert.equal((await response.json()).email, "changed-browser@example.io");
    await page.waitForSelector("#settings-panel[hidden]", { state: "attached" });
    await openSettings();
    assert.equal(await email.inputValue(), "changed-browser@example.io");
    assert.equal(await password.isVisible(), false);
    assert.equal(await password.inputValue(), "");
  } finally {
    await ctx.close();
  }
});


test("over-limit scratchpads remain manageable in Files", async () => {
  const { ctx, page } = await newSession();
  let fixture;
  try {
    const me = await (await page.request.get(`${app.base}/api/v1/auth/me`)).json();
    const relative = "work/entry-ceiling-browser";
    fixture = join(app.dir, "scratch", me.id, relative);
    mkdirSync(fixture, { recursive: true });
    for (let i = 0; i < 1001; i++) writeFileSync(join(fixture, `empty-${i}.txt`), "");
    await page.click("#btn-files");
    await page.click('.files-filter[data-kind="work"]');
    await page.waitForFunction(() => document.querySelector("#quota-text").textContent.includes("entry limit exceeded") && document.querySelectorAll("#files-list .file-row").length > 0);
    assert.ok(await page.locator("#files-list .file-row").count() > 0);
    assert.equal(await page.locator(".quota-bar").isVisible(), false);
    const csrf = (await ctx.cookies(app.base)).find((c) => c.name === "mullsmith_csrf").value;
    const deleted = await page.request.delete(`${app.base}/api/v1/files?path=${encodeURIComponent(relative)}`, {
      headers: { "X-Mullsmith-CSRF": csrf },
    });
    assert.equal(deleted.status(), 204);
    await page.click('.files-filter[data-kind="work"]');
    await page.waitForFunction(() => document.querySelector("#quota-text").textContent.includes(" of "));
    assert.equal(await page.locator(".quota-bar").isVisible(), true);
  } finally {
    if (fixture) rmSync(fixture, { recursive: true, force: true });
    await ctx.close();
  }
});
