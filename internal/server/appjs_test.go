package server

import (
	"strings"
	"testing"

	"mullsmith/web"
)

// Keep this file deliberately limited to the static shell contract. Client
// workflows belong in web/test lifecycle or browser tests, where equivalent
// source text cannot pass without producing the required behavior.
func TestClientShellContainsRequiredSurfaces(t *testing.T) {
	html, err := web.FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	shell := string(html)
	for _, id := range []string{
		"model-picker", "thinking-select", "form-forgot", "form-reset",
		"form-confirm", "btn-delete-account", "files-purge-select",
		"btn-resend-confirm", "btn-cancel-email", "settings-email-pending",
	} {
		if !strings.Contains(shell, `id="`+id+`"`) {
			t.Errorf("index.html lost required element #%s", id)
		}
	}
}

func TestClientAPIExposesAccountAndEmailRoutes(t *testing.T) {
	b, err := web.FS.ReadFile("assets/app/api.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, route := range []string{
		"/api/v1/client-config",
		"/api/v1/auth/forgot-password", "/api/v1/auth/reset-password",
		"/api/v1/auth/confirm-email", "/api/v1/auth/resend-confirm",
		"/api/v1/auth/me/cancel-email", "/api/v1/auth/me/delete",
	} {
		if !strings.Contains(src, `"`+route+`"`) {
			t.Errorf("api.js lost route %s", route)
		}
	}
}
