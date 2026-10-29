// Server-Sent Events over fetch POST (spec §6.1): the chat endpoints stream
// frames like
//
//   event: token
//   data: {"t":"…"}
//
// with ":" comment lines used as keepalive pings. fetch + ReadableStream is
// used instead of EventSource because the requests are POSTs with a CSRF
// header.

import { ApiError, cookieValue } from "/assets/app/api.js";

/**
 * POST a JSON body and stream the SSE reply into `handlers`.
 * Resolves when the stream ends; rejects with ApiError (JSON error envelope)
 * or the abort/fetch failure.
 */
export async function streamChat(path, body, handlers, signal) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const csrf = cookieValue("mullsmith_csrf");
  if (csrf) headers["X-Mullsmith-CSRF"] = csrf;

  const resp = await fetch(path, {
    method: "POST",
    headers,
    credentials: "same-origin",
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal,
  });

  if (!resp.ok) {
    let data = null;
    try {
      data = await resp.json();
    } catch {
      /* non-JSON error body */
    }
    const err = (data && data.error) || {};
    throw new ApiError(resp.status, err.code, err.message, err.request_id);
  }

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf("\n\n")) >= 0) {
      handleFrame(buf.slice(0, idx), handlers);
      buf = buf.slice(idx + 2);
    }
  }
  if (buf.trim() !== "") handleFrame(buf, handlers); // trailing frame at EOF
}

function handleFrame(frame, h) {
  let event = "message";
  const dataLines = [];
  for (let line of frame.split("\n")) {
    if (line.endsWith("\r")) line = line.slice(0, -1);
    if (line.startsWith(":")) continue; // ping comment
    else if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
  }
  if (dataLines.length === 0) return;
  let data;
  try {
    data = JSON.parse(dataLines.join("\n"));
  } catch {
    return; // malformed frame: skip (spec §6.2 leniency)
  }
  switch (event) {
    case "start":
      h.onStart && h.onStart(data);
      break;
    case "token":
      h.onToken && h.onToken(data.t || "");
      break;
    case "reasoning":
      h.onReasoning && h.onReasoning(data.t || "");
      break;
    case "tool_start":
      h.onToolStart && h.onToolStart(data);
      break;
    case "tool_args":
      h.onToolArgs && h.onToolArgs(data);
      break;
    case "tool_end":
      h.onToolEnd && h.onToolEnd(data);
      break;
    case "awaiting_approval":
      h.onAwaitingApproval && h.onAwaitingApproval(data);
      break;
    case "error":
      h.onError && h.onError(data);
      break;
    case "title":
      h.onTitle && h.onTitle(data);
      break;
    case "done":
      h.onDone && h.onDone(data);
      break;
  }
}
