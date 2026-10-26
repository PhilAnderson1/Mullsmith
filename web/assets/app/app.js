// Gabkeep SPA — M3: auth, conversations and live streamed chat.
// Tools and files arrive in M4/M5. No framework, no build step (spec §10).
// DOM helpers are local; views are plain functions over a single state object.

import { api, ApiError } from "/assets/app/api.js";
import { streamChat } from "/assets/app/sse.js";
import { renderMarkdown } from "/assets/app/markdown.js";

const state = {
  meta: null,
  me: null,
  convs: [],
  convCursor: "",
  query: "",
  currentId: "",
  // one-shot: set when entering chat / new chat so selectConversation focuses
  // the composer once the conversation is actually open (spec §10 UX).
  focusOnOpen: false,
  currentConv: null,
  messagesCursor: "",
  inflight: null, // live generation: { controller, genId, msgId, raw, ... }
  // uploads staged in the composer, sent as attachment_paths with the message
  pending: [], // { path, name, size, uploading }
  filesOpen: false,
  filesKind: "",
  settingsOpen: false,
};

// --- tiny DOM helpers -------------------------------------------------------

const $ = (sel) => document.querySelector(sel);

function clone(id) {
  return $("#"+id).content.firstElementChild.cloneNode(true);
}

function show(el, on = true) {
  el.hidden = !on;
}

function debounce(fn, ms) {
  let t;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
}

let toastTimer;
function toast(message, isError = true) {
  let el = $("#toast");
  if (!el) {
    el = document.createElement("div");
    el.id = "toast";
    el.setAttribute("role", "status");
    document.body.appendChild(el);
  }
  el.textContent = message;
  el.classList.toggle("toast-error", isError);
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 5000);
}

// 401 from anywhere means the session died mid-flight: fall back to auth.
function handleErr(err, where) {
  if (err instanceof ApiError && err.status === 401) {
    showAuth();
    return;
  }
  toast(`Error (${where}): ${err.message}`);
}

// --- theme -------------------------------------------------------------------

const THEME_KEY = "gk-theme";

function applyTheme() {
  const pref = localStorage.getItem(THEME_KEY) || "system";
  if (pref === "system") {
    document.documentElement.removeAttribute("data-theme");
  } else {
    document.documentElement.setAttribute("data-theme", pref);
  }
}

function cycleTheme() {
  const order = ["system", "light", "dark"];
  const cur = localStorage.getItem(THEME_KEY) || "system";
  const next = order[(order.indexOf(cur) + 1) % order.length];
  localStorage.setItem(THEME_KEY, next);
  applyTheme();
  toast(`Theme: ${next}`, false);
}

// --- view switching ------------------------------------------------------------

function showAuth() {
  abandonInflight();
  state.me = null;
  state.currentId = "";
  state.pending = [];
  renderPending();
  closeFilesView();
  closeSettingsView();
  show($("#view-chat"), false);
  show($("#view-auth"), true);
  routeAuthForm();
  $("#login-email").focus();
}

async function enterChat(user) {
  state.me = user;
  show($("#view-auth"), false);
  show($("#view-chat"), true);
  $("#user-chip").textContent = user.name || user.email;
  if (location.hash && !location.hash.startsWith("#/c/") && location.hash !== "#/files") {
    location.hash = ""; // drop #login/#register leftovers
  }
  if (location.hash.startsWith("#/c/")) {
    // Let the hash drive the initial selection (awaited: the auto-select
    // below must not race an in-flight deep-link open).
    state.focusOnOpen = true;
    await onHashChange();
  }
  if (!state.currentId) {
    location.hash = "";
    renderConversation();
  }
  try {
    await loadConversations();
  } catch (e) {
    handleErr(e, "load conversations");
    return;
  }
  // Fresh entry without a deep link: land in the most recent conversation
  // (the list is ordered by updated_at DESC) ready for typing.
  if (!state.currentId && state.convs.length) {
    state.focusOnOpen = true;
    await selectConversation(state.convs[0].id);
    location.hash = `#/c/${state.convs[0].id}`;
  }
}

// --- auth view -------------------------------------------------------------------

function routeAuthForm() {
  const wantRegister = location.hash === "#register" &&
    state.meta.registration !== "closed";
  show($("#form-login"), !wantRegister);
  show($("#form-register"), wantRegister);
}

function applyMetaFlags() {
  const m = state.meta;
  show($("#login-remember-row"), m.remember_login);
  show($("#register-remember-row"), m.remember_login);
  show($("#register-invite-wrap"), m.registration === "invite-only");
  if (m.registration === "closed") {
    $("#auth-mode-note").textContent = "Accounts are created by the administrator.";
    show($("#auth-mode-note"), true);
    // Hide the WHOLE "No account yet?" sentence, not just the link — the
    // question itself is noise when nobody can register (the <p> owns the
    // text, so hiding #link-register alone left the sentence on screen).
    show($("#login-switch"), false);
  } else {
    show($("#login-switch"), true);
    if (m.registration === "invite-only") {
      show($("#auth-mode-note"), true);
    }
  }
  const sel = $("#model-picker");
  sel.replaceChildren();
  for (const [value, label] of [["", "Default model"], ...m.models.map((x) => [x, x])]) {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = label;
    sel.appendChild(opt);
  }
}

async function onLogin(ev) {
  ev.preventDefault();
  const errEl = $("#login-error");
  show(errEl, false);
  try {
    const me = await api.login(
      $("#login-email").value.trim(),
      $("#login-password").value,
      $("#login-remember").checked
    );
    enterChat(me);
  } catch (err) {
    errEl.textContent = err.message;
    show(errEl, true);
  }
}

async function onRegister(ev) {
  ev.preventDefault();
  const errEl = $("#register-error");
  show(errEl, false);
  try {
    const me = await api.register({
      name: $("#register-name").value.trim(),
      email: $("#register-email").value.trim(),
      password: $("#register-password").value,
      remember_me: $("#register-remember").checked,
      invite: $("#register-invite").value.trim(),
    });
    enterChat(me);
  } catch (err) {
    errEl.textContent = err.message;
    show(errEl, true);
  }
}

async function onLogout() {
  try {
    await api.logout();
  } catch {
    /* the cookie is gone either way */
  }
  showAuth();
}

// --- conversation list ---------------------------------------------------------------

