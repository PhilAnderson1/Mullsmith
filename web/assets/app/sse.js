// Server-Sent Events over fetch POST (spec §6.1): the chat endpoints stream
// frames like
//
//   event: token
//   data: {"t":"…"}
//
// with ":" comment lines used as keepalive pings. fetch + ReadableStream is
// used instead of EventSource because the requests are POSTs with a CSRF
// header.

import { apiError, fetchAPI } from "/assets/app/api.js";
import { handleFrame, splitFrames } from "/assets/app/sseparse.js";

/**
 * POST a JSON body and stream the SSE reply into `handlers`.
 * Resolves when the stream ends; rejects with ApiError (JSON error envelope)
 * or the abort/fetch failure.
 */
export async function streamChat(path, body, handlers, signal) {
  const resp = await fetchAPI(path, { method: "POST", json: body, signal });
  if (!resp.ok) throw await apiError(resp);

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const split = splitFrames(buf);
    for (const frame of split.frames) handleFrame(frame, handlers);
    buf = split.rest;
  }
  if (buf.trim() !== "") handleFrame(buf, handlers); // trailing frame at EOF
}
