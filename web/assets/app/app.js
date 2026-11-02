// Application shell: authentication, sidebar, composer, files and settings.
// Navigation, generation and thread rendering have separate owners below.

import { api, ApiError } from "/assets/app/api.js";
import { LatestWins } from "/assets/app/ownership.js";
import {createNavigationController} from "/assets/app/navigation.js";
import {createGenerationController} from "/assets/app/generation.js";
import {createThreadRenderer} from "/assets/app/thread-renderer.js";
import {humanSize,fileApiUrl} from "/assets/app/file-presenter.js";

const state = {
  meta: null,
  me: null,
  convs: [],
  convCursor: "",
  query: "",
  // uploads staged in the composer
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

const listOwner = new LatestWins();

let generation;
const renderer = createThreadRenderer({
  onRegenerate: (...args) => generation.regenerateFrom(...args),
  onEarlier: (cursor) => navigation.earlierButton(cursor),
  onApproval: (...args) => generation.resolveApproval(...args),
  updateCtxMeter, recordActivity: () => navigation.recordActivity(), toast,
});
const navigation = createNavigationController({
  view: renderer, getGeneration: () => generation.current,
  abandon: () => generation.abandonInflight(), renderConversation,
  syncComposerButtons, markActive, focusComposer, showAuth, toast, handleErr,
  recoverMissingConversation: firstConversationAfterRefresh,
});
generation = createGenerationController({
  navigation, view: renderer, getMeta: () => state.meta, syncComposerButtons,
  toast, handleErr, applyTitle, showCtxFull, updateCtxMeter, promoteConversation,
});
const navOwner = navigation.owner;
const { selectConversation, beginReconcile, earlierButton } = navigation;
const { startGeneration, stopStream, regenerateFrom, abandonInflight } = generation;
const { appendUserBubble, initialEmptyState } = renderer;

// --- view switching ------------------------------------------------------------

function showAuth() {
  abandonInflight();
  listOwner.bump();
  state.me = null;
  navigation.reset();
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
  try {
    state.meta = { ...state.meta, ...await api.clientConfig() };
    applyMetaFlags();
  } catch (err) {
    handleErr(err, "load application configuration");
    return;
  }
  state.me = user;
  show($("#view-auth"), false);
  show($("#view-chat"), true);
  if (location.hash && !location.hash.startsWith("#/c/") && location.hash !== "#/files") {
    location.hash = ""; // drop #login/#register leftovers
  }
  if (location.hash.startsWith("#/c/")) {
    // Let the hash drive the initial selection (awaited: the auto-select
    // below must not race an in-flight deep-link open).
    navigation.requestFocus();
    await onHashChange();
  }
  if (!navigation.currentId) {
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
  if (!navigation.currentId && state.convs.length) {
    navigation.requestFocus();
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
  for (const value of new Set([m.default_model, ...(m.models || [])].filter(Boolean))) {
    const opt = document.createElement("option");
    opt.value = value;
    // Shorten the label only; selection and requests retain the full ID.
    opt.textContent = value.slice(value.indexOf("/") + 1);
    opt.title = value;
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
    await enterChat(me);
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
    await enterChat(me);
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
  if (c.id === navigation.currentId) li.classList.add("active");

  li.querySelector(".conv-open").addEventListener("click", () => {
    // The long-press echo (see strayClickGuardUntil) must not open the chat.
    if (Date.now() < strayClickGuardUntil) return;
    // Typing right away is the point of opening a chat: the one-shot makes
    // selectConversation focus the composer once it has loaded (skipped on
    // phones, and on re-clicks of the open chat, which fire no hashchange —
    // an unconsumed flag would focus unexpectedly on the next navigation).
    if (c.id !== navigation.currentId) navigation.requestFocus();
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
      if (c.id === navigation.currentId) {
        navigation.updateConversation({ ...navigation.currentConv, title: updated.title });
        renderConversation();
      }
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
    if (c.id === navigation.currentId) await abandonInflight();
    try {
      await api.deleteConversation(c.id);
      li.remove();
      if (c.id === navigation.currentId) {
        navigation.reset();
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
// list row and the cached conversation object, without a reload.
function applyTitle(id, title) {
  const c = state.convs.find((x) => x.id === id);
  if (c) c.title = title;
  const row = document.querySelector(`#conv-list .conv-item[data-id="${CSS.escape(id)}"]`);
  if (row) row.querySelector(".conv-title").textContent = title;
  if (id === navigation.currentId && navigation.currentConv) {
    navigation.updateConversation({ ...navigation.currentConv, title });
    $("#topbar-title").textContent = title;
  }
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

async function firstConversationAfterRefresh() {
  await loadConversations();
  return state.convs[0]?.id || "";
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
    li.classList.toggle("active", li.dataset.id === navigation.currentId);
  }
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

// updateCtxMeter fills the compact footnote below the composer: the freshest
// assistant total (prompt+completion of the last reply, an estimate where
// the endpoint reported nothing) against the context window carried by
// the conversation itself — the window is a model property, so it travels
// with the chat, not with meta — "71.5K tokens · 37% context". With no known window it
// reduces to the count alone; with no usage figure at all there is
// nothing to say.
function updateCtxMeter(used) {
  const el = $("#ctx-meter");
  if (!el) return;
  if (used > 0) navigation.rememberUsage(used); // remembered for a model switch
  el.classList.remove("ctx-meter-full"); // a live figure replaces any refusal
  const max = (navigation.currentConv && navigation.currentConv.max_context_tokens) || 0;
  el.classList.toggle("ctx-meter-high", max > 0 && used / max >= 0.85);
  if (!(used > 0)) {
    el.hidden = true;
    return;
  }
  if (!max) {
    el.textContent = `${fmtTokens(used)} tokens`;
    el.title = "context tokens used — the endpoint reports no window";
  } else {
    const pct = Math.max(1, Math.round((used / max) * 100));
    el.textContent = `${fmtTokens(used)} tokens · ${pct}% context`;
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

// The Enter/Shift+Enter hint is desktop-only furniture: phones have no
// Shift key to spare, and the long string wraps the composer to two
// lines on narrow screens. Width is re-checked whenever mqPhone flips,
// not just at render — a resized window should behave like CSS does.
function syncPlaceholder() {
  $("#composer-input").placeholder = navigation.currentId
    ? (isPhone()
        ? "Message Mullsmith…"
        : "Message Mullsmith… (Enter sends, Shift+Enter newline)")
    : "Start or pick a chat first";
}

function syncComposerButtons() {
  const busy = Boolean(generation.current);
  // A pending navigation owns the screen (review3 issue 2): the thread on
  // display is about to be replaced, so neither Send may address it. The
  // draft stays untouched while the destination loads or fails.
  const nav = Boolean(navigation.navPendingId);
  // Recovery from an uncertain send (review3 finding 4): until the reconcile
  // lands, this thread's true contents are unknown — nothing new may be
  // addressed to it.
  const rec = navigation.reconcileId !== "" && navigation.reconcileId === navigation.currentId;
  const send = $("#btn-send");
  const stop = $("#btn-stop");
  // Stop swaps into Send's slot while generating: a disabled-but-present
  // Send crowds the row next to the stop square (squeezing the textarea,
  // most visibly on phones). Hiding it makes the replacement literal —
  // and measured, so the row cannot shift: the square takes Send's exact
  // width (measured live, because "Send" is font- and locale-dependent).
  if (busy && !send.hidden) stop.style.minWidth = send.offsetWidth + "px";
  send.hidden = busy;
  send.disabled = busy || nav || rec || !navigation.currentId;
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

// Reserve only scroll-end space: the thread itself fills the whole chat
// area and scrolls behind the floating controls. Observe the entire stack
// so wrapped attachments, context notices and multiline drafts stay clear.
function observeComposerSize() {
  const stack = $("#composer-stack");
  const thread = $("#thread");
  const main = $(".chat-main");
  const observer = new ResizeObserver(() => {
    const atBottom = thread.scrollHeight - thread.scrollTop - thread.clientHeight < 90;
    main.style.setProperty("--composer-clearance", stack.getBoundingClientRect().height + "px");
    if (atBottom) thread.scrollTop = thread.scrollHeight;
  });
  observer.observe(stack);
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
  if (!navigation.currentId || navigation.navPendingId || generation.current ||
      navigation.reconcileId === navigation.currentId || !text.trim()) return;
  if (state.pending.some((f) => f.uploading)) {
    toast("Still uploading — give it a moment.", false);
    return;
  }
  // Capture the addressee BEFORE anything async (finding 1): the URL, the
  // tray clear, the reorder and the uncertain-acceptance reconcile all
  // speak of THIS conversation, never of whatever is on screen by then.
  const convId = navigation.currentId;
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
      if (navigation.currentId === convId && navOwner.isCurrent(view)) {
        input.value = text;
        autosizeComposer();
      }
      toast("Connection lost before the reply started — reloading to check it was saved.", false);
      beginReconcile(convId, view);
    }
  );
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
  $("#settings-email-password").value = "";
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
  $("#settings-email-password").value = "";
  syncEmailVerification();
  $("#form-password").reset();
  // email_features: the field keeps showing the CONFIRMED address; a
  // pending change is a notice with its own escape hatch, not a value.
  const pendingAddr = me.unconfirmed_email || "";
  show($("#settings-email-pending"), !!pendingAddr);
  if (pendingAddr) $("#pending-email-addr").textContent = pendingAddr;
}

function emailAddressChanged() {
  return $("#settings-email").value.trim().toLowerCase() !== (state.me?.email || "");
}

function syncEmailVerification() {
  const changed = emailAddressChanged();
  show($("#settings-email-verification"), changed);
  const password = $("#settings-email-password");
  password.disabled = !changed;
  password.required = changed;
  if (!changed) password.value = "";
}

// Both save buttons double as "done": on SUCCESS the panel closes and the
// chat the user was reading comes back (same navigation as the X button).
// A failed save deliberately keeps the panel open so nothing typed is lost.
function leaveSettings() {
  location.hash = navigation.currentId ? `#/c/${navigation.currentId}` : "";
}

async function onSettingsSave(ev) {
  ev.preventDefault();
  syncEmailVerification();
  const password = $("#settings-email-password");
  const update = {
    name: $("#settings-name").value.trim(),
    email: $("#settings-email").value.trim(),
    default_system_prompt: $("#settings-prompt").value.trim(),
  };
  if (emailAddressChanged()) {
    if (!password.value) {
      toast("Enter your current password to change your email address.", true);
      password.focus();
      return;
    }
    update.current_password = password.value;
  }
  try {
    state.me = await api.updateMe(update);
    leaveSettings();
    if (state.me.unconfirmed_email) {
      toast("Confirmation link sent to " + state.me.unconfirmed_email, false);
    } else {
      toast("Settings saved", false);
    }
  } catch (err) {
    handleErr(err, "save settings");
  } finally {
    password.value = "";
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
    state.me.unconfirmed_email = "";
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
let filesCursor = "";
let filesLoading = false;
let filesSeen = new Set();

// Cursor pages share the current tab's sequence. A reset supersedes every
// older page; failed pages retain their cursor so Load more can retry.
async function loadFiles(reset) {
  if (!reset && (filesLoading || !filesCursor)) return;
  const gen = reset ? ++filesLoadSeq : filesLoadSeq;
  const more = $("#btn-files-more");
  if (reset) {
    $("#files-list").replaceChildren();
    filesCursor = "";
    filesSeen = new Set();
    show(more, false);
  }
  filesLoading = true;
  more.disabled = true;
  try {
    const data = await api.listFiles(state.filesKind, filesCursor);
    if (gen !== filesLoadSeq) return;
    const list = $("#files-list");
    for (const f of data.items || []) {
      if (filesSeen.has(f.path)) continue;
      filesSeen.add(f.path);
      list.appendChild(fileRow(f));
    }
    filesCursor = data.next_cursor || "";
    show(more, Boolean(filesCursor));
    const q = data.quota || {};
    const used = q.used_bytes || 0;
    const total = q.total_bytes || 0;
    $("#quota-fill").style.width = (total > 0 ? Math.min(100, (used / total) * 100) : 0).toFixed(1) + "%";
    $("#quota-text").textContent = q.entry_limit_reached
      ? "Scratchpad entry limit exceeded — delete files or directories to make room."
      : `${humanSize(used)} of ${humanSize(total)} used`;
    $("#quota-fill").parentElement.hidden = !!q.entry_limit_reached;
  } finally {
    if (gen === filesLoadSeq) {
      filesLoading = false;
      more.disabled = false;
    }
  }
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

function renderConversation() {
  const c = navigation.currentId ? navigation.currentConv : null;
  const title = $("#topbar-title");
  title.textContent = c?.title || "";
  show(title, Boolean(c));
  const sel = $("#model-picker");
  sel.disabled = !navigation.currentId;
  if (c) {
    // Show the model ACTUALLY in use. The server already resolves a
    // model the config no longer offers to the default, so the name is
    // always one of the picker's own options.
    const active = c.model || state.meta.default_model || "";
    sel.value = active;
  }

  const input = $("#composer-input");
  input.disabled = !navigation.currentId;
  syncPlaceholder();
  if (!navigation.currentId) input.value = "";
  syncComposerButtons();
  // Each dropdown entry describes ITS model: opening a conversation on
  // another model must rebuild the choice list, not just re-point the
  // previous model's options.
  populateThinkingChoices();
  syncThinkingSelect();

  if (!navigation.currentId) {
    navigation.updateConversation(null);
    navigation.rememberUsage(0); // the meter speaks for one thread only
    $("#thread").replaceChildren(initialEmptyState());
    updateCtxMeter(0); // no thread: no meter
  }
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
    navigation.requestFocus();
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
  if (!navigation.currentId) return;
  const convId = navigation.currentId; // the PATCH targets the chat on screen
  const model = ev.target.value;
  const epoch = navOwner.current(); // view epoch at click time (C5)
  // Identity equality alone admits an obsolete commit after A → B → A;
  // the epoch moves on every navigation, return included.
  const owns = () => navOwner.isCurrent(epoch) && navigation.currentId === convId;
  // .catch(…) before .then: one block's unexpected throw must not poison
  // the queue for every later pick.
  settingsPatch = settingsPatch.catch(() => {}).then(async () => {
    try {
      const updated = await api.patchConversation(convId, { model });
      // A PATCH that lands after navigation describes ANOTHER conversation —
      // committing it would put that row's payload on this screen (finding 1).
      if (!owns()) return;
      navigation.updateConversation(updated);
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
    updateCtxMeter(navigation.lastCtxUsed || 0);
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
  const active = (navigation.currentConv && navigation.currentConv.model)
    || state.meta.default_model || "";
  return th.choices_by_model[active] || [];
}

// Build the dropdown from authenticated /api/v1/client-config. The row APPEARS whenever the
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
  sel.disabled = !navigation.currentId || choices.length < 2;
  sel.title = (navigation.currentId && choices.length < 2)
    ? "this model has no thinking to choose — switch to a thinking model"
    : "Thinking level for this conversation";
  if (!navigation.currentId) return;
  const want = (navigation.currentConv && navigation.currentConv.thinking) || "default";
  sel.value = [...sel.options].some((o) => o.value === want) ? want : "default";
}

async function onThinkingChange(ev) {
  if (!navigation.currentId) return;
  const convId = navigation.currentId; // identity guard, as in onModelChange
  const choice = ev.target.value;
  const before = (navigation.currentConv && navigation.currentConv.thinking) || "default";
  const label = ev.target.selectedOptions[0]
    ? ev.target.selectedOptions[0].textContent : choice;
  const epoch = navOwner.current(); // view epoch at click time (C5)
  const owns = () => navOwner.isCurrent(epoch) && navigation.currentId === convId;
  // Same single queue as the model picker (C5): model and thinking picks
  // for one conversation commit in click order, never racing each other.
  settingsPatch = settingsPatch.catch(() => {}).then(async () => {
    try {
      const updated = await api.patchConversation(convId, { thinking: choice });
      if (!owns()) return; // navigated mid-PATCH (same ID under a newer epoch too)
      navigation.updateConversation(updated);
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
    return selectConversation(m[1]);
  } else if ((navigation.currentId || navigation.navPendingId) && location.hash === "") {
    // Leaving for the blank route retires EVERYTHING aimed at a thread:
    // a navigation whose metadata or page is still in flight would
    // otherwise commit an identity the user has just walked away from,
    // with the URL showing nothing (C5). bump() kills those loaders;
    // the pending marker goes with them.
    navOwner.bump();
      abandonInflight();
    navigation.reset();
    renderConversation();
  }
}

async function boot() {
  applyTheme();
  observeComposerSize();
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
      if (navigation.currentId && res.ids.includes(navigation.currentId)) {
        navigation.reset();
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
  $("#btn-add").addEventListener("pointerdown", (ev) => {
    // Keep an already-open soft keyboard up by preventing the button from
    // taking focus. Its click still toggles the menu; keyboard navigation
    // and taps when the composer is unfocused retain their normal behaviour.
    if (ev.button === 0 && document.activeElement === $("#composer-input")) ev.preventDefault();
  });
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
  $("#btn-files-more").addEventListener("click", () => {
    loadFiles(false).catch((e) => handleErr(e, "more files"));
  });
  $("#btn-files-close").addEventListener("click", () => {
    location.hash = navigation.currentId ? `#/c/${navigation.currentId}` : "";
  });
  $("#btn-settings").addEventListener("click", () => {
    location.hash = "#/settings";
  });
  $("#btn-settings-close").addEventListener("click", () => {
    location.hash = navigation.currentId ? `#/c/${navigation.currentId}` : "";
  });
  $("#form-profile").addEventListener("submit", onSettingsSave);
  $("#settings-email").addEventListener("input", syncEmailVerification);
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
    state.meta = await api.authConfig();
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
    await enterChat(handled || me);
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
  state, navigation, generation, navOwner, listOwner, selectConversation, startGeneration, abandonInflight,
  sendFromComposer, regenerateFrom, onHashChange, onModelChange, onThinkingChange, earlierButton,
  newChat, renderConversation,
};