function convRow(c) {
  const li = clone("tpl-conv-item");
  li.dataset.id = c.id;
  li.querySelector(".conv-title").textContent = c.title;
  if (c.id === state.currentId) li.classList.add("active");

  li.querySelector(".conv-open").addEventListener("click", () => {
    location.hash = `#/c/${c.id}`;
    if (isPhone()) closeDrawer();
  });

  const form = li.querySelector(".conv-rename");
  const input = form.querySelector("input");
  li.querySelector(".conv-rename-btn").addEventListener("click", () => {
    input.value = c.title;
    show(li.querySelector(".conv-open"), false);
    show(form, true);
    input.focus();
    input.select();
  });
  const cancelRename = () => {
    show(form, false);
    show(li.querySelector(".conv-open"), true);
  };
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") cancelRename();
  });
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const title = input.value.trim();
    if (!title || title === c.title) return cancelRename();
    try {
      const updated = await api.patchConversation(c.id, { title });
      c.title = updated.title;
      li.querySelector(".conv-title").textContent = updated.title;
      if (c.id === state.currentId) renderConversation();
    } catch (err) {
      handleErr(err, "rename");
    }
    cancelRename();
  });

  li.querySelector(".conv-delete-btn").addEventListener("click", async () => {
    const ok = await confirmDialog(`Delete "${c.title}"?`);
    if (!ok) return;
    if (c.id === state.currentId) await abandonInflight();
    try {
      await api.deleteConversation(c.id);
      li.remove();
      if (c.id === state.currentId) {
        state.currentId = "";
        location.hash = "";
        renderConversation();
      }
    } catch (err) {
      handleErr(err, "delete");
    }
  });
  return li;
}

// applyTitle reflects a server-generated title (SSE "title" event) into the
// list row, the cached conversation object and the topbar, without a reload.
function applyTitle(id, title) {
  const c = state.convs.find((x) => x.id === id);
  if (c) c.title = title;
  const row = document.querySelector(`#conv-list .conv-item[data-id="${CSS.escape(id)}"]`);
  if (row) row.querySelector(".conv-title").textContent = title;
  if (id === state.currentId) $("#topbar-title").textContent = title;
}

// promoteConversation records "opened" as activity (owner request): bump
// the server (fire-and-forget — ordering is cosmetic and self-heals on the
// next load) and reorder our local list + DOM immediately so the click
// visibly moves the chat to the top without waiting on the round-trip.
function promoteConversation(id) {
  api.touchConversation(id).catch(() => {});
  const i = state.convs.findIndex((c) => c.id === id);
  if (i > 0) {
    const [c] = state.convs.splice(i, 1);
    state.convs.unshift(c);
    const row = document.querySelector(`#conv-list .conv-item[data-id="${CSS.escape(id)}"]`);
    if (row) $("#conv-list").prepend(row);
  }
}

async function loadConversations() {
  const data = await api.listConversations(state.query, "");
  state.convs = data.items;
  state.convCursor = data.next_cursor;
  const list = $("#conv-list");
  list.replaceChildren();
  for (const c of state.convs) list.appendChild(convRow(c));
  show($("#btn-more"), Boolean(state.convCursor));
}

async function loadMoreConversations() {
  if (!state.convCursor) return;
  const data = await api.listConversations(state.query, state.convCursor);
  state.convCursor = data.next_cursor;
  for (const c of data.items) {
    state.convs.push(c);
    $("#conv-list").appendChild(convRow(c));
  }
  show($("#btn-more"), Boolean(state.convCursor));
}

function markActive() {
  for (const li of $("#conv-list").children) {
    li.classList.toggle("active", li.dataset.id === state.currentId);
  }
}

// --- messages ---------------------------------------------------------------------------

function msgNode(m) {
  const el = clone("tpl-message");
  el.classList.add(m.role === "user" ? "from-user" : "from-assistant");
  el.dataset.id = m.id || "";
  el.querySelector(".msg-role").textContent = m.role === "user" ? "You" : "Gabkeep";
  const time = el.querySelector("time");
  time.dateTime = m.created_at;
  time.textContent = new Date(m.created_at).toLocaleString();

  const body = el.querySelector(".msg-body");
  if (m.role === "assistant" && m.content) {
    body.classList.add("md");
    body.append(renderMarkdown(m.content)); // XSS-safe: DOM-only builder
  } else {
    body.textContent = m.content;
  }

  // Tool cards from the audit projection (§10.2): collapsed by default,
  // placed above the answer like their live counterparts. A calls-only
  // assistant row has no prose: the empty body folds away.
  if (m.role === "assistant" && Array.isArray(m.tool_calls) && m.tool_calls.length) {
    const host = toolCardsHost(el);
    for (const c of m.tool_calls) {
      host.append(toolCardFromAudit(c));
      const art = artifactFromAudit(c); // files_share ⇒ visible download card
      if (art) host.append(art);
    }
    if (!m.content) body.hidden = true;
  }
  if (m.role === "user" && Array.isArray(m.files) && m.files.length) {
    appendFileChips(el, m.files);
  }

  if (m.status === "aborted") {
    showStatusNote(el, "Stopped before finishing.");
  } else if (m.status === "error") {
    showStatusNote(el, "The model endpoint failed for this reply.");
  } else if (m.status && m.status !== "complete") {
    showStatusNote(el, `status: ${m.status}`);
  }

  if (m.role === "assistant") attachRegen(el);
  return el;
}

function showStatusNote(el, text) {
  const st = el.querySelector(".msg-status");
  st.textContent = text;
  show(st, true);
}

async function selectConversation(id) {
  if (id === state.currentId) return;
  await abandonInflight(); // a live stream belongs to the old thread
  promoteConversation(id); // opening is activity: move the row to the top
  try {
    state.currentConv = await api.getConversation(id);
    state.currentId = id;
    state.messagesCursor = "";
    await loadFirstMessages();
    markActive();
    renderConversation();
    if (state.focusOnOpen) {
      state.focusOnOpen = false;
      focusComposer();
    }
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      toast("That conversation no longer exists.");
      state.currentId = "";
      location.hash = "";
      renderConversation();
      return;
    }
    handleErr(err, "open conversation");
  }
}

async function loadFirstMessages() {
  const data = await api.listMessages(state.currentId, "");
  const thread = $("#thread");
  thread.replaceChildren();
  if (data.items.length === 0) {
    thread.appendChild(initialEmptyState());
  } else {
    if (data.next_cursor) thread.appendChild(earlierButton(data.next_cursor));
    // role='tool' rows carry model-facing payloads; the UI shows them as
    // tool cards attached to the assistant message that proposed the call.
    for (const m of data.items) if (m.role !== "tool") thread.appendChild(msgNode(m));
  }
  state.messagesCursor = data.next_cursor;
  refreshLastAssistant();
  scrollThreadToBottom();
}

