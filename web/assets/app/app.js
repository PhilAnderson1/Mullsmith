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
  currentConv: null,
  messagesCursor: "",
  inflight: null, // live generation: { controller, genId, msgId, raw, ... }
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
  show($("#view-chat"), false);
  show($("#view-auth"), true);
  routeAuthForm();
  $("#login-email").focus();
}

function enterChat(user) {
  state.me = user;
  show($("#view-auth"), false);
  show($("#view-chat"), true);
  $("#user-chip").textContent = user.name || user.email;
  if (location.hash && !location.hash.startsWith("#/c/")) {
    location.hash = ""; // drop #login/#register leftovers
  }
  if (location.hash.startsWith("#/c/")) {
    // Let the hash drive the initial selection.
    onHashChange();
  }
  if (!state.currentId) {
    location.hash = "";
    renderConversation();
  }
  loadConversations().catch((e) => handleErr(e, "load conversations"));
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
    show($("#link-register"), false);
  } else if (m.registration === "invite-only") {
    show($("#auth-mode-note"), true);
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

  if (m.status === "aborted") {
    showStatusNote(el, "Stopped before finishing.");
  } else if (m.status === "error") {
    showStatusNote(el, "The model endpoint failed for this reply.");
  } else if (m.status && m.status !== "complete") {
    showStatusNote(el, `status: ${m.status}`);
  }

  const regen = el.querySelector(".regen-btn");
  if (regen && m.role === "assistant" && m.id) {
    regen.addEventListener("click", () => regenerateFrom(el, m.id));
  }
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
  try {
    state.currentConv = await api.getConversation(id);
    state.currentId = id;
    state.messagesCursor = "";
    await loadFirstMessages();
    markActive();
    renderConversation();
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
    for (const m of data.items) thread.appendChild(msgNode(m));
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
      for (const m of data.items) host.insertBefore(msgNode(m), ref);
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

function autosizeComposer() {
  const el = $("#composer-input");
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, Math.round(window.innerHeight * 0.3)) + "px";
}

// The composer entry point: optimistic user bubble, then the streamed reply.
// The rollback restores the draft if the server rejects before starting.
async function sendFromComposer() {
  const input = $("#composer-input");
  const text = input.value;
  if (!state.currentId || state.inflight || !text.trim()) return;
  const placeholder = $("#thread .empty-state");
  if (placeholder) placeholder.remove();
  const userEl = appendUserBubble(text.trim());
  input.value = "";
  autosizeComposer();
  await startGeneration(
    `/api/v1/conversations/${encodeURIComponent(state.currentId)}/messages`,
    { content: text.trim() },
    () => {
      userEl.remove();
      input.value = text;
      autosizeComposer();
      input.focus();
    }
  );
}

function appendUserBubble(text) {
  const el = msgNode({
    role: "user",
    content: text,
    status: "complete",
    created_at: new Date().toISOString(),
  });
  $("#thread").append(el);
  maybeScroll(true);
  return el;
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

/**
 * Run one assistant turn against `url` and stream it into the thread.
 * `rollback` undoes optimistic UI when the server rejects before starting.
 */
async function startGeneration(url, body, rollback) {
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
          const el = streamNode();
          $("#thread").append(el);
          st.nodes = { el, body: el.querySelector(".msg-body") };
          maybeScroll(true);
        },
        onToken(t) {
          st.raw += t;
          if (st.nodes) {
            st.nodes.body.textContent = st.raw; // plain text while streaming
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
        onError(d) {
          st.errorData = d;
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
    if (err.name === "AbortError") return; // deliberately abandoned
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
