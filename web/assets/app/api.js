// Minimal JSON API client (spec §5): same-origin cookies, double-submit CSRF
// read fresh from the cookie before EVERY request (the server may re-issue it
// on any response), one ApiError type carrying the server envelope.

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message || `HTTP ${status}`);
    this.status = status;
    this.code = code || "";
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
    throw new ApiError(resp.status, err.code, err.message);
  }
  return data;
}

async function request(method, path, body) {
  const headers = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  const csrf = cookieValue("mullsmith_csrf");
  if (csrf) headers["X-Mullsmith-CSRF"] = csrf;

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
  const csrf = cookieValue("mullsmith_csrf");
  if (csrf) headers["X-Mullsmith-CSRF"] = csrf;
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

  // email_features (plan §6): all POST like the rest of the API; the token
  // values arrive via the SPA hash routes, never as GET query params.
  forgotPassword: (email) => request("POST", "/api/v1/auth/forgot-password", { email }),
  resetPassword: (token, password) =>
    request("POST", "/api/v1/auth/reset-password", { token, password }),
  confirmEmail: (token) => request("POST", "/api/v1/auth/confirm-email", { token }),
  resendConfirm: (email) => request("POST", "/api/v1/auth/resend-confirm", { email }),
  cancelEmail: () => request("POST", "/api/v1/auth/me/cancel-email", {}),
  deleteAccount: () => request("POST", "/api/v1/auth/me/delete"),

  listConversations: (q, cursor) => {
    const p = new URLSearchParams();
    if (q) p.set("q", q);
    if (cursor) p.set("cursor", cursor);
    const qs = p.toString();
    return request("GET", "/api/v1/conversations" + (qs ? "?" + qs : ""));
  },
  createConversation: (title, model) =>
    request("POST", "/api/v1/conversations", { title, model: model || "" }),
  getConversation: (id) => request("GET", `/api/v1/conversations/${encodeURIComponent(id)}`),
  patchConversation: (id, patch) =>
    request("PATCH", `/api/v1/conversations/${encodeURIComponent(id)}`, patch),
  deleteConversation: (id) => request("DELETE", `/api/v1/conversations/${encodeURIComponent(id)}`),
  pruneConversations: (olderThan) =>
    request("DELETE", `/api/v1/conversations?older_than=${encodeURIComponent(olderThan)}`),
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
  resolveApproval: (genId, approvalId, approve, remember = false) =>
    request("POST", `/api/v1/generations/${encodeURIComponent(genId)}/approvals`, {
      approval_id: approvalId,
      approve,
      remember,
    }),

  uploadFile,
  // Settings page: profile details + default prompt in one PATCH; the
  // password endpoint is separate because it revokes other sessions.
  updateMe: (patch) => request("PATCH", "/api/v1/auth/me", patch),
  changePassword: (current, next) =>
    request("POST", "/api/v1/auth/password", { current, new: next }),
  // kind is the plural directory name: uploads | artifacts | work ("" = all).
  listFiles: (kind) => request("GET", "/api/v1/files" + (kind ? "?kind=" + encodeURIComponent(kind) : "")),
  // The one file identity is the scratchpad-relative path — for the three
  // layout directories alike. No ids, no cursors: the disk is the truth.
  deleteFile: (path) => request("DELETE", "/api/v1/files?path=" + encodeURIComponent(path)),
  // Bulk housekeeping: aged content of one kind ("" = all three layout
  // dirs). Tokens are the conversation-prune ones: 1d|1w|1mo|1y|all.
  purgeFiles: (kind, olderThan) => {
    const p = new URLSearchParams({ older_than: olderThan });
    if (kind) p.set("kind", kind);
    return request("DELETE", "/api/v1/files?" + p.toString());
  },
};