function earlierButton(cursor) {
  const btn = document.createElement("button");
  btn.className = "btn ghost wide load-earlier";
  btn.textContent = "Load earlier messages";
  btn.addEventListener("click", async () => {
    try {
      const data = await api.listMessages(state.currentId, cursor);
      const host = $("#thread");
      const nextBtn = data.next_cursor ? earlierButton(data.next_cursor) : null;
      if (nextBtn) btn.replaceWith(nextBtn);
      else btn.remove();
      // The page is chronological (oldest first); insert each item before the
      // same ref so the whole page lands between the button and what was there.
      const ref = nextBtn ? nextBtn.nextSibling : host.firstChild;
      for (const m of data.items) if (m.role !== "tool") host.insertBefore(msgNode(m), ref);
      state.messagesCursor = data.next_cursor;
      refreshLastAssistant();
    } catch (err) {
      handleErr(err, "load earlier messages");
    }
  }, { once: true });
  return btn;
}

function scrollThreadToBottom() {
  const t = $("#thread");
  t.scrollTop = t.scrollHeight;
}

// --- live generation (spec §6.1 streaming, §6.2 abort) ------------------------

function isNearBottom() {
  const t = $("#thread");
  return t.scrollHeight - t.scrollTop - t.clientHeight < 90;
}

function maybeScroll(force = false) {
  if (force || isNearBottom()) scrollThreadToBottom();
}

// Only the newest finished assistant bubble offers regenerate.
function refreshLastAssistant() {
  const thread = $("#thread");
  for (const el of thread.querySelectorAll(".msg")) el.classList.remove("last-assistant");
  const msgs = thread.querySelectorAll(".msg");
  const last = msgs[msgs.length - 1];
  if (
    last &&
    last.classList.contains("from-assistant") &&
    !last.querySelector(".msg-body.streaming")
  ) {
    last.classList.add("last-assistant");
  }
}

function syncComposerButtons() {
  const busy = Boolean(state.inflight);
  $("#btn-send").disabled = busy || !state.currentId;
  $("#btn-stop").hidden = !busy;
}

function focusComposer() {
  // On phones focusing the composer pops the soft keyboard over the thread;
  // the keyboard pops naturally when the user taps to type anyway.
  if (isPhone()) return;
  const input = $("#composer-input");
  if (!input.disabled) input.focus({ preventScroll: true });
}

function autosizeComposer() {
  const el = $("#composer-input");
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, Math.round(window.innerHeight * 0.3)) + "px";
}

// The composer entry point: optimistic user bubble, then the streamed reply.
// The rollback restores the draft (and the attachment chips) if the server
// rejects before starting.
async function sendFromComposer() {
  const input = $("#composer-input");
  const text = input.value;
  if (!state.currentId || state.inflight || !text.trim()) return;
  if (state.pending.some((f) => f.uploading)) {
    toast("Still uploading — give it a moment.", false);
    return;
  }
  const attachments = state.pending.slice();
  const placeholder = $("#thread .empty-state");
  if (placeholder) placeholder.remove();
  const userEl = appendUserBubble(text.trim(), attachments);
  input.value = "";
  autosizeComposer();
  const body = { content: text.trim() };
  if (attachments.length) body.attachment_paths = attachments.map((f) => f.path);
  await startGeneration(
    `/api/v1/conversations/${encodeURIComponent(state.currentId)}/messages`,
    body,
    () => {
      userEl.remove();
      input.value = text;
      autosizeComposer();
      input.focus();
    },
    () => {
      // Accepted: the server now owns these uploads, clear the tray.
      state.pending = state.pending.filter((f) => !attachments.includes(f));
      renderPending();
    }
  );
}

function appendUserBubble(text, files) {
  const el = msgNode({
    role: "user",
    content: text,
    status: "complete",
    created_at: new Date().toISOString(),
  });
  if (files && files.length) appendFileChips(el, files);
  $("#thread").append(el);
  maybeScroll(true);
  return el;
}

// --- busy spinner (pure-CSS dots) ------------------------------------------------

// spinnerNode returns an inline three-dot span animated entirely by CSS
// (styles.css .spinner). The old version cycled braille glyphs from a JS
// timer and drew a FROZEN glyph when prefers-reduced-motion matched — on
// iPads (iOS turns Reduce Motion on for several accessibility features)
// the "thinking" icon therefore displayed but never moved and read as
// broken. CSS keyframes need no timers, no exotic glyphs, and under
// reduced motion the dots fade softly instead of going dead still.
function spinnerNode(st) {
  const span = document.createElement("span");
  span.className = "spinner";
  span.setAttribute("aria-hidden", "true");
  for (let i = 0; i < 3; i++) span.appendChild(document.createElement("i"));
  st.spinNode = span; // stopSpinner removes it from the DOM
  return span;
}

// stopSpinner unmounts the dots. Called on the first streamed answer token
// (the dots only mean "waiting") and on every exit path, so a stale bubble
// can never keep spinning. The animation lives and dies with the node.
function stopSpinner(st) {
  if (!st) return;
  if (st.spinNode) {
    st.spinNode.remove();
    st.spinNode = 0;
  }
}

function streamNode() {
  const el = clone("tpl-message");
  el.classList.add("from-assistant");
  el.querySelector(".msg-role").textContent = "Gabkeep";
  const time = el.querySelector("time");
  const now = new Date();
  time.dateTime = now.toISOString();
  time.textContent = now.toLocaleString();
  el.querySelector(".msg-body").classList.add("streaming");
  return el;
}

// Collapsed-by-default chain-of-thought disclosure (only when the server
// streams reasoning at all — llm.show_reasoning, spec §6.2).
function ensureThinking(st) {
  if (st.nodes.think) return;
  const d = document.createElement("details");
  d.className = "thinking";
  d.open = true;
  const summary = document.createElement("summary");
  summary.textContent = "Thinking…";
  const pre = document.createElement("pre");
  d.append(summary, pre);
  st.nodes.body.parentNode.insertBefore(d, st.nodes.body);
  st.nodes.think = d;
  st.nodes.thinkPre = pre;
  st.nodes.thinkSummary = summary;
}

// --- tool cards (spec §7, §10.2) ------------------------------------------------
// One card per model-proposed tool call, attached to the assistant
// <article> — deliberately OUTSIDE .msg-body, which finishGeneration
// clears when the markdown answer lands. Built live from the
// tool_start/tool_args/tool_end/awaiting_approval SSE events, and from
// the persisted tool_calls projection on reload.

function toolCardsHost(el) {
  let host = el.querySelector(".tool-cards");
  if (!host) {
    host = document.createElement("div");
    host.className = "tool-cards";
    el.querySelector(".msg-body").before(host);
  }
  return host;
}

