package scratch

import (
	"path"
	"strings"
)

// MediaTypeForPath is a small display classifier keyed on the file
// extension. It lives here because both the fs tools (share results) and
// the server (attachment manifests) need it and neither should import the
// other; actual DOWNLOADS sniff the content and enforce the §9.4 inline
// allowlist, so a miss here is cosmetic.
func MediaTypeForPath(rel string) string {
	switch strings.ToLower(path.Ext(rel)) {
	case ".txt", ".log", ".md", ".markdown":
		return "text/plain"
	case ".csv":
		return "text/csv"
	case ".tsv":
		return "text/tab-separated-values"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}
