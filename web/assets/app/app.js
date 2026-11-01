// Mullsmith SPA — M3: auth, conversations and live streamed chat.
// Tools and files arrive in M4/M5. No framework, no build step (spec §10).
// DOM helpers are local; views are plain functions over a single state object.

import { api, ApiError } from "/assets/app/api.js";
import { streamChat } from "/assets/app/sse.js";
import { renderMarkdown } from "/assets/app/markdown.js";
import { LatestWins } from "/assets/app/ownership.js";
// C7: the live view and the reloaded history render cards, statuses and
// rounds through the SAME projection functions.
import {
  callFromEvent, callFromStored, buildCallCard, paintCallOutcome, toolCardNode,
  toolCardsHost, formatToolArgs, sharedFileFor, roundStatusNote,
} from "/assets/app/thread.js";

const state = {
  meta: null,
  me: null,
  convs: [],
  convCursor: "",
  query: "",
  currentId: "",
  // Navigation target with a loader still in flight (C5): the destination
  // of a selectConversation whose metadata or first page has not landed.
  // currentId stays on the fully rendered thread until both arrive, so a
  // repeated intent can be recognised by this marker alone.
  navPendingId: "",
  // Conversation whose history is being RECONCILED after an uncertain loss
  // (review3 finding 4): Send stays disabled and the send/regenerate paths
  // stay shut until that reload settles, so a second send cannot complete
  // underneath an older snapshot that is still on the wire.
  reconcileId: "",
  // Thread-activity counter (review3 finding 4): bumped whenever the client
  // adds content the server snapshot may not contain (optimistic bubble,
  // stream start, finished answer). A history page fetched before a bump
  // must not replace what that bump made.
  threadRev: 0,
  // one-shot: set when entering chat / new chat so selectConversation focuses
  // the composer once the conversation is actually open (spec §10 UX).
  focusOnOpen: false,
  currentConv: null,
  inflight: null, // live generation: { controller, genId, msgId, raw, ... }
  // uploads staged in the composer, sent as attachment_paths with the message
  pending: [], // { path, download_url, name, size, uploading }
  filesOpen: false,
  filesKind: "",
  settingsOpen: false,
  // email_features (plan §6): the token currently being acted on, the
  // address whose "check your inbox" panel is showing ("" = off), and the
  // confirm token already spent — auto-submission must fire exactly once.
  emailToken: "",
  pendingInbox: "",
  confirmDone: "",
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
  syncThemeColor(pref);
}