const TOOL_STATE_LABELS = {
  ok: "done",
  denied: "denied",
  error: "failed",
  running: "interrupted",
  approved: "ran",
  pending_approval: "approval expired",
};

function toolStateLabel(status) {
  if (status === "ok") return "done";
  if (status === "denied") return "denied";
  if (status === "error") return "failed";
  return TOOL_STATE_LABELS[status] || status || "unknown";
}

function toolCardNode(callId, tool) {
  const d = document.createElement("details");
  d.className = "tool-card st-running";
  d.dataset.call = callId || "";
  const sum = document.createElement("summary");
  const name = document.createElement("span");
  name.className = "tool-name";
  name.textContent = tool || "tool";
  const state = document.createElement("span");
  state.className = "tool-state";
  state.textContent = "running…";
  sum.append(name, state);
  const args = document.createElement("pre");
  args.className = "tool-args";
  args.hidden = true;
  const results = document.createElement("div");
  results.className = "tool-results";
  d.append(sum, args, results);
  return d;
}

function formatToolArgs(raw) {
  let s = String(raw || "");
  try {
    s = JSON.stringify(JSON.parse(s)); // canonical one-liner when complete
  } catch {
    /* partial JSON mid-stream: show as it came in */
  }
  const runes = Array.from(s);
  return runes.length > 600 ? runes.slice(0, 600).join("") + "…" : s;
}

// safeLink is the only way a tool result URL reaches an <a href>: anything
// that is not http(s) (javascript:, data:, …) is dropped.
function safeLink(u) {
  if (typeof u !== "string") return null;
  try {
    const url = new URL(u, location.origin);
    if (url.protocol !== "http:" && url.protocol !== "https:") return null;
    return url.href;
  } catch {
    return null;
  }
}

function renderToolResults(container, data) {
  // web_search → [{title,url,snippet,age}]; web_fetch → {url,title}.
  const items = Array.isArray(data) ? data : data && data.url ? [data] : [];
  for (const it of items) {
    const href = it && typeof it.url === "string" ? safeLink(it.url) : null;
    if (!href) continue;
    const a = document.createElement("a");
    a.className = "tool-link";
    a.href = href;
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    const title = document.createElement("span");
    title.className = "tool-link-title";
    title.textContent = String(it.title || href).slice(0, 160);
    const host = document.createElement("span");
    host.className = "tool-link-host";
    host.textContent = new URL(href).host;
    a.append(title, host);
    container.append(a);
  }
}

function setToolSummary(card, text) {
  if (!text) return;
  let p = card.querySelector(".tool-summary");
  if (!p) {
    p = document.createElement("p");
    p.className = "tool-summary";
    card.querySelector(".tool-results").before(p);
  }
  p.textContent = text;
}

function attachApproval(card, st, d) {
  card.classList.remove("st-running");
  card.classList.add("st-awaiting");
  card.open = true;
  card.querySelector(".tool-state").textContent = "needs approval";
  const argsEl = card.querySelector(".tool-args");
  argsEl.textContent = formatToolArgs(d.arguments);
  argsEl.hidden = false;
  if (card.querySelector(".approval-row")) return; // duplicate event: idempotent

  const row = document.createElement("div");
  row.className = "approval-row";
  const label = document.createElement("span");
  label.className = "approval-label";
  label.textContent = `Allow ${d.tool || "the tool"} to run?`;
  const allow = document.createElement("button");
  allow.type = "button";
  allow.className = "btn small primary approval-allow";
  allow.textContent = "Allow";
  const deny = document.createElement("button");
  deny.type = "button";
  deny.className = "btn small ghost approval-deny";
  deny.textContent = "Deny";
  // §6.1/§9.9: code_run offers "always allow in this conversation".
  let rememberBox = null;
  if (d.rememberable) {
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.className = "approval-remember-input";
    const txt = document.createElement("span");
    txt.textContent = " Always allow in this conversation";
    rememberBox = document.createElement("label");
    rememberBox.className = "approval-remember";
    rememberBox.append(cb, txt);
  }
  const decide = async (approve) => {
    for (const b of row.querySelectorAll("button, input")) b.disabled = true;
    const remember = Boolean(rememberBox && rememberBox.querySelector("input").checked);
    try {
      await api.resolveApproval(st.genId, d.approval_id, approve, remember);
    } catch {
      toast("That approval is no longer pending.");
    }
  };
  allow.addEventListener("click", () => decide(true));
  deny.addEventListener("click", () => decide(false));
  row.append(label, allow, deny);
  if (rememberBox) row.append(rememberBox);
  card.append(row);
}

// toolCardFromAudit rebuilds a card from the persisted projection
// (messageJSON.tool_calls) when a conversation is loaded fresh.
function toolCardFromAudit(c) {
  const card = toolCardNode(c.id, c.tool);
  card.classList.remove("st-running");
  card.classList.add("st-" + (c.status || "ok"));
  card.querySelector(".tool-state").textContent = toolStateLabel(c.status);
  if (c.arguments) {
    const argsEl = card.querySelector(".tool-args");
    argsEl.textContent = formatToolArgs(c.arguments);
    argsEl.hidden = false;
  }
  let parsed = null;
  try {
    parsed = JSON.parse(c.result);
  } catch {
    /* plain text or empty result */
  }
  if (parsed && parsed.error) setToolSummary(card, String(parsed.error));
  if (parsed && typeof parsed.exit_code === "number") {
    // code_run: the summary is not stored, but the result JSON carries
    // enough to rebuild it ("exit 0" / "timed out (killed)").
    let s = parsed.timed_out ? "timed out (killed)" : "exit " + parsed.exit_code;
    if (parsed.truncated) s += " · output truncated";
    setToolSummary(card, s);
  }
  if (parsed && Array.isArray(parsed.results)) {
    renderToolResults(card.querySelector(".tool-results"), parsed.results);
  } else if (parsed && typeof parsed.url === "string") {
    renderToolResults(card.querySelector(".tool-results"), parsed);
  }
  return card;
}

// --- attachments, artifact cards & files view (spec §10.2, M5) -------------------

function humanSize(n) {
  if (typeof n !== "number" || !isFinite(n) || n < 0) return "";
  if (n >= 1 << 30) return (n / 2 ** 30).toFixed(1) + " GiB";
  if (n >= 1 << 20) return (n / 2 ** 20).toFixed(1) + " MiB";
  if (n >= 1 << 10) return (n / 2 ** 10).toFixed(1) + " KiB";
  return n + " B";
}

