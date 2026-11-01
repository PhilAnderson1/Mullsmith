// Shared file display formatting and download-link validation.
export function humanSize(n) {
  if (typeof n !== "number" || !isFinite(n) || n < 0) return "";
  if (n >= 1 << 30) return (n / 2 ** 30).toFixed(1) + " GiB";
  if (n >= 1 << 20) return (n / 2 ** 20).toFixed(1) + " MiB";
  if (n >= 1 << 10) return (n / 2 ** 10).toFixed(1) + " KiB";
  return n + " B";
}

export function fileApiUrl(u) {
  return typeof u === "string" && u.startsWith("/api/v1/files/") && !/[\s"']/.test(u) ? u : null;
}
