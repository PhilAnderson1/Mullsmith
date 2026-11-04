// The thread projection (C7): ONE description of a round and a tool card,
// rendered from the live SSE event AND from the stored history row. Before
// this module the live view built cards from tool_end payloads while the
// reload re-derived them by parsing the middle-elided model-facing result
// — two renderers, guaranteed to disagree (a cut JSON string lost every
// link; denials lost their reason). Anything about how a round or a card
// LOOKS lives here, and app.js owns only DOM assembly and state.
//
// Import-free and network-free: the browser loads it as a module, and the
// node test suites drive the same functions the shell runs.

// --- tool cards (spec §7, §10.2) -------------------------------------------------

export const TOOL_STATE_LABELS = {
  ok: "done",
  denied: "denied",
  error: "failed",
  running: "interrupted",
  approved: "ran",
  pending_approval: "approval expired",
};

export function toolStateLabel(status) {
  return TOOL_STATE_LABELS[status] || status || "unknown";
}

export function formatToolArgs(raw) {
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
export function safeLink(u) {
  if (typeof u !== "string") return null;
  try {
    const url = new URL(u, location.origin);
    if (url.protocol !== "http:" && url.protocol !== "https:") return null;
    return url.href;
  } catch {
    return null;
  }
}

export function toolCardNode(callId, tool) {
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

// toolCardsHost is where cards live on a bubble: OUTSIDE .msg-body, which
// finishGeneration clears when the markdown answer lands. Prose introduces
// this round's calls, so keep it above their cards in both live and history views.
export function toolCardsHost(el) {
  let host = el.querySelector(".tool-cards");
  if (!host) {
    host = document.createElement("div");
    host.className = "tool-cards";
    el.querySelector(".msg-body").after(host);
  }
  return host;
}

export function setToolSummary(card, text) {
  if (!text) return;
  let p = card.querySelector(".tool-summary");
  if (!p) {
    p = document.createElement("p");
    p.className = "tool-summary";
    card.querySelector(".tool-results").before(p);
  }
  p.textContent = text;
}

export function renderToolResults(container, data) {
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
    title.textContent = String(it.title || href).slice(0, 160);
    const host = document.createElement("span");
    host.className = "tool-link-host";
    host.textContent = new URL(href).host;
    a.append(title, host);
    container.append(a);
  }
}

// --- the card projection ----------------------------------------------------------

// callView is THE shape both views converge on: one tool call as the card
// needs it. A live tool_end event and a stored tool_calls row produce the
// SAME view because the server clamped the card once and put the identical
// payload in both places (tool_calls.display, spec §6.1).
//

function parseJSON(raw) {
  if (typeof raw !== "string" || raw === "") return null;
  try {
    return JSON.parse(raw);
  } catch {
    return null; // truncated mid-JSON: nothing to read, never a crash
  }
}

export function callFromEvent(d) {
  return {
    id: d.call_id || "",
    tool: d.tool || "",
    status: d.status || "ok",
    summary: d.summary || "",
    data: d.data ?? null,
    truncated: Boolean(d.truncated),
  };
}

export function callFromStored(c) {
  const display = parseJSON(c.display) || {};
  return {
    id: c.id || "",
    tool: c.tool || "",
    status: c.status || "ok",
    summary: display.summary || "",
    data: display.data ?? null,
    truncated: Boolean(display.truncated),
  };
}

// paintCallOutcome renders a settled card — live tool_end and stored reload
// arrive here through the same four lines, which is the whole point.
export function paintCallOutcome(card, view) {
  card.classList.remove("st-running", "st-awaiting");
  card.classList.add("st-" + (view.status || "ok"));
  card.querySelector(".tool-state").textContent = toolStateLabel(view.status);
  const row = card.querySelector(".approval-row"); // answered: buttons expire
  if (row) row.remove();
  const summary = [view.summary, view.truncated ? "Some tool details were omitted." : ""].filter(Boolean).join(" · ");
  if (summary) setToolSummary(card, summary);
  if (view.data) renderToolResults(card.querySelector(".tool-results"), view.data);
}

// buildCallCard is the reload path's whole card: node, args, outcome.
export function buildCallCard(view, argsRaw) {
  const card = toolCardNode(view.id, view.tool);
  card.classList.remove("st-running");
  if (argsRaw) {
    const argsEl = card.querySelector(".tool-args");
    argsEl.textContent = formatToolArgs(argsRaw);
    argsEl.hidden = false;
  }
  paintCallOutcome(card, view);
  return card;
}

// files_share carries the download information in its display payload.
export function sharedFileFor(view) {
  if (view.tool !== "files_share" || view.status !== "ok") return null;
  const file = view.data?.file;
  return file && typeof file === "object" ? file : null;
}

// display_image carries a compact, durable artifact reference. The image
// bytes stay on disk; live and history views consume the same projection.
export function displayedImageFor(view) {
  if (view.tool !== "display_image" || view.status !== "ok") return null;
  const image = view.data?.image;
  return image && typeof image === "object" ? image : null;
}

// --- round status -------------------------------------------------------------------

// roundStatusNote is what a finished round says beneath its answer, live
// AND on reload — one function, so the reload cannot contradict the
// stream. Server-authored notes (messages.status_note, echoed by the done
// event's rounds[] entry) always win: they name the reason the loop
// stopped, which no generic label can know.
export function roundStatusNote(status, storedNote, errorMessage) {
  if (storedNote) return storedNote;
  if (status === "aborted") return "Stopped before finishing.";
  if (status === "error") {
    return errorMessage || "The model endpoint failed for this reply.";
  }
  if (status === "lost") return "Connection lost — the reply was interrupted.";
  if (status && status !== "complete") return `status: ${status}`;
  return "";
}