// File URLs are ONLY ever our own API paths; anything else (a hostile tool
// result included) never reaches an href.
function fileApiUrl(u) {
  return typeof u === "string" && u.startsWith("/api/v1/files/") && !/[\s"']/.test(u) ? u : null;
}

function fileChip(f) {
  const href = fileApiUrl(f.download_url || f.url);
  if (!href) return null;
  const a = document.createElement("a");
  a.className = "file-chip";
  a.href = href;
  a.download = f.name || "";
  const name = document.createElement("span");
  name.className = "chip-name";
  name.textContent = f.name || "file";
  const size = document.createElement("span");
  size.className = "chip-size";
  size.textContent = humanSize(f.size ?? f.bytes);
  a.append(name, size);
  return a;
}

function appendFileChips(msgEl, files) {
  const host = document.createElement("div");
  host.className = "msg-files";
  for (const f of files) {
    const chip = fileChip(f);
    if (chip) host.append(chip);
  }
  if (host.childElementCount) msgEl.querySelector(".msg-body").after(host);
}

function artifactCard(file) {
  const href = fileApiUrl(file.url);
  if (!href) return null;
  const card = document.createElement("div");
  card.className = "artifact-card";
  const icon = document.createElement("span");
  icon.className = "artifact-icon";
  icon.setAttribute("aria-hidden", "true");
  icon.textContent = "📄";
  const meta = document.createElement("div");
  meta.className = "artifact-meta";
  const name = document.createElement("div");
  name.className = "artifact-name";
  name.textContent = file.name || "download";
  const sub = document.createElement("div");
  sub.className = "artifact-sub muted";
  sub.textContent = [humanSize(file.bytes), file.note].filter(Boolean).join(" — ");
  meta.append(name, sub);
  const dl = document.createElement("a");
  dl.className = "btn small primary artifact-dl";
  dl.href = href;
  dl.download = file.name || "";
  dl.textContent = "Download";
  card.append(icon, meta, dl);
  return card;
}

// artifactFromAudit rebuilds the download card from the persisted
// files_share result when the conversation is loaded fresh.
function artifactFromAudit(c) {
  if (c.tool !== "files_share" || c.status !== "ok" || !c.result) return null;
  let parsed = null;
  try {
    parsed = JSON.parse(c.result);
  } catch {
    return null; // truncated mid-JSON: no card beats a wrong card
  }
  if (!parsed || parsed.error || typeof parsed.path !== "string") return null;
  return artifactCard({
    path: parsed.path, name: parsed.name, bytes: parsed.bytes,
    url: parsed.url, note: parsed.note,
  });
}

function renderPending() {
  const tray = $("#pending-files");
  tray.replaceChildren();
  for (const f of state.pending) {
    const chip = document.createElement("span");
    chip.className = "attach-chip" + (f.uploading ? " uploading" : "");
    const name = document.createElement("span");
    name.className = "chip-name";
    name.textContent = f.uploading ? `Uploading ${f.name}…` : f.name;
    const size = document.createElement("span");
    size.className = "chip-size";
    size.textContent = humanSize(f.size);
    chip.append(name, size);
    if (!f.uploading) {
      const rm = document.createElement("button");
      rm.type = "button";
      rm.className = "chip-remove";
      rm.setAttribute("aria-label", `Remove ${f.name}`);
      rm.textContent = "×";
      rm.addEventListener("click", () => {
        state.pending = state.pending.filter((x) => x !== f);
        renderPending();
      });
      chip.append(rm);
    }
    tray.append(chip);
  }
  show(tray, state.pending.length > 0);
}

async function onAttachPick(ev) {
  const files = Array.from(ev.target.files || []);
  ev.target.value = ""; // same file re-selectable next time
  const caps = (state.meta && state.meta.files) || {};
  const blocked = caps.blocked_extensions || [];
  for (const file of files) {
    const maxAtt = caps.max_attachments || 8;
    if (state.pending.length >= maxAtt) {
      toast(`At most ${maxAtt} attachments per message.`);
      break;
    }
    const dot = file.name.toLowerCase().lastIndexOf(".");
    if (dot >= 0 && blocked.includes(file.name.toLowerCase().slice(dot))) {
      toast(`This file type cannot be uploaded: ${file.name}`);
      continue;
    }
    if (caps.max_file_size && file.size > caps.max_file_size) {
      toast(`${file.name}: exceeds the ${humanSize(caps.max_file_size)} limit.`);
      continue;
    }
    const entry = { path: "", name: file.name, size: file.size, uploading: true };
    state.pending.push(entry);
    renderPending();
    try {
      const rec = await api.uploadFile(file);
      entry.path = rec.path;
      entry.uploading = false;
    } catch (err) {
      state.pending = state.pending.filter((x) => x !== entry);
      if (err instanceof ApiError && err.status === 401) {
        showAuth();
        return;
      }
      toast(`Upload failed (${file.name}): ${err.message}`);
    }
    renderPending();
  }
}

// --- files view --------------------------------------------------------------

function openFilesView() {
  if (state.filesOpen) return;
  if (state.settingsOpen) closeSettingsView(); // one overlay panel at a time
  state.filesOpen = true;
  document.body.classList.add("files-open");
  show($("#files-panel"), true);
  if (isPhone()) closeDrawer();
  loadFiles(true).catch((e) => handleErr(e, "files"));
}

function closeFilesView() {
  state.filesOpen = false;
  document.body.classList.remove("files-open");
  show($("#files-panel"), false);
}

// --- settings view (account details, default prompt, password) -------------------

function openSettingsView() {
  if (state.settingsOpen) return;
  if (state.filesOpen) closeFilesView(); // one overlay panel at a time
  state.settingsOpen = true;
  document.body.classList.add("settings-open");
  fillSettingsForm();
  show($("#settings-panel"), true);
  if (isPhone()) closeDrawer();
}

function closeSettingsView() {
  state.settingsOpen = false;
  document.body.classList.remove("settings-open");
  show($("#settings-panel"), false);
}

// The form is always re-seeded from the server-truth copy of the user, so
// a half-typed edit never survives a re-open looking saved when it is not.
function fillSettingsForm() {
  const me = state.me || {};
  $("#settings-name").value = me.name || "";
  $("#settings-email").value = me.email || "";
  $("#settings-prompt").value = me.default_system_prompt || "";
  $("#form-password").reset();
}

// Both save buttons double as "done": on SUCCESS the panel closes and the
// chat the user was reading comes back (same navigation as the X button).
// A failed save deliberately keeps the panel open so nothing typed is lost.
function leaveSettings() {
  location.hash = state.currentId ? `#/c/${state.currentId}` : "";
}

async function onSettingsSave(ev) {
  ev.preventDefault();
  try {
    state.me = await api.updateMe({
      name: $("#settings-name").value.trim(),
      email: $("#settings-email").value.trim(),
      default_system_prompt: $("#settings-prompt").value.trim(),
    });
    $("#user-chip").textContent = state.me.name || state.me.email;
    leaveSettings();
    toast("Settings saved", false);
  } catch (err) {
    handleErr(err, "save settings");
  }
}

async function onPasswordSave(ev) {
  ev.preventDefault();
  const cur = $("#pw-current").value;
  const next = $("#pw-new").value;
  if (next !== $("#pw-confirm").value) {
    toast("New passwords do not match.");
    return;
  }
  try {
    await api.changePassword(cur, next);
    $("#form-password").reset();
    // The server rotated our session cookie (other sessions were revoked).
    leaveSettings();
    toast("Password updated", false);
  } catch (err) {
    handleErr(err, "change password");
  }
}

// Stale-response guard: loadFiles clears the list up front but appends
// AFTER the await, so two overlapping loads (tab switch while a list is in
// flight) used to merge into one list — the same file appearing twice.
// Only the newest load may touch the DOM.
let filesLoadSeq = 0;

// One endpoint for everything: the server physically walks uploads/,
// artifacts/ and work/, so there is no cursor and no second listing —
// each tab is one request (kind filter "" = all).
async function loadFiles(reset) {
  const gen = ++filesLoadSeq;
  if (reset) $("#files-list").replaceChildren();
  const data = await api.listFiles(state.filesKind);
  if (gen !== filesLoadSeq) return; // superseded by a newer load — drop it
  const list = $("#files-list");
  for (const f of data.items || []) list.appendChild(fileRow(f));
  if (data.truncated) {
    const li = document.createElement("li");
    li.className = "f-sub";
    li.textContent = "Only the most recent files are shown.";
    list.appendChild(li);
  }
  const q = data.quota || {};
  const used = q.used_bytes || 0;
  const total = q.total_bytes || 0;
  $("#quota-fill").style.width = (total > 0 ? Math.min(100, (used / total) * 100) : 0).toFixed(1) + "%";
  $("#quota-text").textContent = `${humanSize(used)} of ${humanSize(total)} used`;
}

function fileRow(f) {
  const li = document.createElement("li");
  li.className = "file-row";
  const isDir = f.kind === "dir";
  const main = document.createElement("div");
  main.className = "f-main";
  const name = document.createElement("div");
  name.className = "f-name";
  name.textContent = f.name;
  const sub = document.createElement("div");
  sub.className = "f-sub";
  const badge = document.createElement("span");
  badge.className = "kind-badge " + f.kind;
  badge.textContent = isDir ? "folder" : f.kind;
  const when = new Date(f.modified_at || f.created_at).toLocaleString();
  const bits = isDir
    ? [f.files > 0 ? `${f.files}${f.files_capped ? "+" : ""} file${f.files === 1 ? "" : "s"} inside` : "empty", when]
    : [humanSize(f.size), when];
  if (f.expires_at) bits.push(`expires ${new Date(f.expires_at).toLocaleDateString()}`);
  sub.append(badge, " " + bits.join(" · "));
  main.append(name, sub);
  if (f.path) {
    // The relative path is the point: two directories can hold the same
    // filename, and the bare name cannot tell them apart.
    const p = document.createElement("div");
    p.className = "f-path";
    p.textContent = f.path;
    main.append(p);
  }

  const dl = document.createElement("a");
  dl.className = "btn small";
  dl.href = fileApiUrl(f.download_url) || "#";
  if (fileApiUrl(f.download_url)) {
    dl.download = f.name;
  } else {
    dl.hidden = true; // folders have no bytes to download
  }
  dl.textContent = "Download";

  const del = document.createElement("button");
  del.type = "button";
  del.className = "btn small ghost danger-text";
  del.textContent = "Delete";
  del.addEventListener("click", async () => {
    // Directory deletes are recursive — spell out the full relative path
    // and what goes with it before anything is touched.
    const msg = isDir
      ? `Delete directory "${f.path}"?` +
        (f.files > 0
          ? ` Everything inside it — ${f.files}${f.files_capped ? "+" : ""} file(s) and any subdirectories — will be deleted too, including files shared as download cards.`
          : "") +
        " This cannot be undone."
      : `Delete "${f.name}"?`;
    if (!(await confirmDialog(msg))) return;
    del.disabled = true;
    try {
      // One delete for every kind — the path is the whole identity, and
      // directories go recursively server-side.
      await api.deleteFile(f.path);
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 404)) {
        del.disabled = false;
        handleErr(err, "delete file");
        return;
      }
      // Already gone server-side (a folder delete took it, or a script
      // cleaned up). The row is stale — refresh shows the truth instead
      // of an undeletable ghost.
    }
    // Reload the view rather than only removing this row: deleting a
    // folder deletes every listed path under it, and those rows must go
    // too (along with ancestor "N files inside" counts and the quota).
    try {
      await loadFiles(true);
    } catch (err2) {
      li.remove(); // delete itself succeeded — at least drop this row
      handleErr(err2, "files refresh");
    }
  });

  li.append(main, dl, del);
  return li;
}

