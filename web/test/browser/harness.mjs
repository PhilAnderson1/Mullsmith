// Browser-suite harness (C7 Stage 6): a scripted OpenAI-compatible upstream,
// and the REAL mullsmith binary booted against a temporary database and
// scratchpad pointing at it. Nothing here mocks the app: Chromium drives the
// shipped HTML/JS over the network exactly like a user.
//
// The fake LLM serves a QUEUE of reply scripts. Each POST /v1/chat/completions
// pops one; every request's decoded body is recorded so a test can assert what
// the retry prompt did NOT contain. Scripts are tiny arrays of deltas, which
// the writer turns into OpenAI SSE chunks — including optional per-chunk
// delays for abort-mid-stream tests.

import { createServer } from "node:http";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// --- the fake OpenAI-compatible endpoint -----------------------------------------

export async function startFakeLLM() {
  const state = { queue: [], requests: [] };
  const server = createServer((req, res) => {
    if (req.method === "GET" && req.url.startsWith("/v1/models")) {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ object: "list", data: [{ id: "test-model", object: "model" }] }));
      return;
    }
    if (req.method !== "POST" || !req.url.startsWith("/v1/chat/completions")) {
      res.writeHead(404).end();
      return;
    }
    let raw = "";
    req.on("data", (d) => (raw += d));
    req.on("end", async () => {
      let body = {};
      try { body = JSON.parse(raw); } catch { /* keep the raw count honest */ }
      state.requests.push(body);
      // Non-streaming callers (the auto-title) never consume a scripted
      // streaming reply: they get a fixed JSON completion. That keeps each
      // test's queue exactly the model rounds it declares.
      if (body.stream !== true) {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({
          id: "chatcmpl-fake", object: "chat.completion", created: 0, model: "test-model",
          choices: [{ index: 0, message: { role: "assistant", content: "Browser test" }, finish_reason: "stop" }],
        }));
        return;
      }
      const script = state.queue.shift();
      if (!script) {
        res.writeHead(500, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message: "fake LLM ran out of scripted replies" } }));
        return;
      }
      res.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
      });
      const chunk = (delta, finish) => JSON.stringify({
        id: "chatcmpl-fake", object: "chat.completion.chunk", created: 0,
        model: "test-model",
        choices: [{ index: 0, delta, finish_reason: finish || null }],
      });
      for (const f of script) {
        if (f.delay) await sleep(f.delay);
        res.write(`data: ${chunk(f.delta, f.finish)}\n\n`);
      }
      res.write("data: [DONE]\n\n");
      res.end();
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const port = server.address().port;
  return {
    url: `http://127.0.0.1:${port}/v1`,
    state,
    // queueScripts appends replies consumed in order, one per model call.
    queueScripts(...scripts) { state.queue.push(...scripts); },
    close() { return new Promise((r) => server.close(r)); },
  };
}

// textScript answers with prose, optionally slowly (abort scenarios).
export function textScript(text, { pieces = 2, delayMs = 0, finish = "stop" } = {}) {
  const each = Math.max(1, Math.ceil([...text].length / pieces));
  const script = [];
  for (let i = 0; i < text.length; i += each) {
    script.push({ delta: { content: text.slice(i, i + each) }, delay: delayMs });
  }
  script.push({ delta: {}, finish });
  return script;
}

// toolScript answers with one complete tool call (arguments arrive as a
// single delta; the client/server assemble fragments either way).
export function toolScript(id, name, args) {
  return [
    { delta: { tool_calls: [{ index: 0, id, type: "function", function: { name, arguments: JSON.stringify(args) } }] } },
    { delta: {}, finish: "tool_calls" },
  ];
}

// phantomScript reproduces the fragmentation seen from real endpoints: the
// NAME arrives with no id, the arguments stream without one, and the id
// appears only on the final fragment. The live card announcement and the
// audit row's terminal event may then carry different ids — the client
// must still settle ONE card.
export function phantomScript(name, args, lateId) {
  return [
    { delta: { tool_calls: [{ index: 0, function: { name, arguments: "" } }] } },
    { delta: { tool_calls: [{ index: 0, function: { arguments: JSON.stringify(args) } }] } },
    { delta: { tool_calls: [{ index: 0, id: lateId, function: { arguments: "" } }] } },
    { delta: {}, finish: "tool_calls" },
  ];
}

// --- the real server ---------------------------------------------------------------

