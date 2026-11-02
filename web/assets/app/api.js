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

async function responseJSON(resp) {
  try { return await resp.json(); } catch { return null; }
}

export async function apiError(resp) {
  const data = await responseJSON(resp);
  const err = data?.error || {};
  return new ApiError(resp.status, err.code, err.message);
}

// Shared transport policy. Multipart bodies keep the browser's boundary;
// successful stream bodies remain untouched for the SSE reader.
export function fetchAPI(path, { json, ...options } = {}) {
  const headers = { ...options.headers };
  const csrf = cookieValue("mullsmith_csrf");
  if (csrf) headers["X-Mullsmith-CSRF"] = csrf;
  if (json !== undefined) {
    headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(json);
  }
  return fetch(path, { ...options, headers, credentials: "same-origin" });
}

async function request(method, path, body) {
  const resp = await fetchAPI(path, { method, json: body });
  if (!resp.ok) throw await apiError(resp);
  return resp.status === 204 ? null : responseJSON(resp);
}

async function uploadFile(file) {
  const body = new FormData();
  body.append("file", file);
  const resp = await fetchAPI("/api/v1/files", { method: "POST", body });
  if (!resp.ok) throw await apiError(resp);
  return responseJSON(resp);
}

export const api = {
  authConfig: () => request("GET", "/api/v1/auth/config"),
  clientConfig: () => request("GET", "/api/v1/client-config"),
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
  listFiles: (kind, cursor) => {
    const p = new URLSearchParams();
    if (kind) p.set("kind", kind);
    if (cursor) p.set("cursor", cursor);
    return request("GET", "/api/v1/files" + (p.size ? "?" + p.toString() : ""));
  },
  // The one file identity is the scratchpad-relative path — for the three
  // layout directories alike. File operations address paths; listing cursors
  // only describe a position in the modification-time/path order.
  deleteFile: (path) => request("DELETE", "/api/v1/files?path=" + encodeURIComponent(path)),
  // Bulk housekeeping: aged content of one kind ("" = all three layout
  // dirs). Tokens are the conversation-prune ones: 1d|1w|1mo|1y|all.
  purgeFiles: (kind, olderThan) => {
    const p = new URLSearchParams({ older_than: olderThan });
    if (kind) p.set("kind", kind);
    return request("DELETE", "/api/v1/files?" + p.toString());
  },
};
