package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/coollabsio/coolify/backend/internal/web"
)

func TestServeUIShell_AllowlistAndStrict404(t *testing.T) {
	staticFS := fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><html><body>Coolify Home</body></html>")},
		"app/index.html":    {Data: []byte("<!doctype html><html><body>Coolify Shell</body></html>")},
		"login/index.html":  {Data: []byte("<!doctype html><html><body>Sign in to Coolify</body></html>")},
		"_astro/app.123.js": {Data: []byte("console.log('ok');")},
	}
	handler := web.ServeUIShell(web.DefaultUIAllowlist(), staticFS)

	// /login MUST serve login/index.html, NEVER the dashboard shell
	reqLogin := httptest.NewRequest(http.MethodGet, "/login", nil)
	recLogin := httptest.NewRecorder()
	handler.ServeHTTP(recLogin, reqLogin)
	if recLogin.Code != http.StatusOK || !strings.Contains(recLogin.Body.String(), "Sign in to Coolify") || strings.Contains(recLogin.Body.String(), "Coolify Shell") {
		t.Fatalf("expected /login to serve login/index.html, got %d body=%q", recLogin.Code, recLogin.Body.String())
	}

	// Allowlisted runtime UUID UI routes & baseline UI paths serve HTML shell
	for _, uiPath := range []string{
		"/server/cabc123def456/settings",
		"/terminal",
		"/vulnerabilities",
		"/analytics",
		"/metrics",
		"/canvas",
		"/dashboard/canvas",
		"/sources",
		"/source/github/gh123/permissions",
		"/shared-variables/team",
		"/security/cloud-tokens",
		"/settings/oauth/github",
		"/project/p1/environment/e1/application/a1/rollback",
	} {
		req := httptest.NewRequest(http.MethodGet, uiPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "Coolify Shell") {
			t.Fatalf("expected 200 text/html shell for UI route %s, got %d %s", uiPath, rec.Code, rec.Header().Get("Content-Type"))
		}
	}

	// API, /app/*, /canvas/api/*, /mcp, /webhooks/*, /realtime/*, /terminal/* 404 never fall back to HTML shell
	for _, path := range []string{"/api/v1/nonexistent", "/app/unknown", "/canvas/api/unknown", "/mcp", "/webhooks/unknown", "/realtime/unknown", "/terminal/unknown"} {
		reqAPI := httptest.NewRequest(http.MethodGet, path, nil)
		recAPI := httptest.NewRecorder()
		handler.ServeHTTP(recAPI, reqAPI)
		if recAPI.Code != http.StatusNotFound || !strings.Contains(recAPI.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("expected 404 application/json for %s, got %d %s", path, recAPI.Code, recAPI.Header().Get("Content-Type"))
		}
	}

	// Missing static asset never falls back to HTML shell
	reqAsset := httptest.NewRequest(http.MethodGet, "/_astro/missing.js", nil)
	recAsset := httptest.NewRecorder()
	handler.ServeHTTP(recAsset, reqAsset)
	if recAsset.Code != http.StatusNotFound || strings.Contains(recAsset.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected 404 non-HTML for missing asset, got %d %s", recAsset.Code, recAsset.Header().Get("Content-Type"))
	}
}
