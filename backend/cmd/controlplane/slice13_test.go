package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionSlice13_WebCanvasOAuthWebhooksAndUIRoutes(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()

	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 1. Create a project via /canvas/api/projects (session-authenticated canvas mirror!)
	cProj := doJSON(t, http.MethodPost, ts.URL+"/canvas/api/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Canvas Proj"})
	var cProjOut struct {
		Project    struct{ UUID string `json:"uuid"` } `json:"project"`
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(cProj.Body).Decode(&cProjOut)
	cProj.Body.Close()
	if cProj.StatusCode != http.StatusCreated || cProjOut.Project.UUID == "" {
		t.Fatalf("POST /canvas/api/projects failed: %d", cProj.StatusCode)
	}

	// Canvas positions save & read back
	posPatch := doJSON(t, http.MethodPatch, ts.URL+"/canvas/api/projects/canvas/positions", rootCookies, rootCSRF, "", map[string]any{
		"positions": []map[string]any{{"node_type": "project", "node_uuid": cProjOut.Project.UUID, "x": 120.5, "y": 240.0}},
	})
	posPatch.Body.Close()
	if posPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /canvas/api/projects/canvas/positions failed: %d", posPatch.StatusCode)
	}
	cGraph := doJSON(t, http.MethodGet, ts.URL+"/canvas/api/projects/canvas", rootCookies, "", "", nil)
	cGraphBody, _ := io.ReadAll(cGraph.Body)
	cGraph.Body.Close()
	if cGraph.StatusCode != http.StatusOK || !strings.Contains(string(cGraphBody), cProjOut.Project.UUID) || !strings.Contains(string(cGraphBody), "120.5") {
		t.Fatalf("GET /canvas/api/projects/canvas failed: %d %s", cGraph.StatusCode, string(cGraphBody))
	}

	// Environment canvas links CRUD
	envLinkPost := doJSON(t, http.MethodPost, ts.URL+"/canvas/api/projects/"+cProjOut.Project.UUID+"/environments/"+cProjOut.DefaultEnv.UUID+"/canvas/links", rootCookies, rootCSRF, "", map[string]any{
		"source_type": "application", "source_id": 10, "target_type": "database", "target_id": 20, "label": "DATABASE_URL",
	})
	var linkOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(envLinkPost.Body).Decode(&linkOut)
	envLinkPost.Body.Close()
	if envLinkPost.StatusCode != http.StatusCreated || linkOut.UUID == "" {
		t.Fatalf("POST env canvas link failed: %d", envLinkPost.StatusCode)
	}
	envCanvas := doJSON(t, http.MethodGet, ts.URL+"/canvas/api/projects/"+cProjOut.Project.UUID+"/environments/"+cProjOut.DefaultEnv.UUID+"/canvas", rootCookies, "", "", nil)
	envCanvasBody, _ := io.ReadAll(envCanvas.Body)
	envCanvas.Body.Close()
	if envCanvas.StatusCode != http.StatusOK || !strings.Contains(string(envCanvasBody), "DATABASE_URL") {
		t.Fatalf("GET env canvas failed: %d %s", envCanvas.StatusCode, string(envCanvasBody))
	}
	envLinkDel := doJSON(t, http.MethodDelete, ts.URL+"/canvas/api/projects/"+cProjOut.Project.UUID+"/environments/"+cProjOut.DefaultEnv.UUID+"/canvas/links/"+linkOut.UUID, rootCookies, rootCSRF, "", nil)
	envLinkDel.Body.Close()
	if envLinkDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE env canvas link failed: %d", envLinkDel.StatusCode)
	}

	// 2. Forgot password, Email Verify, Magic Link
	fp := doJSON(t, http.MethodPost, ts.URL+"/forgot-password", nil, "", "", map[string]string{"email": "root@coolify.test"})
	fp.Body.Close()
	if fp.StatusCode != http.StatusOK {
		t.Fatalf("POST /forgot-password failed: %d", fp.StatusCode)
	}
	var resetCnt int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM go_one_time_tokens WHERE purpose = 'password_reset' AND user_id = 0`).Scan(&resetCnt)
	if resetCnt != 1 {
		t.Fatalf("expected password_reset token stored, got %d", resetCnt)
	}

	sum := sha1.Sum([]byte("root@coolify.test"))
	verResp := doJSON(t, http.MethodGet, ts.URL+"/email/verify/0/"+hex.EncodeToString(sum[:]), rootCookies, "", "", nil)
	verResp.Body.Close()
	if verResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /email/verify failed: %d", verResp.StatusCode)
	}

	_, _ = pool.Exec(ctx, `INSERT INTO go_one_time_tokens (token, purpose, user_id, team_id, expires_at) VALUES ('magic-tok-1', 'magic_link', 0, 0, NOW() + INTERVAL '10 minutes')`)
	mlGet := doJSON(t, http.MethodGet, ts.URL+"/auth/link?token=magic-tok-1", nil, "", "", nil)
	mlGet.Body.Close()
	if mlGet.StatusCode != http.StatusOK {
		t.Fatalf("GET /auth/link failed: %d", mlGet.StatusCode)
	}
	mlPost := doJSON(t, http.MethodPost, ts.URL+"/auth/link", nil, "", "", map[string]string{"token": "magic-tok-1"})
	mlPost.Body.Close()
	if mlPost.StatusCode != http.StatusOK {
		t.Fatalf("POST /auth/link failed: %d", mlPost.StatusCode)
	}

	// 3. OAuth Settings + Redirect + Callback against real local httptest OIDC provider
	oauthProv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":"oidc-user-42","email":"oauth-user@coolify.test","name":"OAuth User"}`))
	}))
	defer oauthProv.Close()

	cfgOAuth := doJSON(t, http.MethodPatch, ts.URL+"/app/settings/oauth/github", rootCookies, rootCSRF, "", map[string]any{
		"enabled": true, "client_id": "cid-1", "client_secret": "csec-1", "redirect_uri": ts.URL + "/auth/github/callback", "tenant": oauthProv.URL, "allow_registration": true,
	})
	cfgOAuth.Body.Close()
	if cfgOAuth.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /app/settings/oauth/github failed: %d", cfgOAuth.StatusCode)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirResp, err := noFollow.Get(ts.URL + "/auth/github/redirect")
	if err != nil {
		t.Fatal(err)
	}
	redirResp.Body.Close()
	if redirResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 from /auth/github/redirect, got %d", redirResp.StatusCode)
	}
	locURL, _ := url.Parse(redirResp.Header.Get("Location"))
	state := locURL.Query().Get("state")
	cbResp, err := http.Get(ts.URL + "/auth/github/callback?code=authcode123&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	cbBody, _ := io.ReadAll(cbResp.Body)
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK || !strings.Contains(string(cbBody), `"authenticated":true`) {
		t.Fatalf("GET /auth/github/callback failed: %d %s", cbResp.StatusCode, string(cbBody))
	}

	// 4. Terminal HTTP Auth/IPs/Session + File Browser Download + SVG Avatars + Icons
	tAuth := doJSON(t, http.MethodPost, ts.URL+"/terminal/auth", rootCookies, rootCSRF, "", nil)
	tAuth.Body.Close()
	if tAuth.StatusCode != http.StatusOK {
		t.Fatalf("POST /terminal/auth failed: %d", tAuth.StatusCode)
	}
	tIPs := doJSON(t, http.MethodPost, ts.URL+"/terminal/auth/ips", rootCookies, rootCSRF, "", nil)
	tIPs.Body.Close()
	if tIPs.StatusCode != http.StatusOK {
		t.Fatalf("POST /terminal/auth/ips failed: %d", tIPs.StatusCode)
	}

	dlTmp := filepath.Join(t.TempDir(), "sample.txt")
	_ = os.WriteFile(dlTmp, []byte("HELLO_FILE_BROWSER"), 0o644)
	plBytes, _ := json.Marshal(map[string]string{"path": dlTmp})
	_, _ = pool.Exec(ctx, `INSERT INTO go_one_time_tokens (token, purpose, user_id, team_id, payload, expires_at) VALUES ('fbtok-1', 'file_download', 0, 0, $1::jsonb, NOW() + INTERVAL '5 minutes')`, string(plBytes))
	fbDL := doJSON(t, http.MethodGet, ts.URL+"/files/download/fbtok-1", rootCookies, "", "", nil)
	fbBytes, _ := io.ReadAll(fbDL.Body)
	fbDL.Body.Close()
	if fbDL.StatusCode != http.StatusOK || string(fbBytes) != "HELLO_FILE_BROWSER" {
		t.Fatalf("GET /files/download failed: %d %q", fbDL.StatusCode, string(fbBytes))
	}
	// Second download of single-use token MUST return 404
	fbDL2 := doJSON(t, http.MethodGet, ts.URL+"/files/download/fbtok-1", rootCookies, "", "", nil)
	fbDL2.Body.Close()
	if fbDL2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on single-use file download token replay, got %d", fbDL2.StatusCode)
	}

	// Avatars & Project Icon & Legacy Redirect
	for _, u := range []string{"/profile/avatar", "/team/0/avatar", "/project/" + cProjOut.Project.UUID + "/icon"} {
		resp := doJSON(t, http.MethodGet, ts.URL+u, rootCookies, "", "", nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<svg") {
			t.Fatalf("expected 200 SVG for %s, got %d %s", u, resp.StatusCode, string(body))
		}
	}

	// Stripe webhook (requires Stripe-Signature) & GitHub source redirect
	noSig := doJSON(t, http.MethodPost, ts.URL+"/webhooks/payments/stripe/events", nil, "", "", map[string]string{"id": "evt_1", "type": "invoice.paid"})
	noSig.Body.Close()
	if noSig.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 stripe webhook without signature, got %d", noSig.StatusCode)
	}
	reqStripe, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/payments/stripe/events", strings.NewReader(`{"id":"evt_1","type":"invoice.paid"}`))
	reqStripe.Header.Set("Content-Type", "application/json")
	reqStripe.Header.Set("Stripe-Signature", "t=123,v1=abc")
	stResp, err := http.DefaultClient.Do(reqStripe)
	if err != nil {
		t.Fatal(err)
	}
	stResp.Body.Close()
	if stResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /webhooks/payments/stripe/events failed: %d", stResp.StatusCode)
	}

	ghRedir := doJSON(t, http.MethodGet, ts.URL+"/webhooks/source/github/redirect", rootCookies, "", "", nil)
	ghRedir.Body.Close()
	if ghRedir.StatusCode != http.StatusOK {
		t.Fatalf("GET /webhooks/source/github/redirect failed: %d", ghRedir.StatusCode)
	}
}