// syncThemeColor keeps the OS status/nav bar (theme-color meta, index.html)
// matched to the theme actually painted. The static metas already cover the
// "system" preference; this matters when the user pins light or dark. The
// two hexes mirror --bg in styles.css.
function syncThemeColor(pref) {
  const dark = pref === "dark" ||
    (pref !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  for (const m of document.querySelectorAll('meta[name="theme-color"]')) {
    m.content = dark ? "#14161b" : "#f7f8fa";
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

// --- request ownership (finding 1) ------------------------------------------------
//
// navOwner sequences conversation navigation (opening a thread, loading its
// first page); listOwner sequences sidebar list loads. Rule, already proven
// by loadFiles' sequence guard: capture the token BEFORE the first await,
// and only the youngest token's work may commit shared state or paint.
// Sign-out bumps both — a response that lands after logout must not paint
// the auth screen with the previous identity's data, nor race the next one.
const navOwner = new LatestWins();
const listOwner = new LatestWins();

// --- view switching ------------------------------------------------------------

function showAuth() {
  abandonInflight();
  navOwner.bump(); // nothing outstanding belongs to whoever signs in next
  listOwner.bump();
  state.navPendingId = ""; // the pending navigation dies with this identity (C5)
  state.reconcileId = ""; // and so does an in-flight recovery (finding 4)
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

// The auth card holds exactly one visible form; the rest of the API surface
// for email features (#forgot, token-carrying #reset?t= / #confirm?t=, the
// post-signup inbox panel) routes through the same hash mechanism as
// #login/#register do.
const AUTH_FORMS = [
  "form-login", "form-register", "form-forgot", "form-reset",
  "form-confirm", "register-confirm-note",
];

function showAuthForm(id) {
  for (const f of AUTH_FORMS) show($("#" + f), f === id);
  // Any switch clears the previous success note; the handlers that set one
  // display it AFTER routing (scrub → route → note), never before.
  show($("#auth-done-note"), false);
  if (id !== "form-forgot") show($("#forgot-ok"), false);
  if (id === "form-forgot") show($("#forgot-input-wrap"), true);
}

// The mail link arrives as https://host/#reset?t=<token>: one query string
// hanging off the fragment, parsed with URLSearchParams.
function hashToken() {
  const i = location.hash.indexOf("?t=");
  if (i < 0) return "";
  return new URLSearchParams(location.hash.slice(i + 1)).get("t") || "";
}

// Consumed tokens must not linger in the URL or history. replaceState never
// fires hashchange, so routing is re-run by hand after the scrub.
function scrubHash() {
  state.emailToken = "";
  history.replaceState(null, "", location.pathname + location.search);
  routeAuthForm();
}

// Which mail-link route (if any) the current hash carries. Shared by the
// auth-view router and the live-session handler below.
function emailTokenRoute() {
  if (!(state.meta && state.meta.email_features)) return "";
  if (location.hash.startsWith("#confirm")) return "confirm";
  if (location.hash.startsWith("#reset")) return "reset";
  return "";
}

function routeAuthForm() {
  const emailOn = !!(state.meta && state.meta.email_features);
  const h = location.hash;
  if (emailOn && h === "#forgot") {
    showAuthForm("form-forgot");
    return;
  }
  if (emailOn && h.startsWith("#reset")) {
    const tok = hashToken();
    if (!tok) { location.hash = ""; return; }
    state.emailToken = tok;
    showAuthForm("form-reset");
    return;
  }
  if (emailOn && h.startsWith("#confirm")) {
    const tok = hashToken();
    if (!tok || tok === state.confirmDone) { showAuthForm("form-login"); return; }
    showAuthForm("form-confirm");
    runConfirmEmail(tok);
    return;
  }
  if (state.pendingInbox) { showAuthForm("register-confirm-note"); return; }
  const wantRegister = h === "#register" && state.meta.registration !== "closed";
  showAuthForm(wantRegister ? "form-register" : "form-login");
}

async function runConfirmEmail(tok) {
  state.confirmDone = tok; // one attempt per token, dead or alive
  show($("#confirm-error"), false);
  $("#confirm-note").textContent = "Checking your confirmation link…";
  show($("#confirm-note"), true);
  try {
    await api.confirmEmail(tok);
    state.pendingInbox = "";
    history.replaceState(null, "", location.pathname + location.search);
    state.emailToken = "";
    routeAuthForm(); // token is gone from the hash: lands on login
    $("#auth-done-note").textContent = "Email confirmed — sign in.";
    show($("#auth-done-note"), true);
  } catch (err) {
    // Stay on this (already visible) form so the verdict is readable; the
    // token still disappears from the URL.
    history.replaceState(null, "", location.pathname + location.search);
    state.emailToken = "";
    $("#confirm-note").textContent = "This link no longer works — it was already used, expired, or the pending address changed since.";
    const errEl = $("#confirm-error");
    errEl.textContent = err.message;
    show(errEl, true);
  }
}

// A mail link landing on an ALREADY-RUNNING, logged-in session — the normal
// path for Settings address confirmations (you are signed in when you
// request them) and for reset links clicked in the same browser. Without
// this, enterChat()/the chat router would wipe #confirm?t=… as a #login
// leftover and the confirmation would never be sent — the field bug this
// guards. Returns null (nothing to do), "auth" (auth view took over), or a
// fresh user object when a confirmation applied.
async function consumeEmailToken() {
  const route = emailTokenRoute();
  if (!route) return null;
  const tok = hashToken();
  if (!tok) {
    history.replaceState(null, "", location.pathname + location.search);
    return null;
  }
  if (route === "reset") {
    // Completing a reset kills every session anyway; hand the screen to
    // the real form while the token hash is still visible to the router.
    showAuth();
    return "auth";
  }
  try {
    await api.confirmEmail(tok);
    const fresh = await api.me();
    history.replaceState(null, "", location.pathname + location.search);
    toast("Email address confirmed — " + fresh.email + " is now your sign-in address.", false);
    return fresh;
  } catch (err) {
    history.replaceState(null, "", location.pathname + location.search);
    toast("Confirming the email address failed: " + err.message, true);
    return null;
  }
}

async function onForgot(ev) {
  ev.preventDefault();
  const errEl = $("#forgot-error");
  show(errEl, false);
  try {
    await api.forgotPassword($("#forgot-email").value.trim());
    // The server answers one generic 200; this copy mirrors it verbatim.
    show($("#forgot-input-wrap"), false);
    show($("#forgot-ok"), true);
  } catch (err) {
    errEl.textContent = err.message;
    show(errEl, true);
  }
}

async function onResetSubmit(ev) {
  ev.preventDefault();
  const errEl = $("#reset-error");
  show(errEl, false);
  const pw = $("#reset-password").value;
  if (pw !== $("#reset-confirm").value) {
    errEl.textContent = "New passwords do not match.";
    show(errEl, true);
    return;
  }
  try {
    await api.resetPassword(state.emailToken, pw);
    $("#form-reset").reset();
    scrubHash(); // lands on the login form
    $("#auth-done-note").textContent = "Password updated — sign in with the new one. Every device was signed out.";
    show($("#auth-done-note"), true);
  } catch (err) {
    errEl.textContent = err.message;
    show(errEl, true);
  }
}

async function onResendConfirm() {
  const errEl = $("#resend-error");
  show(errEl, false);
  try {
    await api.resendConfirm(state.pendingInbox);
    toast("Confirmation mail sent again — check your inbox.", false);
  } catch (err) {
    errEl.textContent = err.message;
    show(errEl, true);
  }
}

async function onCancelPendingEmail() {
  try {
    state.me = await api.cancelEmail();
    fillSettingsForm();
    toast("Pending address change cancelled", false);
  } catch (err) {
    handleErr(err, "cancel email change");
  }
}

// The irreversible one: the shared dialog is the required confirmation
// (same pattern as deleting a conversation, weightier words).
async function onDeleteAccount() {
  const ok = await confirmDialog(
    "Delete your account permanently? Every chat, file and setting is erased now — this cannot be undone.");
  if (!ok) return;
  try {
    await api.deleteAccount();
  } catch (err) {
    handleErr(err, "delete account");
    return;
  }
  // The row, sessions, chats and scratchpad are gone server-side; blank
  // everything that carried this identity. showAuth abandons any live
  // stream, hides the panels and lands on the sign-in form.
  state.convs = [];
  state.convCursor = "";
  $("#conv-list").replaceChildren();
  showAuth();
  toast("Account deleted. Goodbye.", false);
}

function applyMetaFlags() {
  const m = state.meta;
  show($("#login-remember-row"), m.remember_login);
  // Registration only hands out a session when email confirmation is off —
  // with email_features on, sign-up ends at the check-inbox panel, so the
  // remember-me checkbox would promise something that cannot happen yet
  // (it reappears on login, where it does apply).
  show($("#register-remember-row"), m.remember_login && !m.email_features);
  show($("#register-invite-wrap"), m.registration === "invite-only");
  show($("#login-forgot"), !!m.email_features);
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
  // Concrete model names only — no placeholder entry: every
  // conversation's selection resolves to a real one (default_model covers
  // those that never chose explicitly). The Set dedupes the common case
  // where models already contains the default.
  for (const value of new Set([m.default_model, ...m.models].filter(Boolean))) {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = value;
    sel.appendChild(opt);
  }
  populateThinkingChoices();
}

async function onLogin(ev) {
  ev.preventDefault();
  show($("#auth-done-note"), false);
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
    if (me && me.email_confirmation_required) {
      // Pending sign-up (plan §6): swap to the check-inbox panel instead
      // of entering chat — there is no session and nothing to enter yet.
      state.pendingInbox = me.email || $("#register-email").value.trim();
      $("#confirm-note-email").textContent = state.pendingInbox;
      if (location.hash === "#register") location.hash = "";
      routeAuthForm();
      return;
    }
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
    // The long-press echo (see strayClickGuardUntil) must not open the chat.
    if (Date.now() < strayClickGuardUntil) return;
    // Typing right away is the point of opening a chat: the one-shot makes
    // selectConversation focus the composer once it has loaded (skipped on
    // phones, and on re-clicks of the open chat, which fire no hashchange —
    // an unconsumed flag would focus unexpectedly on the next navigation).
    if (c.id !== state.currentId) state.focusOnOpen = true;
    location.hash = `#/c/${c.id}`;
    if (isPhone()) closeDrawer();
  });

  const form = li.querySelector(".conv-rename");
  const input = form.querySelector("input");
  const openBtn = li.querySelector(".conv-open");
  const menuBtn = li.querySelector(".conv-menu-btn");
  const startRename = () => {
    input.value = c.title;
    show(openBtn, false);
    show(menuBtn, false);
    show(form, true);
    input.focus();
    input.select();
  };
  const cancelRename = () => {
    show(form, false);
    show(openBtn, true);
    show(menuBtn, true);
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

  // Delete lives here, not behind a DOM button: the row menu drives this
  // one closure over the conversation object.
  const confirmDelete = async () => {
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
  };

  menuBtn.addEventListener("click", () => {
    // A simple toggle: the second press closes rather than re-summons.
    if (rowMenuTarget && rowMenuTarget.li === li && !$("#conv-row-menu").hidden) closeConvRowMenu();
    else openConvRowMenu({ li, rename: startRename, del: confirmDelete });
  });
  // Long press summons the row menu on touch devices. iOS browsers never
  // fire contextmenu (Android's does, inconsistently), so a touch timer
  // covers every phone and tablet; CSS callout suppression (styles.css)
  // keeps the OS copy-bubble away. Desktop keeps the "…" button.
  let pressTimer = null;
  let pressFired = false;
  let pressPos = null;
  li.addEventListener("touchstart", (ev) => {
    if (ev.touches.length !== 1 || ev.target.closest(".conv-menu-btn, .conv-rename")) return;
    const t = ev.touches[0];
    pressPos = { clientX: t.clientX, clientY: t.clientY };
    pressFired = false;
    clearTimeout(pressTimer);
    pressTimer = setTimeout(() => {
      pressFired = true;
      openConvRowMenu({ li, rename: startRename, del: confirmDelete }, pressPos);
    }, 500);
  }, { passive: true });
  li.addEventListener("touchmove", (ev) => {
    const t = ev.touches[0];
    if (!t || !pressPos) return;
    // A sliding finger means scrolling, not summoning.
    if (Math.abs(t.clientX - pressPos.clientX) > 10 || Math.abs(t.clientY - pressPos.clientY) > 10) {
      clearTimeout(pressTimer);
    }
  }, { passive: true });
  li.addEventListener("touchend", (ev) => {
    clearTimeout(pressTimer);
    if (pressFired) {
      ev.preventDefault(); // swallow the tap the held finger would also send
      pressFired = false;
      strayClickGuardUntil = Date.now() + 700; // …and its home-screen echo
    }
  });
  li.addEventListener("touchcancel", () => {
    clearTimeout(pressTimer);
    pressFired = false;
  });
  // Belt and braces: silence any native long-press menu the browser still
  // tries to raise over ours. No browser gesture opens the row menu itself.
  li.addEventListener("contextmenu", (ev) => ev.preventDefault());

  return li;
}

// The sidebar row's action menu: the "…" button summons it at the row,
// long press at the finger. One row at a
// time; the row hands over its own action closures, so the menu knows
// nothing about titles or the delete flow.
let rowMenuTarget = null;
// Home-screen (standalone) WebKit can still synthesise a click after a
// long press even though touchend called preventDefault — in-app-browser
// it stays away, in home-screen mode it does not, and that stray click
// lands inside the list and closes the menu instantly. For a short window
// after the gesture, clicks inside the list are ignored (deliberate taps
// elsewhere, or on the menu itself, still work).
let strayClickGuardUntil = 0;

function openConvRowMenu(target, ev) {
  closeConvRowMenu();
  rowMenuTarget = target;
  const menu = $("#conv-row-menu");
  show(menu, true);
  const pad = 8;
  const w = menu.offsetWidth, h = menu.offsetHeight;
  let x = ev ? ev.clientX : target.li.getBoundingClientRect().right - 8;
  // Long press: the menu sits above the finger — below, the fingertip would
  // hide it. The clamp below pushes it back down when the headroom is gone.
  let y = ev ? ev.clientY - h - 12 : target.li.getBoundingClientRect().bottom + 4;
  x = Math.min(Math.max(x, pad), window.innerWidth - w - pad);
  y = Math.min(Math.max(y, pad), window.innerHeight - h - pad);
  menu.style.left = x + "px";
  menu.style.top = y + "px";
}

function closeConvRowMenu() {
  rowMenuTarget = null;
  show($("#conv-row-menu"), false);
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

// promoteConversation is a LOCAL mirror of server-side ordering: posting a
// message bumps the conversation's updated_at (store AddMessage), so the
// chat belongs at the top — reorder the list + DOM immediately instead of
// waiting for the next load. Opening a chat is NOT activity (owner rule):
// only posts move a conversation, so this is called from the send and
// regenerate paths, never from selectConversation.
function promoteConversation(id) {
  const i = state.convs.findIndex((c) => c.id === id);
  if (i > 0) {
    const [c] = state.convs.splice(i, 1);
    state.convs.unshift(c);
    const row = document.querySelector(`#conv-list .conv-item[data-id="${CSS.escape(id)}"]`);
    if (row) $("#conv-list").prepend(row);
  }
}

async function loadConversations() {
  const tok = listOwner.begin(); // newest search/list wins (finding 1)
  const data = await api.listConversations(state.query, "");
  if (!listOwner.isCurrent(tok)) return;
  state.convs = data.items;
  state.convCursor = data.next_cursor;
  const list = $("#conv-list");
  list.replaceChildren();
  for (const c of state.convs) list.appendChild(convRow(c));
  show($("#btn-more"), Boolean(state.convCursor));
}

// One page-load at a time, and JOINING the current list sequence rather
// than taking it over: a newer search supersedes this page when it lands,
// but the page never supersedes its own base load (its rows append to the
// list that load established).
let listMoreBusy = false;
async function loadMoreConversations() {
  if (!state.convCursor || listMoreBusy) return;
  const tok = listOwner.current();
  const cursor = state.convCursor;
  listMoreBusy = true;
  try {
    const data = await api.listConversations(state.query, cursor);
    if (!listOwner.isCurrent(tok)) return;
    state.convCursor = data.next_cursor;
    for (const c of data.items) {
      state.convs.push(c);
      $("#conv-list").appendChild(convRow(c));
    }
    show($("#btn-more"), Boolean(state.convCursor));
  } finally {
    listMoreBusy = false;
  }
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
  el.dataset.gen = m.generation_id || ""; // the turn this bubble belongs to (C7)
  el.querySelector(".msg-role").textContent = m.role === "user" ? "You" : "Mullsmith";
  const time = el.querySelector("time");
  time.dateTime = m.created_at;
  time.textContent = new Date(m.created_at).toLocaleString();
  // tok/s only rates text the user can read — tool-only turns stay silent.
  if (m.role === "assistant" && m.content && m.content.trim()) {
    tpsNote(el, m.completion_tokens, m.duration_ms);
  }

  const body = el.querySelector(".msg-body");
  // Tool/status UI lives outside this prose bubble. Empty or whitespace-only
  // assistant content must not leave a padded, bordered rectangle behind it.
  if (m.role === "assistant") body.hidden = !(m.content || "").trim();
  if (m.role === "assistant" && m.content) {
    // Same prose wrapper the live view streams into (.stream-text): the
    // views must be indistinguishable, cards and notes excluded.
    const prose = document.createElement("div");
    prose.className = "stream-text";
    prose.classList.add("md");
    prose.append(renderMarkdown(m.content)); // XSS-safe: DOM-only builder
    body.append(prose);
  } else {
    body.textContent = m.content;
  }

  // Tool cards from the audit projection (§10.2): collapsed by default,
  // placed above the answer like their live counterparts. A calls-only
  // assistant row has no prose: the empty body folds away.
  if (m.role === "assistant" && Array.isArray(m.tool_calls) && m.tool_calls.length) {
    const host = toolCardsHost(el);
    for (const c of m.tool_calls) {
      const view = callFromStored(c);
      host.append(buildCallCard(view, c.arguments));
      const file = sharedFileFor(view); // files_share ⇒ visible download card
      if (file) host.append(artifactCard(file));
    }
  }
  if (m.role === "user" && Array.isArray(m.files) && m.files.length) {
    appendFileChips(el, m.files);
  }

  const note = roundStatusNote(m.status, m.status_note);
  if (note) showStatusNote(el, note);

  if (m.role === "assistant") attachRegen(el);
  return el;
}

function showStatusNote(el, text) {
  const st = el.querySelector(".msg-status");
  st.textContent = text;
  show(st, true);
}

// --- context meter + per-message speed ---------------------------------------

// fmtInt groups thousands (32,768) in the browser's locale.
function fmtInt(n) {
  return Number(n).toLocaleString();
}

// fmtTokens always speaks thousands with a K suffix — 0.2K, 2.2K, 71.5K,
// 192K — because the K is what tells you the line counts tokens: a bare
// "2,237 (1%)" is an unexplained number. One decimal below 100K, whole
// above, and M past a million; the tooltip always carries the exact count.
function fmtTokens(n) {
  const [scale, suffix] = n >= 1e6 ? [1e6, "M"] : [1e3, "K"];
  const v = n / scale;
  const s = v >= 100 ? String(Math.round(v)) : v.toFixed(1).replace(/\.0$/, "");
  return s + suffix;
}

// updateCtxMeter fills the fullness line above the composer: the freshest
// assistant total (prompt+completion of the last reply, an estimate where
// the endpoint reported nothing) against the context window carried by
// the conversation itself — the window is a model property, so it travels
// with the chat, not with meta — "71.5K (37%)". With no known window it
// reduces to the count alone; with no usage figure at all there is
// nothing to say.
function updateCtxMeter(used) {
  const el = $("#ctx-meter");
  if (!el) return;
  if (used > 0) state.lastCtxUsed = used; // remembered for a model switch
  el.classList.remove("ctx-meter-full"); // a live figure replaces any refusal
  const max = (state.currentConv && state.currentConv.max_context_tokens) || 0;
  if (!(used > 0)) {
    el.hidden = true;
    return;
  }
  if (!max) {
    el.textContent = fmtTokens(used);
    el.title = "context tokens used — the endpoint reports no window";
  } else {
    const pct = Math.max(1, Math.round((used / max) * 100));
    el.textContent = `${fmtTokens(used)} (${pct}%)`;
    el.title = `${fmtInt(used)} of ${fmtInt(max)} context tokens`;
  }
  el.hidden = false;
}

// showCtxFull pins the server's "conversation is full" answer to the meter
// line: the refusal stands for this thread, so it should not fade out like
// a toast. Any later usage figure clears it (see updateCtxMeter).
function showCtxFull(message) {
  const el = $("#ctx-meter");
  if (!el) {
    toast(message);
    return;
  }
  el.textContent = message || "this conversation is full — start a new chat";
  el.title = "nothing was sent";
  el.classList.add("ctx-meter-full");
  el.hidden = false;
}

// lastContextUsed scans a page of messages (chronological) from the end for
// the assistant total that best represents how full the context is now.
function lastContextUsed(items) {
  for (let i = items.length - 1; i >= 0; i--) {
    const m = items[i];
    if (m.role === "assistant" && m.token_count > 0) return m.token_count;
  }
  return 0;
}

// msgTps is the reply's output tokens per second (completion ÷ model-round
// time, tool runs excluded), or null when either figure is missing — rows
// predating per-message timing stay silent rather than show a lie.
function msgTps(completion, durationMS) {
  if (!(completion > 0) || !(durationMS > 0)) return null;
  const tps = (completion / durationMS) * 1000;
  return tps >= 100 ? String(Math.round(tps)) : tps.toFixed(1);
}

// tpsNote prints the reply's generation rate in the message footer — the
// bottom-right corner under the answer, where a report on the round reads
// better than in the who/when header. Callers pass only text-bearing turns
// (a tool card alone has nothing to rate). Missing figures (rows from before
// the feature, or an endpoint that reported nothing) print nothing.
function tpsNote(el, completion, durationMS) {
  const tps = msgTps(completion, durationMS);
  if (!tps) return;
  const foot = el.querySelector(".msg-foot");
  const span = el.querySelector(".msg-tps");
  if (!foot || !span || span.textContent) return;
  span.textContent = `${tps} tok/s`;
  foot.classList.add("on"); // the row shows when it has a figure or a button
}

async function selectConversation(id) {
  // A repeated intent for the conversation a navigation is ALREADY loading
  // (C5): while its metadata or first page is in flight, currentId is not
  // yet id, so the committed check below cannot recognise the target — and
  // calling begin() here would orphan the one loader in flight (it loses
  // ownership, the early return starts no replacement, and the chosen
  // thread never renders). The pending navigation goes to this same place;
  // make sure the address names it and let that loader finish.
  if (id === state.navPendingId) {
    if (location.hash !== `#/c/${id}`) location.hash = `#/c/${id}`;
    return;
  }
  // Every other click is a navigation intent, including a repeat click on
  // the conversation already on screen (review2 finding 3): begin() runs
  // FIRST, so a still-pending request from an older click loses its
  // ownership. Returning before it let "select B, then click A again"
  // end with B's late response taking over the screen.
  const tok = navOwner.begin();
  // Any intent replaces or clears the pending marker (C5): it must never
  // outlive its loader, because ownership just moved and that loader will
  // not commit. The marker also drives Send's enablement (review3 issue
  // 2), so the buttons follow it in BOTH directions — set (a new
  // destination) and cleared (a repeat click on the visible thread).
  state.navPendingId = id === state.currentId ? "" : id;
  syncComposerButtons();
  if (id === state.currentId) return;
  // Ownership is captured BEFORE the first await (finding 1): with two
  // chats clicked in quick succession, only the youngest may commit. The
  // confirmed bug wrote currentConv/currentId from whichever response
  // landed LAST — the visible destination could differ from the user's
  // choice, and sends follow state.currentId, so this was more than a
  // flicker.
  await abandonInflight(); // a live stream belongs to the old thread
  try {
    const conv = await api.getConversation(id);
    if (!navOwner.isCurrent(tok)) return; // superseded by a newer navigation
    // Metadata alone must not commit identity (C5): the FIRST PAGE rides
    // with it so conversation row, identity and rendered thread land
    // together. Committing on the metadata opened an interval where the
    // old thread stayed on screen while sends were already addressed to
    // id and the pickers pointed at its model.
    const page = await api.listMessages(id, "");
    if (!navOwner.isCurrent(tok)) return;
    // A generation this commit REPLACES must die with the old view
    // (review3 issue 2) — including work started after navigation began,
    // whose late `start`/token events would otherwise append into the
    // thread painted just now. Ownership is the pair (conversation, view
    // epoch): a generation belongs here only if both still match the
    // destination being committed.
    const gen = state.inflight;
    if (gen && (gen.convId !== id || gen.epoch !== tok)) abandonInflight();
    state.currentConv = conv;
    state.currentId = id;
    state.navPendingId = "";
    if (state.reconcileId === id) state.reconcileId = ""; // this page IS the reconcile
    markActive();
    renderConversation();
    renderThread(page);
    if (state.focusOnOpen) {
      state.focusOnOpen = false;
      focusComposer();
    }
  } catch (err) {
    if (navOwner.isCurrent(tok)) state.navPendingId = ""; // ours to clear
    if (!navOwner.isCurrent(tok)) return; // a superseded request's failure is not this screen's
    // Failed navigation: the OLD thread is still the whole truth, so
    // enablement follows it back (review3 issue 2 — composer and committed
    // thread must never disagree).
    syncComposerButtons();
    if (err instanceof ApiError && err.status === 404) {
      toast("That conversation no longer exists.");
      state.currentId = "";
      location.hash = "";
      renderConversation();
      return;
    }
    // Nothing committed — the previous thread is still the whole truth,
    // so the address returns to what is on screen rather than stranding
    // a hash for a conversation that never rendered (C5).
    location.hash = state.currentId ? `#/c/${state.currentId}` : "";
    handleErr(err, "open conversation");
  }
}

// loadFirstMessages fetches the newest page for convId — captured BEFORE
// the await, never re-read after it (finding 1: the global current ID can
// change mid-flight) — and only the youngest owner paints. A caller inside
// a navigation passes its token; a standalone reload (regenerate's
// rollback) joins the current sequence.
async function loadFirstMessages(tok = navOwner.current(), convId = state.currentId) {
  // "Newer thread activity" is the activity counter, not only the epoch
  // (review3 finding 4): a second send does not bump the navigation owner,
  // yet the answer it produces must never be replaced by this snapshot.
  const rev = state.threadRev;
  const data = await api.listMessages(convId, "");
  if (!navOwner.isCurrent(tok) || state.currentId !== convId) return;
  if (state.threadRev !== rev) return;
  renderThread(data);
}

// beginReconcile reloads the thread after an uncertain loss and OWNS the
// composer until the answer arrives (review3 finding 4): without the
// recovery marker Send reopens while the snapshot is still on the wire,
// and a second send completing before it lands gets painted away by the
// older history. A failed reconcile is ANNOUNCED — the user must not be
// left believing the thread on screen is the whole truth.
function beginReconcile(convId, view) {
  state.reconcileId = convId;
  syncComposerButtons();
  loadFirstMessages(view, convId).then(
    () => endReconcile(convId),
    (err) => {
      if (err instanceof ApiError && err.status === 401) showAuth();
      else if (state.reconcileId === convId) {
        toast("Could not reload the thread — refresh the page to check what was saved.", false);
      }
      endReconcile(convId);
    },
  );
}

function endReconcile(convId) {
  if (state.reconcileId !== convId) return; // a newer marker or commit owns this now
  state.reconcileId = "";
  syncComposerButtons();
}

function renderThread(data) {
  const thread = $("#thread");
  thread.replaceChildren();
  if (data.items.length === 0) {
    thread.appendChild(initialEmptyState());
  } else {
    if (data.next_cursor) thread.appendChild(earlierButton(data.next_cursor));
    // role='tool' rows carry model-facing payloads; the UI shows them as
    // tool cards attached to the assistant message that proposed the call.
    for (const m of data.items) thread.appendChild(msgNode(m));
  }
  refreshLastAssistant();
  updateCtxMeter(lastContextUsed(data.items));
  scrollThreadToBottom();
}

function earlierButton(cursor) {
  const btn = document.createElement("button");
  btn.className = "btn ghost wide load-earlier";
  btn.textContent = "Load earlier messages";
  btn.addEventListener("click", async () => {
    const convId = state.currentId; // the page belongs to THIS thread
    // The view EPOCH, not just the ID (C5): leaving for B and returning to
    // A restores ID equality, but this page was requested against the
    // earlier visit — its rows may already be stale. Any navigation since
    // the click advances the token, and the page is dropped.
    const epoch = navOwner.current();
    try {
      const data = await api.listMessages(convId, cursor);
      if (state.currentId !== convId || !navOwner.isCurrent(epoch)) return; // navigated away mid-page
      const host = $("#thread");
      const nextBtn = data.next_cursor ? earlierButton(data.next_cursor) : null;
      if (nextBtn) btn.replaceWith(nextBtn);
      else btn.remove();
      // The page is chronological (oldest first); insert each item before the
      // same ref so the whole page lands between the button and what was there.
      const ref = nextBtn ? nextBtn.nextSibling : host.firstChild;
      for (const m of data.items) host.insertBefore(msgNode(m), ref);
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

// The Enter/Shift+Enter hint is desktop-only furniture: phones have no
// Shift key to spare, and the long string wraps the composer to two
// lines on narrow screens. Width is re-checked whenever mqPhone flips,
// not just at render — a resized window should behave like CSS does.
function syncPlaceholder() {
  $("#composer-input").placeholder = state.currentId
    ? (isPhone()
        ? "Message Mullsmith…"
        : "Message Mullsmith… (Enter sends, Shift+Enter newline)")
    : "Start or pick a chat first";
}

function syncComposerButtons() {
  const busy = Boolean(state.inflight);
  // A pending navigation owns the screen (review3 issue 2): the thread on
  // display is about to be replaced, so neither Send may address it. The
  // draft stays untouched while the destination loads or fails.
  const nav = Boolean(state.navPendingId);
  // Recovery from an uncertain send (review3 finding 4): until the reconcile
  // lands, this thread's true contents are unknown — nothing new may be
  // addressed to it.
  const rec = state.reconcileId !== "" && state.reconcileId === state.currentId;
  const send = $("#btn-send");
  const stop = $("#btn-stop");
  // Stop swaps into Send's slot while generating: a disabled-but-present
  // Send crowds the row next to the stop square (squeezing the textarea,
  // most visibly on phones). Hiding it makes the replacement literal —
  // and measured, so the row cannot shift: the square takes Send's exact
  // width (measured live, because "Send" is font- and locale-dependent).
  if (busy && !send.hidden) stop.style.minWidth = send.offsetWidth + "px";
  send.hidden = busy;
  send.disabled = busy || nav || rec || !state.currentId;
  stop.hidden = !busy;
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
  // A pending navigation owns the screen (review3 issue 2): the thread on
  // display is about to be replaced, so there is nothing this could truly
  // address. Send is disabled while the marker is set, but Enter paths
  // arrive here too — reject them, and the draft stays untouched.
  // A reconcile in flight (review3 finding 4): the thread on display is not
  // known to be the whole truth yet, so a send would be aimed at a fiction.
  if (!state.currentId || state.navPendingId || state.inflight ||
      state.reconcileId === state.currentId || !text.trim()) return;
  if (state.pending.some((f) => f.uploading)) {
    toast("Still uploading — give it a moment.", false);
    return;
  }
  // Capture the addressee BEFORE anything async (finding 1): the URL, the
  // tray clear, the reorder and the uncertain-acceptance reconcile all
  // speak of THIS conversation, never of whatever is on screen by then.
  const convId = state.currentId;
  const view = navOwner.current();
  const attachments = state.pending.slice();
  const placeholder = $("#thread .empty-state");
  if (placeholder) placeholder.remove();
  const userEl = appendUserBubble(text.trim(), attachments);
  input.value = "";
  autosizeComposer();
  const body = { content: text.trim() };
  if (attachments.length) body.attachment_paths = attachments.map((f) => f.path);
  // Piggyback the level the selector shows: a thinking PATCH still in
  // flight would otherwise let this turn snapshot the stored value from
  // before the switch (the server persists this field before streaming).
  const thinkSel = $("#thinking-select");
  if (!thinkSel.disabled && thinkSel.value) body.thinking = thinkSel.value;
  await startGeneration(
    `/api/v1/conversations/${encodeURIComponent(convId)}/messages`,
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
      // The post bumped updated_at server-side — mirror the new order now.
      promoteConversation(convId);
    },
    () => {
      // UNCERTAIN acceptance (review3 issue 4): the turn may be durable
      // with the answer already streamed. Drop the optimistic guess, ask
      // the server what is really there — under the captured (convId,
      // view), so a late reconcile can never paint across a navigation —
      // and give the draft back: a lost typing is worse than a visible
      // possible duplicate the toast justifies.
      userEl.remove();
      if (state.currentId === convId && navOwner.isCurrent(view)) {
        input.value = text;
        autosizeComposer();
      }
      toast("Connection lost before the reply started — reloading to check it was saved.", false);
      beginReconcile(convId, view);
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
  state.threadRev++; // newer than any history page already on the wire
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
  el.querySelector(".msg-role").textContent = "Mullsmith";
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
// clears when the markdown answer lands. The card NODES, the outcome
// painting and the state labels live in thread.js — the single projection
// shared by the live SSE events and the persisted tool_calls projection on
// reload (C7). This file only wires events to bubbles.

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
  allow.className = "btn small primary";
  allow.textContent = "Allow";
  const deny = document.createElement("button");
  deny.type = "button";
  deny.className = "btn small ghost";
  deny.textContent = "Deny";
  // §6.1/§9.9: code_run offers "always allow in this conversation".
  let rememberBox = null;
  if (d.rememberable) {
    const cb = document.createElement("input");
    cb.type = "checkbox";
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
      // Carry the download URL through: the optimistic user bubble's file
      // chip is built from it before the thread ever reloads.
      entry.download_url = rec.download_url;
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

// Shared by both "Delete older than…" command dropdowns (sidebar chats and
// the Files view): token → confirmation label, months/years being the
// server's deliberate calendar approximations (spec §5.3).
const pruneLabels = { "1d": "1 day", "1w": "1 week", "1mo": "1 month", "1y": "1 year", all: "ALL" };

// When a native confirm() closes, WebKit hands focus back to whatever held
// it before — the select — and a focused select opens its popup on iOS.
// Take it back off: now, in case focus already came back, and via the
// once-listener whenever WebKit actually performs the restore (it expires
// after a sec so a later keyboard Tab still works).
function kickSelectRefocus(el) {
  const kick = () => el.blur();
  kick();
  el.addEventListener("focus", kick, { once: true });
  setTimeout(() => el.removeEventListener("focus", kick), 1000);
}

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
  // email_features: the field keeps showing the CONFIRMED address; a
  // pending change is a notice with its own escape hatch, not a value.
  const pendingAddr = me.unconfirmed_email || "";
  show($("#settings-email-pending"), !!pendingAddr);
  if (pendingAddr) $("#pending-email-addr").textContent = pendingAddr;
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
    if (state.me.unconfirmed_email) {
      toast("Confirmation link sent to " + state.me.unconfirmed_email, false);
    } else {
      toast("Settings saved", false);
    }
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
async function startGeneration(url, body, rollback, onAccepted, onUncertain) {
  const controller = new AbortController();
  const st = {
    controller,
    genId: "",
    msgId: "", // id of the round currently streaming (C7)
    started: false,
    errorData: null,
    nodes: null, // = st.cur.nodes: thinking/spinner helpers read this
    // Rounds in row order (C7): one entry per announced model round,
    // { id, nodes, raw, reasoning }. The bubble boundaries here ARE the
    // stored row boundaries — that is the whole live/reload agreement.
    rounds: [],
    byId: new Map(), // message_id → round, the target every event names
    cur: null, // the round currently streaming
    cards: {}, // call_id → { card, args } for the live tool cards
    // The ownership pair (review3 issue 2): WHICH conversation this
    // request addresses and WHICH view epoch owned the screen when it
    // started — the pair a navigation commit compares against before
    // disposing a generation the new thread replaces.
    convId: state.currentId,
    epoch: navOwner.current(),
  };
  state.inflight = st;
  syncComposerButtons();
  // One ownership rule for every live callback (finding 5, extended by
  // review3 issue 2): events belong on screen only while this is still
  // the live generation AND the conversation it addresses is the one on
  // display. The epoch is deliberately NOT part of live(): a repeat click
  // on the CURRENT conversation bumps navOwner without orphaning a stream
  // that still belongs there.
  const live = () => state.inflight === st && state.currentId === st.convId;
  // openRound opens one bubble per model round (C7). The `start` and
  // `round` events each name the row that round streams into, so a
  // tool-only round keeps its own (empty) prose and its own cards instead
  // of merging everything into one bubble stamped with the final row's id.
  const openRound = (msgId) => {
    stopSpinner(st); // the previous round stops "waiting" as the next begins
    if (st.cur) syncRoundBody(st.cur);
    const el = streamNode();
    el.dataset.gen = st.genId || ""; // the turn marker C7 uses to retire rounds
    $("#thread").append(el);
    const nodes = { el, body: el.querySelector(".msg-body") };
    // Dedicated text node so tokens never clobber the container; the
    // spinner rides along until the first answer token lands, then
    // settleRound swaps everything for rendered markdown.
    nodes.text = document.createElement("div"); // prose only: cards live outside it
    nodes.text.className = "stream-text";
    nodes.body.append(nodes.text, spinnerNode(st));
    const r = { id: msgId || "", nodes, raw: "", reasoning: "" };
    st.rounds.push(r);
    if (r.id) st.byId.set(r.id, r);
    st.cur = r;
    st.nodes = nodes;
  };
  // Every tool event names the round that owns its card.
  const cardHost = (d) => {
    const r = st.byId.get(d.message_id);
    if (!r) return null;
    // The tool card is now the progress indicator. Retire this round's
    // waiting dots without leaving their empty answer bubble underneath.
    if (r === st.cur) stopSpinner(st);
    syncRoundBody(r);
    return toolCardsHost(r.nodes.el);
  };
  // One terminal transition for the UNCERTAIN pre-start outcome (review3
  // issue 4): the request may have been accepted before the connection
  // died, so a rollback that ASSUMES refusal is not safe — the caller
  // reconciles against the server instead (reload, draft, toast).
  const settleUncertain = (err) => {
    if (state.inflight !== st) return;
    state.inflight = null;
    syncComposerButtons();
    if (onUncertain) onUncertain(err);
    else if (rollback) rollback();
  };
  try {
    await streamChat(
      url,
      body,
      {
        onStart(d) {
          if (!live()) return;
          state.threadRev++; // a live stream IS newer activity (finding 4)
          st.started = true;
          st.genId = d.generation_id || "";
          st.msgId = d.message_id || "";
          if (onAccepted) onAccepted();
          openRound(st.msgId); // `start` names round 1's own row (C7)
          maybeScroll(true);
        },
        onRound(d) {
          if (!live()) return;
          // A fresh assistant row exists for the next model round (C7):
          // open the bubble it names, so the live split matches history.
          st.msgId = d.message_id || "";
          openRound(st.msgId);
          maybeScroll(true);
        },
        onToken(t) {
          if (!live()) return;
          stopSpinner(st); // output is flowing: the "waiting" dots are done
          if (!st.cur) return;
          st.cur.raw += t;
          st.cur.nodes.text.textContent = st.cur.raw; // plain text while streaming
          syncRoundBody(st.cur);
          maybeScroll();
        },
        onReasoning(t) {
          if (!live()) return;
          if (!st.cur) return;
          st.cur.reasoning += t;
          if (!state.meta || !state.meta.show_reasoning || !st.nodes) return;
          ensureThinking(st);
          st.nodes.thinkPre.textContent = st.cur.reasoning;
          maybeScroll();
        },
        onToolStart(d) {
          if (!live()) return;
          if (!st.cur) return;
          const host = cardHost(d);
          if (!host) return;
          const card = toolCardNode(d.call_id, d.tool);
          host.append(card);
          st.cards[d.call_id] = { card, args: "" };
          maybeScroll();
        },
        onToolArgs(d) {
          if (!live()) return;
          const c = st.cards[d.call_id];
          if (!c) return;
          c.args += d.t || "";
          const pre = c.card.querySelector(".tool-args");
          pre.textContent = formatToolArgs(c.args);
          pre.hidden = false;
        },
        onToolEnd(d) {
          if (!live()) return;
          let c = st.cards[d.call_id];
          if (!c) {
            // The card announced at tool_start carries a different id than
            // the audit row's terminal event (an endpoint may fragment a
            // call's id differently than the accumulator stitches it). The
            // outcome belongs on the round that ran it: settle that round's
            // still-running card of this tool, so the live view agrees with
            // the reload instead of dropping the event.
            const r = st.byId.get(d.message_id);
            if (r) {
              for (const other of Object.values(st.cards)) {
                if (other.card.classList.contains("st-running") &&
                    (other.card.querySelector(".tool-name").textContent || "") === (d.tool || "") &&
                    r.nodes.el.contains(other.card)) { c = other; break; }
              }
            }
            if (!c) return; // no matching round on screen: the reload will show it
            delete st.cards[d.call_id];
            st.cards[d.call_id] = c; // later events for this id find it too
          }
          // The SAME projection a reload renders from tool_calls.display
          // (C7): one paintCallOutcome for both views.
          const view = callFromEvent(d);
          paintCallOutcome(c.card, view);
          // files_share ⇒ a visible download card next to the card (§10.2).
          const file = sharedFileFor(view);
          if (file) {
            const host = cardHost(d);
            if (host) host.append(artifactCard(file));
          }
          maybeScroll();
        },
        onAwaitingApproval(d) {
          if (!live()) return;
          if (!st.cur) return;
          const host = cardHost(d);
          if (!host) return;
          let c = st.cards[d.call_id];
          if (!c) {
            const card = toolCardNode(d.call_id, d.tool);
            host.append(card);
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
    // Upstream closed cleanly but never said done. After start the answer
    // was interrupted; BEFORE start the outcome is uncertain (review3
    // issue 4) — the request may have been accepted and the reply is
    // already durable — so reconcile exactly like a network death.
    if (state.inflight === st) {
      if (st.started) finishGeneration(st, { status: "lost" });
      else settleUncertain(new Error("The connection closed before the reply started."));
    }
  } catch (err) {
    if (err.name === "AbortError") {
      // One idempotent terminal transition (finding 5): finishGeneration
      // is the single owner of cleanup, and its own staleness check makes
      // it a no-op once the stream was abandoned. The old bare `return`
      // left state.inflight SET whenever Stop's abort API failed and the
      // fetch abort arrived here — the composer refused every later send.
      finishGeneration(st, { status: "aborted" });
      return;
    }
    if (err instanceof ApiError && err.status === 401) {
      // A late 401 from an ABANDONED request is not the current screen's
      // verdict (review2 finding 4): clearing state.inflight here erased a
      // newer live generation's bookkeeping. The generation that owns the
      // screen will meet its own auth failure if the session is really
      // gone.
      if (!live()) return;
      state.inflight = null;
      syncComposerButtons();
      handleErr(err, "send");
      return;
    }
    if (st.started) {
      finishGeneration(st, { status: "lost" });
    } else {
      // Pre-start rollback is screen-mutating (restores the draft, may
      // pin the ctx-full panel): an abandoned generation must not run it
      // against a composer that belongs to the newer one (review2
      // finding 4's reproduced sequence).
      if (!live()) return;
      // A DEFINITE refusal versus an uncertain loss (review3 issue 4): a
      // code in the envelope is this API's own rejection, written before
      // anything was accepted or persisted — rollback is honest. Without
      // a code the death came from transport or proxy, and whether the
      // POST landed is unknown: reconcile instead of hiding a durable
      // message behind a restored draft.
      if (err instanceof ApiError && err.code) {
        if (rollback) rollback();
        if (err.code === "context_full" || err.code === "history_limit") {
          // The thread can take no more turns (or no more MESSAGES — the
          // replay cap): pin the explanation where the fullness figures
          // live instead of a toast that fades in five seconds.
          showCtxFull(err.message);
        } else {
          toast(err.message || "Could not start a reply.");
        }
        state.inflight = null;
        syncComposerButtons();
      } else {
        settleUncertain(err);
      }
    }
  }
}

function finishGeneration(st, done) {
  stopSpinner(st); // every exit path must end the animation
  retireStrandedCards(st, done.rounds || []);
  if (state.inflight !== st) return; // abandoned mid-flight; bubble is gone
  state.inflight = null;
  state.threadRev++; // the finished answer outranks any history on the wire
  syncComposerButtons();
  if (!st.rounds.length) return; // nothing ever streamed onto the screen
  const err = (st.errorData && st.errorData.message) || "";
  const outcomes = new Map((done.rounds || []).map((entry) => [entry.message_id, entry]));
  for (const r of st.rounds) {
    const entry = outcomes.get(r.id);
    if (entry) {
      settleRound(r, entry.status, entry.note,
        entry.completion_tokens, entry.duration_ms, err);
    } else {
      // Transport loss or a failed save can leave an announced round
      // without a durable outcome. Keep its identity and mark it interrupted.
      settleRound(r, done.status === "complete" ? "lost" : (done.status || "lost"),
        "", 0, 0, err);
    }
  }
  if (done.usage_tokens > 0) updateCtxMeter(done.usage_tokens); // freshest fullness figure
  for (const r of st.rounds) {
    // the button becomes visible now — make it work, per settled row
    if (r.nodes.el.dataset.id) {
      if (st.genId) r.nodes.el.dataset.gen = st.genId; // the shared turn marker (C7)
      attachRegen(r.nodes.el);
    }
  }
  refreshLastAssistant();
  maybeScroll();
}

// settleRound closes one streamed bubble: markdown replaces the raw text,
// the terminal note lands, and speed rates the prose. The note comes from
// the server's durable text when there is one (rounds[].note /
// status_note — the same string either way); otherwise roundStatusNote
// supplies the exact line a reload will derive from the row's status.
function syncRoundBody(r) {
  // Keep a new round's waiting indicator and actual prose visible; tool
  // cards, reasoning and status notes have their own containers.
  r.nodes.body.hidden = !r.raw.trim() && !r.nodes.body.querySelector(".spinner");
}

function settleRound(r, status, note, completion, durationMS, errorMessage) {
  const nodes = r.nodes;
  if (nodes.think) {
    nodes.think.open = false;
    nodes.thinkSummary.textContent = "Thought process";
  }
  nodes.body.classList.remove("streaming");
  // Markdown replaces the raw text INSIDE the prose wrapper: the reloaded
  // bubble (msgNode) puts its markdown in the same .stream-text, so the
  // two views are structurally identical.
  nodes.text.classList.add("md");
  nodes.text.replaceChildren(renderMarkdown(r.raw)); // full markdown once done
  syncRoundBody(r);
  // Any approval prompt still on screen is dead: the loop moved on
  // (timeout, deny or abort). Retire the buttons.
  for (const ctl of nodes.el.querySelectorAll(".approval-row button, .approval-row input")) ctl.disabled = true;
  if (r.id) nodes.el.dataset.id = r.id;
  const text = roundStatusNote(status, note, status === "error" ? errorMessage : "");
  if (text) showStatusNote(nodes.el, text);
  // Rate the prose only: a turn whose body is empty (tool card alone) shows nothing.
  if (r.raw.trim()) tpsNote(nodes.el, completion, durationMS);
}

// retireStrandedCards is the generation's last word on its own DOM: a tool
// card still marked running when the stream ended never received its
// tool_end — the outcome landed on the RELOAD projection instead (ids can
// disagree between the streaming announcement and the stored audit row).
// Relabel it exactly what the reload will say — "interrupted" — rather
// than leave a perpetual running… card on screen; a card still asking for
// an approval the generation no longer has is retired the same way.
function retireStrandedCards(st, outcomes) {
  const incomplete = new Set(outcomes.filter((r) => r.status !== "complete").map((r) => r.message_id));
  for (const key of Object.keys(st.cards || {})) {
    const c = st.cards[key];
    if (!c || !c.card.isConnected) continue;
    if (!c.card.classList.contains("st-running") &&
        !c.card.classList.contains("st-awaiting")) continue;
    // A failed model round never reached tool execution. Its streamed
    // proposals have no audit rows and must not survive as phantom cards.
    const round = st.rounds.find((r) => r.nodes.el.contains(c.card));
    if (round && incomplete.has(round.id)) {
      c.card.remove();
      continue;
    }
    c.card.classList.remove("st-running", "st-awaiting");
    c.card.classList.add("st-lost");
    const state = c.card.querySelector(".tool-state");
    if (state) state.textContent = "interrupted";
    for (const ctl of c.card.querySelectorAll(".approval-row button, .approval-row input")) ctl.disabled = true;
  }
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
  // Same screen ownership as Send (review3 issue 2): while a navigation
  // is loading, this bubble may not even be on the thread that commits.
  if (state.inflight || state.navPendingId || state.reconcileId === state.currentId || !msgId) return;
  // Captured before the removal and the request: reloads and the reorder
  // belong to THIS thread, under THIS view epoch (review3 issues 2 and 4
  // meet here — a definite refusal and an uncertain death both reconcile
  // against the server, and neither may paint a screen we left).
  const convId = state.currentId;
  const view = navOwner.current();
  // Both terminal paths RELOAD, and both go through the recovery state:
  // while the truth is being re-fetched nothing else may address this
  // thread, an older snapshot can never paint over a newer answer, and a
  // failed reload is announced rather than swallowed (review3 finding 4).
  const reload = () => beginReconcile(convId, view);
  // Retire the WHOLE turn from the screen, not just the clicked bubble:
  // the server deletes every row of the generation, and the same marker
  // groups the streamed rounds.
  const gen = el.dataset.gen;
  for (const bubble of document.querySelectorAll(`#thread .msg[data-gen="${gen}"]`)) bubble.remove();
  await startGeneration(
    `/api/v1/messages/${encodeURIComponent(msgId)}/regenerate`,
    undefined,
    reload,
    () => promoteConversation(convId), // a fresh reply is a post too
    reload // uncertain acceptance: the deleted row may have been replaced — the server holds the truth
  );
}

// Stop the running generation because the UI thread is going away. Detach
// LOCALLY first (finding 5): state is cleared and the fetch aborted before
// anything is awaited, so navigation is never held hostage by the abort
// API, and the old stream's callbacks are already dead by the time the
// next screen paints. The server abort stays — best-effort persistence
// coordination for the partial answer, fire-and-forget.
function abandonInflight() {
  const st = state.inflight;
  if (!st) return;
  stopSpinner(st); // the bubble is going away; don't spin on a detached node
  state.inflight = null;
  st.controller.abort(); // kills the fetch; its AbortError lands on a dead token
  syncComposerButtons();
  if (st.genId) {
    api.abortGeneration(st.genId).catch(() => { /* already finished or gone */ });
  }
}

function renderConversation() {
  const c = state.currentId ? state.currentConv : null;
  $("#topbar-title").textContent = c ? c.title : "Mullsmith";
  const sel = $("#model-picker");
  sel.disabled = !state.currentId;
  if (c) {
    // Show the model ACTUALLY in use. The server already resolves a
    // model the config no longer offers to the default, so the name is
    // always one of the picker's own options.
    const active = c.model || state.meta.default_model || "";
    sel.value = active;
  }

  const input = $("#composer-input");
  input.disabled = !state.currentId;
  syncPlaceholder();
  if (!state.currentId) input.value = "";
  syncComposerButtons();
  // Each dropdown entry describes ITS model: opening a conversation on
  // another model must rebuild the choice list, not just re-point the
  // previous model's options.
  populateThinkingChoices();
  syncThinkingSelect();

  if (!state.currentId) {
    state.currentConv = null;
    state.lastCtxUsed = 0; // the meter speaks for one thread only
    $("#thread").replaceChildren(initialEmptyState());
    updateCtxMeter(0); // no thread: no meter
  }
}

function initialEmptyState() {
  const el = document.createElement("div");
  el.className = "empty-state";
  const h = document.createElement("h2");
  h.textContent = "Mullsmith";
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
    // A new chat continues from the model currently in the picker (the
    // conversation you were last on), not the config default: pass the
    // picker's value so the row stores it.
    const c = await api.createConversation("", $("#model-picker").value);
    state.convs.unshift(c);
    // The created conversation carries its model's context window (the
    // server refreshed auto-detection on create), so the meter gets its
    // denominator even if boot-time discovery raced a starting model
    // server — no extra round-trip here.
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

// Settings PATCHes for the conversation on screen share ONE queue (C5):
// two quick picks must reach the server — and commit — in click order.
// Unqueued, PATCH N+1 can overcomplete PATCH N and the older response,
// still carrying a fuller row, can land LAST and overwrite the newer pick.
let settingsPatch = Promise.resolve();

async function onModelChange(ev) {
  if (!state.currentId) return;
  const convId = state.currentId; // the PATCH targets the chat on screen
  const model = ev.target.value;
  const epoch = navOwner.current(); // view epoch at click time (C5)
  // Identity equality alone admits an obsolete commit after A → B → A;
  // the epoch moves on every navigation, return included.
  const owns = () => navOwner.isCurrent(epoch) && state.currentId === convId;
  // .catch(…) before .then: one block's unexpected throw must not poison
  // the queue for every later pick.
  settingsPatch = settingsPatch.catch(() => {}).then(async () => {
    try {
      const updated = await api.patchConversation(convId, { model });
      // A PATCH that lands after navigation describes ANOTHER conversation —
      // committing it would put that row's payload on this screen (finding 1).
      if (!owns()) return;
      state.currentConv = updated;
      toast(`Model: ${model || "default"}`, false);
    } catch (err) {
      if (!owns()) return;
      handleErr(err, "set model");
    }
    if (!owns()) return; // the new screen rendered its own state
    // The dropdown describes the model now in force, so it is rebuilt
    // after a switch either way: a profile-less model hides the row, and
    // a saved choice outside the new model's list falls back to Default.
    populateThinkingChoices();
    syncThinkingSelect();
    // The meter's denominator is the NEW model's window — redraw the
    // figure it already has against it.
    updateCtxMeter(state.lastCtxUsed || 0);
  });
  await settingsPatch;
}

// --- thinking-level selector -----------------------------------------------------

// The list for the model the conversation actually runs on. The server
// projects the ACTIVE model — a stored pick the deployment no longer
// offers has already resolved to the default — so the name here always
// has a row; the empty fallback is belt-and-braces.
function thinkingChoices() {
  const th = state.meta && state.meta.thinking;
  if (!th || !th.choices_by_model) return [];
  const active = (state.currentConv && state.currentConv.model)
    || state.meta.default_model || "";
  return th.choices_by_model[active] || [];
}

// Build the dropdown from /api/v1/meta. The row APPEARS whenever the
// deployment has thinking somewhere (meta.thinking is non-null): a model
// with nothing to choose then shows a DISABLED dropdown whose tooltip
// points at the fix — hiding it would leave the feature undiscoverable
// for users whose default model has no profile. Only when no entry
// anywhere names a profile (meta.thinking null) does the row disappear:
// the feature does not exist for this install (§6.3).
function populateThinkingChoices() {
  const sel = $("#thinking-select");
  sel.replaceChildren();
  for (const c of thinkingChoices()) {
    const opt = document.createElement("option");
    opt.value = c.id;
    opt.textContent = c.label;
    sel.appendChild(opt);
  }
  show($("#thinking-row"), Boolean(state.meta && state.meta.thinking));
}

// The server projects the EFFECTIVE choice (a stale one is already
// Default), so the dropdown simply names what the next request sends.
// Disabled means this model offers nothing to choose — no profile, or
// none its endpoint understands; the tooltip says switch model.
function syncThinkingSelect() {
  const sel = $("#thinking-select");
  const choices = thinkingChoices();
  sel.disabled = !state.currentId || choices.length < 2;
  sel.title = (state.currentId && choices.length < 2)
    ? "this model has no thinking to choose — switch to a thinking model"
    : "Thinking level for this conversation";
  if (!state.currentId) return;
  const want = (state.currentConv && state.currentConv.thinking) || "default";
  sel.value = [...sel.options].some((o) => o.value === want) ? want : "default";
}

async function onThinkingChange(ev) {
  if (!state.currentId) return;
  const convId = state.currentId; // identity guard, as in onModelChange
  const choice = ev.target.value;
  const before = (state.currentConv && state.currentConv.thinking) || "default";
  const label = ev.target.selectedOptions[0]
    ? ev.target.selectedOptions[0].textContent : choice;
  const epoch = navOwner.current(); // view epoch at click time (C5)
  const owns = () => navOwner.isCurrent(epoch) && state.currentId === convId;
  // Same single queue as the model picker (C5): model and thinking picks
  // for one conversation commit in click order, never racing each other.
  settingsPatch = settingsPatch.catch(() => {}).then(async () => {
    try {
      const updated = await api.patchConversation(convId, { thinking: choice });
      if (!owns()) return; // navigated mid-PATCH (same ID under a newer epoch too)
      state.currentConv = updated;
      syncThinkingSelect();
      toast(`Thinking: ${label}`, false);
    } catch (err) {
      if (!owns()) return;
      // The server kept the old state — the dropdown must show that, not
      // the rejected pick.
      ev.target.value = before;
      handleErr(err, "set thinking level");
    }
  });
  await settingsPatch;
}

// --- composer "+" add-menu ---------------------------------------------------------------

// One button, one vertical menu: attach files and the thinking selector
// live here instead of occupying the composer bar and a row beneath it.
function setAddMenu(open) {
  show($("#add-menu"), open);
  $("#btn-add").setAttribute("aria-expanded", String(open));
}

function closeAddMenu() { setAddMenu(false); }

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
mqPhone.addEventListener("change", syncPlaceholder); // live, like a CSS media query

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
  if (emailTokenRoute()) {
    // Mail link opened in an already-running tab: settle it in place.
    consumeEmailToken().then((handled) => {
      if (!handled || handled === "auth") return;
      state.me = handled;
      $("#user-chip").textContent = handled.name || handled.email;
      if (state.settingsOpen) fillSettingsForm();
    });
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
  } else if ((state.currentId || state.navPendingId) && location.hash === "") {
    // Leaving for the blank route retires EVERYTHING aimed at a thread:
    // a navigation whose metadata or page is still in flight would
    // otherwise commit an identity the user has just walked away from,
    // with the URL showing nothing (C5). bump() kills those loaders;
    // the pending marker goes with them.
    navOwner.bump();
    state.navPendingId = "";
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
  $("#form-forgot").addEventListener("submit", onForgot);
  $("#form-reset").addEventListener("submit", onResetSubmit);
  $("#btn-resend-confirm").addEventListener("click", onResendConfirm);
  $("#link-forgot").addEventListener("click", (e) => {
    e.preventDefault();
    location.hash = "#forgot";
  });
  $("#link-forgot-back").addEventListener("click", (e) => {
    e.preventDefault();
    location.hash = "";
  });
  $("#link-confirm-back").addEventListener("click", (e) => {
    e.preventDefault();
    state.pendingInbox = ""; // "back to sign in" also dismisses the panel
    location.hash = "";
  });
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
  // "Delete older than…" is a command dropdown: the choice fires a prune,
  // then the select falls back to its prompt (it holds no setting). The
  // server answers with the deleted ids, so we know precisely whether the
  // open thread was among them.
  $("#conv-prune-select").addEventListener("change", async (ev) => {
    const choice = ev.target.value;
    ev.target.value = "";
    if (!choice) return;
    const what =
      choice === "all" ? "ALL conversations" : `conversations older than ${pruneLabels[choice]}`;
    const ok = await confirmDialog(`Delete ${what}? This cannot be undone.`);
    kickSelectRefocus(ev.target);
    if (!ok) return;
    await abandonInflight(); // the open thread may be one of the doomed
    try {
      const res = await api.pruneConversations(choice);
      await loadConversations();
      if (state.currentId && res.ids.includes(state.currentId)) {
        state.currentId = "";
        location.hash = "";
        renderConversation();
      }
      toast(res.deleted ? `${res.deleted} conversation${res.deleted === 1 ? "" : "s"} deleted` : "Nothing to delete", false);
    } catch (err) {
      handleErr(err, "delete older conversations");
    }
  });
  $("#model-picker").addEventListener("change", onModelChange);
  // Row action menu: outside click or Escape dismisses it. The "…" button
  // is its own toggle (its click bubbles here after the menu opened), so
  // the "outside" check must spare it like a container.
  document.addEventListener("click", (ev) => {
    if (!$("#conv-row-menu").hidden && Date.now() < strayClickGuardUntil && ev.target.closest("#conv-list")) return;
    if (!$("#conv-row-menu").hidden && !ev.target.closest("#conv-row-menu, .conv-menu-btn")) closeConvRowMenu();
  });
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") closeConvRowMenu();
  });
  $("#conv-row-menu").addEventListener("click", (ev) => {
    const item = ev.target.closest(".row-menu-item");
    if (!rowMenuTarget || !item) return;
    const { rename, del } = rowMenuTarget;
    closeConvRowMenu();
    if (item.dataset.action === "rename") rename();
    else del();
  });
  $("#thinking-select").addEventListener("change", (ev) => {
    onThinkingChange(ev);
    closeAddMenu(); // one pick, menu done — like the sidebar's command dropdown
  });
  $("#btn-menu").addEventListener("click", () =>
    document.body.classList.contains("drawer-open") ? closeDrawer() : openDrawer()
  );
  $("#backdrop").addEventListener("click", closeDrawer);
  $("#btn-send").addEventListener("click", () => sendFromComposer());
  $("#btn-stop").addEventListener("click", stopStream);
  // The add-menu: the "+" toggles it; any click outside or Escape dismisses.
  // The check is containment, so clicking the open select's own rows — or
  // the button again — never fights the toggle.
  $("#btn-add").addEventListener("click", () => {
    setAddMenu($("#add-menu").hidden);
  });
  document.addEventListener("click", (ev) => {
    if (!$("#add-menu").hidden && !$("#add-wrap").contains(ev.target)) closeAddMenu();
  });
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") closeAddMenu();
  });
  $("#btn-attach").addEventListener("click", () => {
    closeAddMenu();
    $("#attach-input").click();
  });
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
  $("#btn-cancel-email").addEventListener("click", onCancelPendingEmail);
  $("#btn-delete-account").addEventListener("click", onDeleteAccount);
  for (const fb of document.querySelectorAll(".files-filter")) {
    fb.addEventListener("click", () => {
      state.filesKind = fb.dataset.kind || "";
      for (const b of document.querySelectorAll(".files-filter")) {
        b.classList.toggle("active", b === fb);
      }
      loadFiles(true).catch((e) => handleErr(e, "files"));
    });
  }
  // Bulk housekeeping, same command-dropdown shape as the sidebar's — but
  // always scoped to the ACTIVE filter: the purge covers the list the user
  // is looking at, never a directory they are not viewing.
  $("#files-purge-select").addEventListener("change", async (ev) => {
    const choice = ev.target.value;
    ev.target.value = "";
    if (!choice) return;
    const active = document.querySelector(".files-filter.active");
    const view = active ? active.textContent : "All";
    const what =
      choice === "all"
        ? `ALL files and folders in ${view}`
        : `files in ${view} older than ${pruneLabels[choice]}`;
    const ok = await confirmDialog(`Delete ${what}? This cannot be undone.`);
    kickSelectRefocus(ev.target);
    if (!ok) return;
    try {
      const res = await api.purgeFiles(state.filesKind, choice);
      await loadFiles(true);
      const n = res.files_deleted || 0;
      let msg = n === 0 ? "Nothing to delete" : `${n} file${n === 1 ? "" : "s"} deleted`;
      if (res.dirs_deleted) msg += ` + ${res.dirs_deleted} empty folder${res.dirs_deleted === 1 ? "" : "s"}`;
      if (res.capped) msg += " — some entries remain, run it again";
      toast(msg, false);
    } catch (err) {
      handleErr(err, "delete older files");
    }
  });
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
    state.meta = { registration: "open", remember_login: true, email_features: false,
      models: [], default_model: "" };
  }
  applyMetaFlags();

  try {
    const me = await api.me();
    // A mail link can land on a live session (Settings confirmations are
    // clicked exactly when logged in): settle it BEFORE enterChat, which
    // would otherwise wipe the token hash as a leftover.
    const handled = await consumeEmailToken();
    if (handled === "auth") return;
    enterChat(handled || me);
  } catch (err) {
    if (!(err instanceof ApiError) || err.status !== 401) {
      toast(`Cannot reach the server: ${err.message}`);
    }
    showAuth();
  }
}

// boot() runs on import in the browser. The node lifecycle tests
// (web/test/lifecycle.test.mjs) import this module with
// __MULLSMITH_TEST__ preset to drive the real selectConversation /
// startGeneration against stubs — everything else stays identical.
if (!globalThis.__MULLSMITH_TEST__) boot();

// Test-only exports for the lifecycle suite; the browser ignores them.
export {
  state, navOwner, listOwner, selectConversation, startGeneration, abandonInflight,
  sendFromComposer, regenerateFrom, onHashChange, onModelChange, onThinkingChange, earlierButton,
};
