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
import { existsSync, readdirSync } from "node:fs";
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
async function newSession() {
  const ctx = await browser.newContext(sessionState ? { storageState: sessionState } : {});
  const page = await ctx.newPage();
  if (!sessionState) {
    await registerAndOpen(page, app.base, `browser${++userSeq}@example.io`);
    sessionState = await ctx.storageState();
  } else {
    await page.goto(`${app.base}/`, { waitUntil: "domcontentloaded" });
    await waitForConversation(page);
  }
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

    await waitIdle(page);
    const live = await digest(page);
    const settled = assistants(live);
    assert.deepEqual(settled.map((m) => m.bodyVisible), [false, false, true, true]);
    assert.equal(settled[2].text, "The file is ready.");
    assert.equal(settled[3].text, "Here is the final answer.");

    await reloadAndWait(page);
    assert.deepEqual(comparable(await digest(page)), comparable(live),
      "the visibility of answer bodies must agree live and after reload");
  } finally { await ctx.close(); }
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
  const { ctx, page } = await newSession();
  const styles = () => {
    const prose = document.querySelector(".from-assistant .stream-text.md");
    const css = (selector) => getComputedStyle(prose.querySelector(selector));
    return {
      whiteSpace: getComputedStyle(prose).whiteSpace,
      paragraphMargin: parseFloat(css("p").marginBottom),
      quoteBorder: parseFloat(css("blockquote").borderInlineStartWidth),
      codeFont: css("code").fontFamily,
      linkColor: css("a").color,
      accent: getComputedStyle(document.documentElement).getPropertyValue("--accent").trim(),
    };
  };
  try {
    llm.queueScripts(textScript("# Heading\n\nA paragraph with `code` and [a link](https://example.com).\n\n- One\n- Two\n\n> A quotation"));
    await sendChat(page, "format an answer");
    await waitIdle(page);
    const live = await page.evaluate(styles);
    assert.equal(live.whiteSpace, "normal");
    assert.ok(live.paragraphMargin > 0);
    assert.ok(live.quoteBorder > 0);
    assert.match(live.codeFont, /monospace/);
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
    for (const width of [1280, 390]) {
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
        assert.equal(await page.locator("#topbar-title").isVisible(), false);
      } else {
        assert.equal(await page.locator("#topbar-title").isVisible(), true);
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
