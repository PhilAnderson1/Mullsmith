// Thread DOM assembly. This module has no application/session state; all
// lifecycle effects enter through explicit callbacks.
import { renderMarkdown } from "/assets/app/markdown.js";
import { humanSize, fileApiUrl } from "/assets/app/file-presenter.js";
import { callFromStored, buildCallCard, toolCardsHost, sharedFileFor, displayedImageFor, roundStatusNote, formatToolArgs, syncAssistantVisibility } from "/assets/app/thread.js";
export function createThreadRenderer({
  onRegenerate, onEarlier, onApproval, updateCtxMeter, recordActivity, toast, getMeta = () => ({})
}) {
  const $ = (selector) => document.querySelector(selector);
  const clone = (id) => $("#" + id).content.firstElementChild.cloneNode(true);
  const show = (el, on = true) => { el.hidden = !on; };

  const imageViewer = $("#image-viewer-dialog");
  const viewerImage = imageViewer?.querySelector(".image-viewer-image");
  const viewerCaption = imageViewer?.querySelector(".image-viewer-caption");
  if (typeof imageViewer?.addEventListener === "function") {
    imageViewer.addEventListener("click", (ev) => {
      if (ev.target === imageViewer) imageViewer.close();
    });
    imageViewer.addEventListener("close", () => {
      viewerImage?.removeAttribute("src");
    });
  }

  function openImageViewer(href, image) {
    if (!imageViewer || !viewerImage || !viewerCaption) return false;
    viewerImage.src = href;
    viewerImage.alt = image.caption || image.name || "Displayed image";
    viewerCaption.textContent = image.caption || image.name || "";
    viewerCaption.hidden = !viewerCaption.textContent;
    imageViewer.showModal();
    return true;
  }

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
    // placed after the round's prose like their live counterparts. A calls-only
    // assistant row has no prose: the empty body folds away.
    if (m.role === "assistant" && Array.isArray(m.tool_calls) && m.tool_calls.length) {
      const host = toolCardsHost(el);
      for (const c of m.tool_calls) {
        const view = callFromStored(c);
        const card = buildCallCard(view, c.arguments, getMeta()?.tool_cards);
        if (card) host.append(card);
        const file = sharedFileFor(view); // files_share ⇒ visible download card
        if (file) host.append(artifactCard(file));
        const image = displayedImageFor(view); // display_image ⇒ inline picture
        if (image) {
          const figure = displayImage(image);
          if (figure) host.append(figure);
        }
      }
    }
    if (m.role === "user" && Array.isArray(m.files) && m.files.length) {
      appendFileChips(el, m.files);
    }

    const note = roundStatusNote(m.status, m.status_note);
    if (note) showStatusNote(el, note);

    if (m.role === "assistant") {
      attachRegen(el);
      syncAssistantVisibility(el);
    }
    return el;
  }

  function showStatusNote(el, text) {
    const st = el.querySelector(".msg-status");
    st.textContent = text;
    show(st, true);
  }

  function lastContextUsed(items) {
    for (let i = items.length - 1; i >= 0; i--) {
      const m = items[i];
      if (m.role === "assistant" && m.token_count > 0) return m.token_count;
    }
    return 0;
  }

  function msgTps(completion, durationMS) {
    if (!(completion > 0) || !(durationMS > 0)) return null;
    const tps = (completion / durationMS) * 1000;
    return tps >= 100 ? String(Math.round(tps)) : tps.toFixed(1);
  }

  function tpsNote(el, completion, durationMS) {
    const tps = msgTps(completion, durationMS);
    if (!tps) return;
    const foot = el.querySelector(".msg-foot");
    const span = el.querySelector(".msg-tps");
    if (!foot || !span || span.textContent) return;
    span.textContent = `${tps} tok/s`;
    foot.classList.add("on"); // the row shows when it has a figure or a button
  }

  function appendUserBubble(text, files) {
    const el = msgNode({
      role: "user",
      content: text,
      status: "complete",
      created_at: new Date().toISOString(),
    });
    if (files && files.length) appendFileChips(el, files);
    recordActivity(); // newer than any history page already on the wire
    $("#thread").append(el);
    maybeScroll(true);
    return el;
  }

  function spinnerNode(st) {
    const span = document.createElement("span");
    span.className = "spinner";
    span.setAttribute("aria-hidden", "true");
    for (let i = 0; i < 3; i++) span.appendChild(document.createElement("i"));
    st.spinNode = span; // stopSpinner removes it from the DOM
    return span;
  }

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
        await onApproval(st.genId, d.approval_id, approve, remember);
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

  function displayImage(image) {
    const href = fileApiUrl(image.url);
    if (!href || !href.startsWith("/api/v1/files/image?")) return null;
    const figure = document.createElement("figure");
    figure.className = "display-image";
    const link = document.createElement("a");
    link.className = "display-image-link";
    link.href = href;
    link.setAttribute("aria-label", `Open ${image.name || "image"} at full size`);
    link.addEventListener("click", (ev) => {
      if (openImageViewer(href, image)) ev.preventDefault();
    });
    const img = document.createElement("img");
    img.alt = image.caption || image.name || "Displayed image";
    img.loading = "lazy";
    img.decoding = "async";
    link.append(img);
    const unavailable = document.createElement("div");
    unavailable.className = "display-image-unavailable muted";
    unavailable.textContent = "Image unavailable";
    unavailable.hidden = true;
    figure.append(link, unavailable);
    if (image.caption) {
      const caption = document.createElement("figcaption");
      caption.textContent = image.caption;
      figure.append(caption);
    }

    // Retry transient browser/network failures twice. Cache-busting prevents
    // a cached failure from making the replacement attempt meaningless.
    let failures = 0;
    img.addEventListener("error", () => {
      if (failures < 2 && img.isConnected) {
        failures++;
        setTimeout(() => {
          if (img.isConnected) img.src = `${href}&attempt=${failures}`;
        }, failures * 750);
        return;
      }
      link.remove();
      unavailable.hidden = false;
    });
    img.src = href;
    return figure;
  }

  function attachRegen(el) {
    const regen = el.querySelector(".regen-btn");
    if (!regen || regen.dataset.bound) return;
    regen.dataset.bound = "1";
    regen.addEventListener("click", () => onRegenerate(el, el.dataset.id));
  }

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
    syncAssistantVisibility(nodes.el);
  }

  function retireStrandedCards(st) {
    for (const key of Object.keys(st.cards || {})) {
      const c = st.cards[key];
      if (!c || !c.card || !c.card.isConnected) continue;
      if (!c.card.classList.contains("st-running") &&
          !c.card.classList.contains("st-awaiting")) continue;
      // Complete calls are announced only when execution reaches them. A
      // card still running here was interrupted during approval/execution.
      c.card.classList.remove("st-running", "st-awaiting");
      c.card.classList.add("st-lost");
      const state = c.card.querySelector(".tool-state");
      if (state) state.textContent = "interrupted";
      for (const ctl of c.card.querySelectorAll(".approval-row button, .approval-row input")) ctl.disabled = true;
    }
  }

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

  function scrollThreadToBottom() {
    const t = $("#thread");
    t.scrollTop = t.scrollHeight;
  }

  function isNearBottom() {
    const t = $("#thread");
    return t.scrollHeight - t.scrollTop - t.clientHeight < 90;
  }

  function maybeScroll(force = false) {
    if (force || isNearBottom()) scrollThreadToBottom();
  }

  function renderThread(data) {
    const thread = $("#thread");
    thread.replaceChildren();
    if (data.items.length === 0) {
      thread.appendChild(initialEmptyState());
    } else {
      if (data.next_cursor) thread.appendChild(onEarlier(data.next_cursor));
      // role='tool' rows carry model-facing payloads; the UI shows them as
      // tool cards attached to the assistant message that proposed the call.
      for (const m of data.items) thread.appendChild(msgNode(m));
    }
    refreshLastAssistant();
    updateCtxMeter(lastContextUsed(data.items));
    scrollThreadToBottom();
  }

  function initialEmptyState() {
    const el = document.createElement("div");
    el.className = "empty-state";
    const h = document.createElement("h2");
    h.textContent = "Mullsmith";
    const p1 = document.createElement("p");
    p1.textContent = "Pick a conversation on the left or start a new one.";
    el.append(h, p1);
    return el;
  }

  function openRound(st, msgId) {
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
  }
  // Every tool event names the round that owns its card.
  function cardHost(st, d) {
    const r = st.byId.get(d.message_id);
    if (!r) return null;
    // The tool card is now the progress indicator. Retire this round's
    // waiting dots without leaving their empty answer bubble underneath.
    if (r === st.cur) stopSpinner(st);
    syncRoundBody(r);
    return toolCardsHost(r.nodes.el);
  }

  function earlierButtonNode() {
    const button = document.createElement("button");
    button.className = "btn ghost wide load-earlier";
    button.textContent = "Load earlier messages";
    return button;
  }

  function prependPage(button, next, items) {
    const host = $("#thread");
    if (next) button.replaceWith(next);
    else button.remove();
    const ref = next ? next.nextSibling : host.firstChild;
    for (const m of items) host.insertBefore(msgNode(m), ref);
    refreshLastAssistant();
  }

  return {
    earlierButtonNode, prependPage, msgNode, appendUserBubble, appendFileChips,
    stopSpinner, ensureThinking, attachApproval, artifactCard, displayImage, attachRegen,
    syncRoundBody, settleRound, retireStrandedCards, refreshLastAssistant,
    syncAssistantVisibility,
    scrollThreadToBottom, maybeScroll, renderThread, initialEmptyState,
    openRound, cardHost,
  };
}