/**
 * Run one assistant turn against `url` and stream it into the thread.
 * `rollback` undoes optimistic UI when the server rejects before starting;
 * `onAccepted` fires the moment the stream starts (attachments: the server
 * has taken ownership, empty the composer tray).
 */
async function startGeneration(url, body, rollback, onAccepted) {
  const controller = new AbortController();
  const st = {
    controller,
    genId: "",
    msgId: "",
    raw: "",
    reasoning: "",
    started: false,
    errorData: null,
    nodes: null,
    cards: {}, // call_id → { card, args } for the live tool cards
  };
  state.inflight = st;
  syncComposerButtons();
  try {
    await streamChat(
      url,
      body,
      {
        onStart(d) {
          st.started = true;
          st.genId = d.generation_id || "";
          st.msgId = d.message_id || "";
          if (onAccepted) onAccepted();
          const el = streamNode();
          $("#thread").append(el);
          st.nodes = { el, body: el.querySelector(".msg-body") };
          // Dedicated text node so tokens never clobber the container; the
          // spinner rides along until the first answer token lands, then
          // finishGeneration swaps everything for rendered markdown.
          st.nodes.text = document.createTextNode("");
          st.nodes.body.append(st.nodes.text, spinnerNode(st));
          maybeScroll(true);
        },
        onToken(t) {
          stopSpinner(st); // output is flowing: the "waiting" dots are done
          st.raw += t;
          if (st.nodes) {
            st.nodes.text.data = st.raw; // plain text while streaming
            maybeScroll();
          }
        },
        onReasoning(t) {
          st.reasoning += t;
          if (!state.meta || !state.meta.show_reasoning || !st.nodes) return;
          ensureThinking(st);
          st.nodes.thinkPre.textContent = st.reasoning;
          maybeScroll();
        },
        onToolStart(d) {
          if (!st.nodes) return;
          const card = toolCardNode(d.call_id, d.tool);
          toolCardsHost(st.nodes.el).append(card);
          st.cards[d.call_id] = { card, args: "" };
          maybeScroll();
        },
        onToolArgs(d) {
          const c = st.cards[d.call_id];
          if (!c) return;
          c.args += d.t || "";
          const pre = c.card.querySelector(".tool-args");
          pre.textContent = formatToolArgs(c.args);
          pre.hidden = false;
        },
        onToolEnd(d) {
          const c = st.cards[d.call_id];
          if (!c) return;
          c.card.classList.remove("st-running", "st-awaiting");
          c.card.classList.add("st-" + (d.status || "ok"));
          c.card.querySelector(".tool-state").textContent = toolStateLabel(d.status);
          const row = c.card.querySelector(".approval-row"); // answered: buttons expire
          if (row) row.remove();
          if (d.summary) setToolSummary(c.card, d.summary);
          if (d.data) renderToolResults(c.card.querySelector(".tool-results"), d.data);
          // files_share ⇒ a visible download card next to the card (§10.2).
          if (d.status === "ok" && d.tool === "files_share" && d.data && d.data.file) {
            const art = artifactCard(d.data.file);
            if (art) toolCardsHost(st.nodes.el).append(art);
          }
          maybeScroll();
        },
        onAwaitingApproval(d) {
          if (!st.nodes) return;
          let c = st.cards[d.call_id];
          if (!c) {
            const card = toolCardNode(d.call_id, d.tool);
            toolCardsHost(st.nodes.el).append(card);
            c = st.cards[d.call_id] = { card, args: d.arguments || "" };
          }
          attachApproval(c.card, st, d);
          maybeScroll(true);
        },
        onError(d) {
          st.errorData = d;
        },
        onTitle(d) {
          if (d.title) applyTitle(d.conversation_id || state.currentId, d.title);
        },
        onDone(d) {
          finishGeneration(st, d);
        },
      },
      controller.signal
    );
    // Upstream closed cleanly but never said done: treat as interrupted.
    if (state.inflight === st) finishGeneration(st, { status: "lost" });
  } catch (err) {
    if (err.name === "AbortError") {
      stopSpinner(st); // bubble may stay in the DOM: never spin forever
      return; // deliberately abandoned
    }
    if (err instanceof ApiError && err.status === 401) {
      state.inflight = null;
      syncComposerButtons();
      handleErr(err, "send");
      return;
    }
    if (st.started) {
      finishGeneration(st, { status: "lost" });
    } else {
      if (rollback) rollback();
      toast(err.message || "Could not start a reply.");
      state.inflight = null;
      syncComposerButtons();
    }
  }
}

