// SSE frame parsing, extracted from sse.js (finding 8): the wire semantics
// — event/data lines, ":" comment pings, malformed frames skipped per spec
// §6.2 leniency, the trailing frame at EOF — are exactly what multi-round
// tool streams depend on, and they deserve unit tests without a network,
// DOM or fetch mocks. Pure and import-free.

// handleFrame dispatches one "\n\n"-separated frame into `handlers` and
// reports whether an event was dispatched (false: comment-only, no data
// line, or malformed JSON — all silently ignored, as the spec prescribes).
export function handleFrame(frame, h) {
  let event = "message";
  const dataLines = [];
  for (let line of frame.split("\n")) {
    if (line.endsWith("\r")) line = line.slice(0, -1);
    if (line.startsWith(":")) continue; // ping comment
    else if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
  }
  if (dataLines.length === 0) return false;
  let data;
  try {
    data = JSON.parse(dataLines.join("\n"));
  } catch {
    return false; // malformed frame: skip (spec §6.2 leniency)
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
    default:
      return false;
  }
  return true;
}

// splitFrames consumes whatever "\n\n"-terminated frames have accumulated
// in `buf` and returns { rest, frames }: the unconsumed tail plus the
// complete frame texts. EOF flushing (the trailing frame without a blank
// line) is the CALLER's decision — streamChat applies handleFrame to the
// remainder only when it is not whitespace.
export function splitFrames(buf) {
  const frames = [];
  let idx;
  while ((idx = buf.indexOf("\n\n")) >= 0) {
    frames.push(buf.slice(0, idx));
    buf = buf.slice(idx + 2);
  }
  return { rest: buf, frames };
}
