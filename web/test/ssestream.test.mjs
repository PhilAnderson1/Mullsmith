// Framing through the REAL reader path (c7.md Stage 5): streamChat from
// sse.js is driven with a scripted fetch whose body arrives as arbitrary
// byte chunks. sseparse.test.mjs proves the pure functions; this proves
// the loop that assembles a stream from what a network actually delivers —
// LF frames, CRLF frames, boundaries cut mid-sequence by a chunk edge,
// multi-byte text split across chunks, and the EOF tail with no final
// blank line. Run: make test-js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

// sse.js imports api.js by absolute /assets/app/*.js URL (the server's
// stamp rewriter requires it); Node needs the same resolution hook app.js
// uses.
register("./resolver.mjs", import.meta.url);

// cookieValue reads document.cookie during the request; a bare cookie
// stub is all the DOM this loop touches.
globalThis.document = { cookie: "" };
const { streamChat } = await import("../assets/app/sse.js");

// serve fetches as streamChat calls it: text split into byte chunks
// exactly as listed, so a test controls every chunk boundary.
function serve(text, chunks) {
  const bytes = new TextEncoder().encode(text);
  let off = 0;
  globalThis.fetch = async () => ({
    ok: true,
    body: {
      getReader() {
        return {
          async read() {
            if (off >= bytes.length) return { done: true };
            const n = chunks.shift() ?? bytes.length - off;
            const value = bytes.subarray(off, off + n);
            off += n;
            return { value, done: false };
          },
        };
      },
    },
  });
}

function recorder() {
  const seen = [];
  return {
    seen,
    h: {
      onStart: (d) => seen.push(["start", d.message_id]),
      onRound: (d) => seen.push(["round", d.message_id]),
      onToken: (t) => seen.push(["token", t]),
      onDone: (d) => seen.push(["done", d.status]),
    },
  };
}

// The C7 event taxonomy through the reader: start, round, tokens, done.
const transcript = (sep) =>
  [
    `event: start\ndata: {"generation_id":"g1","message_id":"m1","round":0}`,
    `event: token\ndata: {"t":"hello"}`,
    `event: round\ndata: {"generation_id":"g1","message_id":"m2","round":1}`,
    `event: token\ndata: {"t":" again"}`,
    `event: done\ndata: {"generation_id":"g1","status":"complete","rounds":[]}`,
  ].map((f) => (sep === "\r\n\r\n" ? f.replace(/\n/g, "\r\n") : f) + sep).join("")
    + ": trailing comment without final terminator";

// Note: recorder also needs handleFrame to dispatch "round" — the switch
// in sseparse.js must know it (C7). If a frame is dropped, the seen list
// here is the failure report.

test("LF frames arrive whole", async () => {
  const { seen, h } = recorder();
  serve(transcript("\n\n"), []);
  await streamChat("/api/v1/conversations/c/messages", {}, h);
  assert.deepEqual(seen, [
    ["start", "m1"],
    ["token", "hello"],
    ["round", "m2"],
    ["token", " again"],
    ["done", "complete"],
  ]);
});

test("chunk boundaries cutting frames and terminators mid-sequence still stream everything", async () => {
  const { seen, h } = recorder();
  const text = transcript("\r\n\r\n");
  // One byte at a time: every boundary — inside lines, inside "\r\n\r\n",
  // inside JSON — lands on a chunk edge.
  serve(text, Array.from({ length: text.length }, () => 1));
  await streamChat("/x", {}, h);
  assert.deepEqual(seen.map(([k]) => k), ["start", "token", "round", "token", "done"]);
});

test("a multi-byte token split across chunks is not mangled", async () => {
  const { seen, h } = recorder();
  const frameText = 'event: token\ndata: {"t":"é中"}\n\n';
  const bytes = new TextEncoder().encode(frameText);
  const cut = bytes.indexOf(0xa9) + 1; // between the two UTF-8 bytes of é
  let off = 0;
  globalThis.fetch = async () => ({
    ok: true,
    body: {
      getReader() {
        return {
          async read() {
            if (off >= bytes.length) return { done: true };
            const n = off === 0 ? cut : bytes.length - off;
            const value = bytes.subarray(off, off + n);
            off += n;
            return { value, done: false };
          },
        };
      },
    },
  });
  await streamChat("/x", { b: 1 }, h);
  assert.deepEqual(seen, [["token", "é中"]]);
});

test("the EOF tail (no final blank line) still dispatches", async () => {
  const { seen, h } = recorder();
  serve('event: done\ndata: {"status":"aborted"}', []);
  await streamChat("/x", {}, h);
  assert.deepEqual(seen, [["done", "aborted"]]);
});
