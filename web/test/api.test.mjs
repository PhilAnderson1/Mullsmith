import { test } from "node:test";
import assert from "node:assert/strict";
import { api, apiError, ApiError, fetchAPI } from "../assets/app/api.js";

test("shared transport refreshes CSRF and leaves multipart and successful streams intact", async () => {
  globalThis.document = { cookie: "mullsmith_csrf=first" };
  const calls = [];
  const response = { ok: true, body: {}, json() { throw new Error("stream consumed"); } };
  globalThis.fetch = async (path, options) => { calls.push({ path, options }); return response; };
  assert.equal(await fetchAPI("/stream", { method: "POST", json: { content: "hello" } }), response);
  assert.equal(calls[0].options.headers["X-Mullsmith-CSRF"], "first");
  assert.equal(calls[0].options.credentials, "same-origin");
  assert.equal(calls[0].options.body, '{"content":"hello"}');
  document.cookie = "mullsmith_csrf=second";
  const body = new FormData();
  body.append("file", "data");
  await fetchAPI("/upload", { method: "POST", body });
  assert.equal(calls[1].options.body, body);
  assert.equal(calls[1].options.headers["X-Mullsmith-CSRF"], "second");
  assert.equal(calls[1].options.headers["Content-Type"], undefined);
});

test("JSON requests and streaming share typed envelope errors and malformed-error fallback", async () => {
  globalThis.document = { cookie: "" };
  globalThis.fetch = async () => ({ ok: false, status: 409, json: async () => ({ error: { code: "generating", message: "busy" } }) });
  await assert.rejects(api.me(), (e) => e instanceof ApiError && e.status === 409 && e.code === "generating" && e.message === "busy");
  const e = await apiError({ status: 502, json: async () => { throw new Error("HTML response"); } });
  assert.equal(e.status, 502);
  assert.equal(e.code, "");
  assert.equal(e.message, "HTTP 502");
});
