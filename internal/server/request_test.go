package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSONValidatesWholeBody(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		limit                   int64
		status                  int
	}{
		{"object", `{"name":"ok"}`, "application/json", 64, 204},
		{"exact limit", `{"name":"ok"}`, "application/json", 13, 204},
		{"whitespace", " \n{\"name\":\"ok\"}\t ", "application/json; charset=utf-8", 64, 204},
		{"second object", `{"name":"ok"} {"name":"other"}`, "application/json", 64, 400},
		{"second null", `{"name":"ok"} null`, "application/json", 64, 400},
		{"garbage", `{"name":"ok"} garbage`, "application/json", 64, 400},
		{"null", "null", "application/json", 64, 400},
		{"array", "[]", "application/json", 64, 400},
		{"string", `"text"`, "application/json", 64, 400},
		{"empty", "", "application/json", 64, 400},
		{"unknown field", `{"unexpected":true}`, "application/json", 64, 400},
		{"incomplete", `{"name":`, "application/json", 64, 400},
		{"non JSON whitespace", "\v{\"name\":\"ok\"}", "application/json", 64, 400},
		{"oversized object", `{"name":"` + strings.Repeat("x", 256) + `"}`, "application/json", 64, 413},
		{"oversized suffix", `{"name":"ok"}` + strings.Repeat(" ", 256), "application/json", 64, 413},
		{"wrong media type", `{"name":"ok"}`, "text/plain", 64, 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Chunked requests have no usable Content-Length: the read itself must enforce the cap.
			for _, unknownLength := range []bool{false, true} {
				acted := false
				h := bodyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Name string `json:"name"`
					}
					if !readJSON(w, r, &body) {
						return
					}
					acted = true
					w.WriteHeader(http.StatusNoContent)
				}), tc.limit)
				r := httptest.NewRequest("POST", "/api/v1/probe", strings.NewReader(tc.body))
				if unknownLength {
					r.ContentLength = -1
				}
				r.Header.Set("Content-Type", tc.contentType)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.status || acted != (tc.status == 204) {
					t.Fatalf("unknown length=%v: status=%d acted=%v body=%s", unknownLength, w.Code, acted, w.Body.String())
				}
				if tc.status >= 400 && !json.Valid(w.Body.Bytes()) {
					t.Fatalf("invalid error response: %s", w.Body.String())
				}
				if tc.status == 413 && !strings.Contains(w.Body.String(), `"request_too_large"`) {
					t.Fatalf("wrong body-limit error: %s", w.Body.String())
				}
			}
		})
	}
}
