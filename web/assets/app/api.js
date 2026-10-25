// Minimal JSON API client (spec §5): same-origin cookies, double-submit CSRF
// read fresh from the cookie before EVERY request (the server may re-issue it
// on any response), one ApiError type carrying the server envelope.

export class ApiError extends Error {
  constructor(status, code, message, requestId) {
    super(message || `HTTP ${status}`);
    this.status = status;
    this.code = code || "";
    this.requestId = requestId || "";
  }
}

export function cookieValue(name) {
  for (const pair of document.cookie.split("; ")) {
    if (pair.startsWith(name + "=")) {
      return decodeURIComponent(pair.slice(name.length + 1));
    }
  }
  return "";
}

async function finalize(resp) {
  if (resp.status === 204) return null;
  let data = null;
  try {
    data = await resp.json();
  } catch {
    /* non-JSON body */
  }
  if (!resp.ok) {
    const err = (data && data.error) || {};
    throw new ApiError(resp.status, err.code, err.message, err.request_id);
  }
  return data;
}

async function request(method, path, body) {
  const headers = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  const csrf = cookieValue("gabkeep_csrf");
  if (csrf) headers["X-Gabkeep-CSRF"] = csrf;

  const resp = await fetch(path, {
    method,
    headers,
    credentials: "same-origin",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  return finalize(resp);
}

// Multipart upload; the browser sets the Content-Type boundary, so only the
// CSRF header travels. Streams the file, no whole-body buffering on our side.
async function uploadFile(file) {
  const fd = new FormData();
  fd.append("file", file);
  const headers = {};
  const csrf = cookieValue("gabkeep_csrf");
  if (csrf) headers["X-Gabkeep-CSRF"] = csrf;
  const resp = await fetch("/api/v1/files", {
    method: "POST",
    headers,
    credentials: "same-origin",
    body: fd,
  });
  return finalize(resp);
}

export const api = {
  meta: () => request("GET", "/api/v1/meta"),
  me: () => request("GET", "/api/v1/auth/me"),
  login: (email, password, rememberMe) =>
    request("POST", "/api/v1/auth/login", { email, password, remember_me: rememberMe }),
  register: (payload) => request("POST", "/api/v1/auth/register", payload),
  logout: () => request("POST", "/api/v1/auth/logout"),

  listConversations: (q, cursor) => {
    const p = new URLSearchParams();
    if (q) p.set("q", q);
    if (cursor) p.set("cursor", cursor);
    const qs = p.toString();
    return request("GET", "/api/v1/conversations" + (qs ? "?" + qs : ""));
  },
  createConversation: (title) => request("POST", "/api/v1/conversations", { title }),
  getConversation: (id) => request("GET", `/api/v1/conversations/${encodeURIComponent(id)}`),
  patchConversation: (id, patch) =>
    request("PATCH", `/api/v1/conversations/${encodeURIComponent(id)}`, patch),
  deleteConversation: (id) => request("DELETE", `/api/v1/conversations/${encodeURIComponent(id)}`),
  listMessages: (id, cursor) => {
    const p = new URLSearchParams();
    if (cursor) p.set("cursor", cursor);
    const qs = p.toString();
    return request(
      "GET",
      `/api/v1/conversations/${encodeURIComponent(id)}/messages` + (qs ? "?" + qs : "")
    );
  },
  abortGeneration: (id) =>
    request("POST", `/api/v1/generations/${encodeURIComponent(id)}/abort`),
  resolveApproval: (genId, approvalId, approve) =>
    request("POST", `/api/v1/generations/${encodeURIComponent(genId)}/approvals`, {
      approval_id: approvalId,
      approve,
    }),

  uploadFile,
  listFiles: (kind, cursor) => {
    const p = new URLSearchParams();
    if (kind) p.set("kind", kind);
    if (cursor) p.set("cursor", cursor);
    const qs = p.toString();
    return request("GET", "/api/v1/files" + (qs ? "?" + qs : ""));
  },
  deleteFile: (id) => request("DELETE", `/api/v1/files/${encodeURIComponent(id)}`),
};