function finishGeneration(st, done) {
  stopSpinner(st); // every exit path must end the animation
  if (state.inflight !== st) return; // abandoned mid-flight; bubble is gone
  state.inflight = null;
  syncComposerButtons();
  const nodes = st.nodes;
  if (!nodes) return;
  if (nodes.think) {
    nodes.think.open = false;
    nodes.thinkSummary.textContent = "Thought process";
  }
  nodes.body.classList.remove("streaming");
  nodes.body.classList.add("md");
  nodes.body.replaceChildren(renderMarkdown(st.raw)); // full markdown once done
  // Any approval prompt still on screen is dead: the loop moved on
  // (timeout, deny or abort). Retire the buttons.
  for (const ctl of nodes.el.querySelectorAll(".approval-row button, .approval-row input")) ctl.disabled = true;
  const status = done.status || "";
  if (status === "aborted") {
    showStatusNote(nodes.el, "Stopped.");
  } else if (status === "error") {
    showStatusNote(
      nodes.el,
      (st.errorData && st.errorData.message) || "The model endpoint failed; use regenerate to retry."
    );
  } else if (status === "lost") {
    showStatusNote(nodes.el, "Connection lost — the reply was interrupted.");
  }
  nodes.el.dataset.id = done.message_id || st.msgId || "";
  refreshLastAssistant();
  if (nodes.el.dataset.id) attachRegen(nodes.el); // the button becomes visible now — make it work
  maybeScroll();
}

async function stopStream() {
  const st = state.inflight;
  if (!st) return;
  $("#btn-stop").disabled = true;
  try {
    // Server-side abort persists the partial answer and closes with done.
    await api.abortGeneration(st.genId);
  } catch {
    st.controller.abort(); // endpoint gone anyway: just detach the stream
  } finally {
    $("#btn-stop").disabled = false;
  }
}

// The regen button only exists on an assistant bubble once it is the
// .last-assistant (CSS), but the LISTENER must be attached wherever a
// bubble becomes regenerateable — both when loaded from the server
// (msgNode) and when a live generation completes (finishGeneration).
// Without the finishGeneration path the button on the just-streamed
// bubble was decorative: the first click (on a server-loaded bubble)
// worked, every later click did nothing. dataset.bound keeps double
// wiring impossible if a bubble is ever re-rendered in place.
function attachRegen(el) {
  const regen = el.querySelector(".regen-btn");
  if (!regen || regen.dataset.bound) return;
  regen.dataset.bound = "1";
  regen.addEventListener("click", () => regenerateFrom(el, el.dataset.id));
}

async function regenerateFrom(el, msgId) {
  if (state.inflight || !msgId) return;
  el.remove();
  await startGeneration(
    `/api/v1/messages/${encodeURIComponent(msgId)}/regenerate`,
    undefined,
    () => loadFirstMessages().catch((e) => handleErr(e, "reload messages"))
  );
}

// Stop the running generation (the UI thread is going away). Null the
// inflight state first so finishGeneration leaves the old bubbles alone.
async function abandonInflight() {
  const st = state.inflight;
  if (!st) return;
  stopSpinner(st); // the bubble is going away; don't spin on a detached node
  state.inflight = null;
  if (st.genId) {
    try {
      await api.abortGeneration(st.genId);
    } catch {
      /* already finished or gone */
    }
  }
  st.controller.abort();
  syncComposerButtons();
}

