package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// readJSON validates the complete, body-limited request before handlers act on it.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
		writeAPIError(w, r, http.StatusUnsupportedMediaType,
			"unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	// Reading to EOF enforces the middleware's byte cap even for ignored suffixes.
	body, err := io.ReadAll(r.Body)
	if IsBodyTooLarge(err) {
		writeAPIError(w, r, http.StatusRequestEntityTooLarge,
			"request_too_large", "request exceeds the body size limit")
		return false
	}
	body = bytes.Trim(body, " \t\r\n")
	if err == nil && len(body) > 0 && body[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if dec.Decode(dst) == nil && dec.Decode(new(any)) == io.EOF {
			return true
		}
	}
	writeAPIError(w, r, http.StatusBadRequest, "invalid_request",
		"request body must be a single JSON object with only known fields")
	return false
}