function freePort() {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.on("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

// startApp builds a config against the fake endpoint, boots the binary and
// waits for /api/v1/meta. `approval` overrides the per-tool policy map.
export async function startApp(llmUrl, { repoRoot, binPath, approval } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "mullsmith-browser-"));
  const port = await freePort();
  const policy = {
    fs_write: "auto", fs_list: "auto", fs_read: "auto", files_share: "auto",
    web_search: "auto", web_fetch: "auto", code_run: "confirm", ...(approval || {}),
  };
  const yaml = `
server: { listen: "127.0.0.1:${port}", tls: { enabled: false }, shutdown_grace: "3s" }
database: { dir: "${dir}/db" }
scratchpad:
  root: "${dir}/scratch"
  code_execution: { enabled: false }
auth: { registration: open, argon2: { time: 1, memory: 65536, threads: 4 } }
llm:
  endpoint: "${llmUrl}"
  models: [ { id: "test-model", default: true, context_size: 32768 } ]
  endpoints_file: "${repoRoot}/configs/endpoints.yaml"
  profiles_file: "${repoRoot}/configs/thinking-profiles.yaml"
  endpoint_type: gufo
tools:
  web_fetch: { enabled: true }
  agent:
    max_tool_rounds: 4
    approval: { ${Object.entries(policy).map(([k, v]) => `${k}: ${v}`).join(", ")} }
`;
  const { writeFileSync } = await import("node:fs");
  const cfgPath = join(dir, "config.yaml");
  writeFileSync(cfgPath, yaml);

  const proc = spawn(binPath, ["-config", cfgPath], { stdio: ["ignore", "pipe", "pipe"] });
  let logs = "";
  proc.stdout.on("data", (d) => (logs += d));
  proc.stderr.on("data", (d) => (logs += d));
  const base = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 20_000;
  for (;;) {
    if (proc.exitCode !== null) throw new Error(`server exited early:\n${logs}`);
    try {
      const r = await fetch(`${base}/api/v1/meta`);
      if (r.ok) break;
    } catch { /* not listening yet */ }
    if (Date.now() > deadline) throw new Error(`server never came up:\n${logs}`);
    await sleep(100);
  }
  return {
    base,
    dir,
    logs: () => logs,
    async stop() {
      if (proc.exitCode === null) {
        proc.kill("SIGTERM");
        await Promise.race([new Promise((r) => proc.on("exit", r)), sleep(5000)]);
      }
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

// registerAndOpen registers a fresh account through the API (registration is
// open in the test config), mirroring what the client does: the CSRF cookie
// issued by the first GET must be echoed in X-Mullsmith-CSRF. Then it lands
// on the app.
export async function registerAndOpen(page, base, email) {
  await page.goto(`${base}/`, { waitUntil: "domcontentloaded" });
  const cookies = await page.context().cookies(base);
  const csrf = cookies.find((c) => c.name === "mullsmith_csrf")?.value || "";
  const resp = await page.request.post(`${base}/api/v1/auth/register`, {
    headers: { "X-Mullsmith-CSRF": csrf },
    data: { email, name: "Browser Tester", password: "uncommon-password-9" },
  });
  if (![200, 201, 204].includes(resp.status())) {
    throw new Error(`register failed: ${resp.status()} ${await resp.text()}`);
  }
  await page.goto(`${base}/`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#btn-new-chat:visible");
}

// --- DOM digests -------------------------------------------------------------------

// threadDigest captures EVERYTHING the equivalence claims are about, per
// bubble, in DOM order: identity, prose, terminal note, tool-card state and
// summary, download cards, and the regenerate affordance (real CSS: the
// button is only VISIBLE on the .last-assistant bubble in Chromium).
export const threadDigest = () =>
  Array.from(document.querySelectorAll("#thread .msg")).map((el) => {
    const body = el.querySelector(".msg-body");
    const note = el.querySelector(".msg-status");
    const regen = el.querySelector(".regen-btn");
    // Prose only: .msg-body.innerText also contains card text, and cards
    // legitimately disagree with prose placement between views — `text`
    // is what the ANSWER must agree on.
    const textNode = body ? body.querySelector(".stream-text") : null;
    return {
      id: el.dataset.id || "",
      role: el.classList.contains("from-user") ? "user" : "assistant",
      lastAssistant: el.classList.contains("last-assistant"),
      streaming: !!body && body.classList.contains("streaming"),
      bodyVisible: !!body && body.getClientRects().length > 0,
      text: textNode ? textNode.innerText.trim() : (body ? body.innerText.trim() : ""),
      note: note && !note.hidden ? note.textContent.trim() : "",
      regenVisible: !!regen && regen.offsetParent !== null,
      cards: Array.from(el.querySelectorAll(".tool-card")).map((c) => ({
        tool: (c.querySelector(".tool-name") || {}).textContent?.trim() || "",
        state: (c.querySelector(".tool-state") || {}).textContent?.trim() || "",
        summary: (c.querySelector(".tool-summary") || {}).textContent?.trim() || "",
      })),
      artifacts: Array.from(el.querySelectorAll(".artifact-card")).map((a) => ({
        name: (a.querySelector(".artifact-name") || {}).textContent?.trim() || "",
        href: (a.querySelector("a") || {}).getAttribute?.("href") || "",
      })),
    };
  });

// sendChat composes and sends through the real composer.
export async function sendChat(page, text) {
  await page.fill("#composer-input", text);
  // No thinking fragment: the fake endpoint answers plain Chat Completions.
  // Some test configurations have no empty-valued option. Do not spend
  // Playwright's action timeout waiting for an option that cannot appear.
  const thinking = page.locator("#thinking-select");
  if (await thinking.isEnabled() && await thinking.locator('option[value=""]').count()) {
    await thinking.selectOption("");
  }
  await page.click("#btn-send");
}

// waitIdle resolves once no bubble is streaming any more — the same signal a
// user reads as "the reply finished".
export async function waitIdle(page, timeout = 30_000) {
  await page.waitForFunction(
    () => !document.querySelector(".msg-body.streaming"), null, { timeout },
  );
  // A beat for the terminal events' DOM writes (notes, buttons) to settle.
  await sleep(150);
}