function renderConversation() {
  const c = state.currentId ? state.currentConv : null;
  $("#topbar-title").textContent = c ? c.title : "Gabkeep";
  const sel = $("#model-picker");
  sel.disabled = !state.currentId;
  if (c) sel.value = c.model;

  const input = $("#composer-input");
  input.disabled = !state.currentId;
  input.placeholder = state.currentId
    ? "Message Gabkeep… (Enter sends, Shift+Enter newline)"
    : "Start or pick a chat first";
  if (!state.currentId) input.value = "";
  syncComposerButtons();

  if (!state.currentId) {
    state.currentConv = null;
    $("#thread").replaceChildren(initialEmptyState());
  }
}

function initialEmptyState() {
  const el = document.createElement("div");
  el.className = "empty-state";
  const h = document.createElement("h2");
  h.textContent = "Gabkeep";
  const p1 = document.createElement("p");
  p1.textContent = "Pick a conversation on the left or start a new one.";
  const p2 = document.createElement("p");
  p2.className = "muted";
  p2.textContent = "Everything stays on this server, yours alone.";
  el.append(h, p1, p2);
  return el;
}

// --- new chat / model picker -------------------------------------------------------------

async function newChat() {
  await abandonInflight();
  try {
    const c = await api.createConversation("");
    state.convs.unshift(c);
    const list = $("#conv-list");
    list.prepend(convRow(c));
    markActive();
    // Open directly (awaited) so the composer is enabled and focused before
    // the hashchange round-trip; the later handler call is a no-op.
    state.focusOnOpen = true;
    await selectConversation(c.id);
    location.hash = `#/c/${c.id}`;
    if (isPhone()) closeDrawer();
  } catch (err) {
    handleErr(err, "new chat");
  }
}

async function onModelChange(ev) {
  if (!state.currentId) return;
  const model = ev.target.value;
  try {
    state.currentConv = await api.patchConversation(state.currentId, { model });
    toast(`Model: ${model || "default"}`, false);
  } catch (err) {
    handleErr(err, "set model");
  }
}

// --- confirm dialog ------------------------------------------------------------------------

function confirmDialog(text) {
  const dlg = $("#confirm-dialog");
  $("#confirm-text").textContent = text;
  return new Promise((resolve) => {
    dlg.addEventListener("close", () => resolve(dlg.returnValue === "ok"), { once: true });
    dlg.showModal();
  });
}

// --- responsive drawer (spec §10.1: phone → slide-over) -------------------------------------

const mqPhone = window.matchMedia("(max-width: 1023.98px)");
const isPhone = () => mqPhone.matches;

function openDrawer() {
  document.body.classList.add("drawer-open");
  show($("#backdrop"), true);
  $("#btn-menu").setAttribute("aria-expanded", "true");
}

function closeDrawer() {
  document.body.classList.remove("drawer-open");
  show($("#backdrop"), false);
  $("#btn-menu").setAttribute("aria-expanded", "false");
}

// --- routing & wiring --------------------------------------------------------------------------

function onHashChange() {
  if ($("#view-chat").hidden) {
    routeAuthForm();
    return;
  }
  if (location.hash === "#/files") {
    openFilesView();
    return;
  }
  if (location.hash === "#/settings") {
    openSettingsView();
    return;
  }
  if (state.filesOpen) closeFilesView();
  if (state.settingsOpen) closeSettingsView();
  const m = location.hash.match(/^#\/c\/([A-Za-z0-9]{1,40})$/);
  if (m) {
    selectConversation(m[1]);
  } else if (state.currentId && location.hash === "") {
    abandonInflight();
    state.currentId = "";
    renderConversation();
  }
}

async function boot() {
  applyTheme();
  $("#btn-theme").addEventListener("click", cycleTheme);
  $("#form-login").addEventListener("submit", onLogin);
  $("#form-register").addEventListener("submit", onRegister);
  $("#link-register").addEventListener("click", (e) => {
    e.preventDefault();
    location.hash = "#register";
  });
  $("#link-login").addEventListener("click", (e) => {
    e.preventDefault();
    location.hash = "";
  });
  $("#btn-logout").addEventListener("click", onLogout);
  $("#btn-new-chat").addEventListener("click", newChat);
  $("#btn-more").addEventListener("click", () => loadMoreConversations().catch((e) => handleErr(e, "more chats")));
  $("#model-picker").addEventListener("change", onModelChange);
  $("#btn-menu").addEventListener("click", () =>
    document.body.classList.contains("drawer-open") ? closeDrawer() : openDrawer()
  );
  $("#backdrop").addEventListener("click", closeDrawer);
  $("#btn-send").addEventListener("click", () => sendFromComposer());
  $("#btn-stop").addEventListener("click", stopStream);
  $("#btn-attach").addEventListener("click", () => $("#attach-input").click());
  $("#attach-input").addEventListener("change", onAttachPick);
  $("#btn-files").addEventListener("click", () => {
    location.hash = "#/files";
  });
  $("#btn-files-close").addEventListener("click", () => {
    location.hash = state.currentId ? `#/c/${state.currentId}` : "";
  });
  $("#btn-settings").addEventListener("click", () => {
    location.hash = "#/settings";
  });
  $("#btn-settings-close").addEventListener("click", () => {
    location.hash = state.currentId ? `#/c/${state.currentId}` : "";
  });
  $("#form-profile").addEventListener("submit", onSettingsSave);
  $("#form-password").addEventListener("submit", onPasswordSave);
  for (const fb of document.querySelectorAll(".files-filter")) {
    fb.addEventListener("click", () => {
      state.filesKind = fb.dataset.kind || "";
      for (const b of document.querySelectorAll(".files-filter")) {
        b.classList.toggle("active", b === fb);
      }
      loadFiles(true).catch((e) => handleErr(e, "files"));
    });
  }
  $("#composer-input").addEventListener("input", autosizeComposer);
  $("#composer-input").addEventListener("keydown", (ev) => {
    // Enter sends, Shift+Enter inserts a newline; IME composition excluded.
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      sendFromComposer();
    }
  });
  $("#conv-search").addEventListener(
    "input",
    debounce((ev) => {
      state.query = ev.target.value.trim();
      loadConversations().catch((e) => handleErr(e, "search"));
    }, 250)
  );
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && document.body.classList.contains("drawer-open")) {
      closeDrawer();
    }
  });
  window.addEventListener("hashchange", onHashChange);

  try {
    state.meta = await api.meta();
  } catch {
    state.meta = { registration: "open", remember_login: true, models: [] };
  }
  applyMetaFlags();

  try {
    const me = await api.me();
    enterChat(me);
  } catch (err) {
    if (!(err instanceof ApiError) || err.status !== 401) {
      toast(`Cannot reach the server: ${err.message}`);
    }
    showAuth();
  }
}

boot();
