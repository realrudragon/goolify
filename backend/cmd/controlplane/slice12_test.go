package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/worker"
)

func TestProductionSlice12_GitCloudDNSServerSubsystems(t *testing.T) {
	app, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()

	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// Register a second team user for cross-team checks
	reg := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Other12", "email": "slice12-other@coolify.test", "password": "OtherSecret123!",
	})
	reg.Body.Close()
	otherCookies, otherCSRF := loginClient(t, ts.URL, "slice12-other@coolify.test", "OtherSecret123!")

	// Local upstream mock provider HTTP server (real HTTP client round-trips!)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/installation/repositories":
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"acme/web","default_branch":"main"}]}`))
		case "/repos/acme/web/branches":
			_, _ = w.Write([]byte(`[{"name":"main"},{"name":"staging"}]`))
		case "/zones":
			_, _ = w.Write([]byte(`{"result":[{"id":"cf-zone-1","name":"example.com","account_id":"acc-1","account_name":"Acme"}]}`))
		case "/secrets":
			_, _ = w.Write([]byte(`{"keys":["DB_PASS","API_KEY"],"secrets":{"DB_PASS":"super-secret-db","API_KEY":"key-999"}}`))
		case "/locations":
			_, _ = w.Write([]byte(`{"locations":[{"id":1,"name":"fsn1","country":"DE"}]}`))
		case "/servers":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"server":{"public_net":{"ipv4":{"ip":"203.0.113.55"}}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer upstream.Close()
	t.Setenv("COOLIFY_HETZNER_API_URL", upstream.URL)

	// 1. GitHub Apps CRUD + sentinel id=0 delete protection + real HTTP repos/branches
	delSentinel := doJSON(t, http.MethodDelete, ts.URL+"/api/v1/github-apps/0", rootCookies, rootCSRF, "", nil)
	delSentinel.Body.Close()
	if delSentinel.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 deleting sentinel github_apps id=0, got %d", delSentinel.StatusCode)
	}
	ghCreate := doJSON(t, http.MethodPost, ts.URL+"/api/v1/github-apps", rootCookies, rootCSRF, "", map[string]any{
		"name": "Acme GH App", "api_url": upstream.URL, "html_url": "https://github.com",
		"client_secret": "gh-client-secret-xyz", "webhook_secret": "gh-wh-secret-xyz",
	})
	var ghOut struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(ghCreate.Body).Decode(&ghOut)
	ghCreate.Body.Close()
	if ghCreate.StatusCode != http.StatusCreated || ghOut.UUID == "" {
		t.Fatalf("POST /github-apps failed: %d", ghCreate.StatusCode)
	}
	var encGH string
	_ = pool.QueryRow(ctx, `SELECT client_secret FROM github_apps WHERE uuid = $1`, ghOut.UUID).Scan(&encGH)
	if strings.Contains(encGH, "gh-client-secret-xyz") {
		t.Fatal("github_apps.client_secret stored in plaintext!")
	}
	reposResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/github-apps/"+ghOut.UUID+"/repositories", rootCookies, "", "", nil)
	reposBody, _ := io.ReadAll(reposResp.Body)
	reposResp.Body.Close()
	if reposResp.StatusCode != http.StatusOK || !strings.Contains(string(reposBody), "acme/web") {
		t.Fatalf("GET /github-apps/repositories failed: %d %s", reposResp.StatusCode, string(reposBody))
	}
	brResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/github-apps/"+ghOut.UUID+"/repositories/acme/web/branches", rootCookies, "", "", nil)
	brBody, _ := io.ReadAll(brResp.Body)
	brResp.Body.Close()
	if brResp.StatusCode != http.StatusOK || !strings.Contains(string(brBody), "staging") {
		t.Fatalf("GET /github-apps/branches failed: %d %s", brResp.StatusCode, string(brBody))
	}

	// GitLab & Gitea apps
	glResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/gitlab-apps", rootCookies, rootCSRF, "", map[string]any{
		"name": "Acme GitLab", "app_secret": "gl-secret-123",
	})
	glResp.Body.Close()
	if glResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /gitlab-apps failed: %d", glResp.StatusCode)
	}
	gtResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/gitea-apps", rootCookies, rootCSRF, "", map[string]any{
		"name": "Acme Gitea", "api_url": "https://gitea.local/api/v1", "access_token": "gt-token-123",
	})
	gtResp.Body.Close()
	if gtResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /gitea-apps failed: %d", gtResp.StatusCode)
	}

	// 2. Integration Tokens + DNS Zone Sync + Managed DNS + Secret Manager on Application
	tokResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/integration-tokens", rootCookies, rootCSRF, "", map[string]any{
		"provider": "cloudflare", "name": "CF DNS + Secrets", "token": "cf-live-tok-999",
		"capabilities": []string{"dns", "secrets"},
		"metadata":     map[string]any{"api_base_url": upstream.URL},
	})
	var tokOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(tokResp.Body).Decode(&tokOut)
	tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusCreated || tokOut.UUID == "" {
		t.Fatalf("POST /security/integration-tokens failed: %d", tokResp.StatusCode)
	}
	var encIntTok string
	_ = pool.QueryRow(ctx, `SELECT token FROM integration_tokens WHERE uuid = $1`, tokOut.UUID).Scan(&encIntTok)
	if strings.Contains(encIntTok, "cf-live-tok-999") {
		t.Fatal("integration_tokens.token stored in plaintext!")
	}
	syncResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/integration-tokens/"+tokOut.UUID+"/sync-zones", rootCookies, rootCSRF, "", nil)
	var syncOut struct {
		Zones []struct{ UUID string `json:"uuid"` } `json:"zones"`
	}
	_ = json.NewDecoder(syncResp.Body).Decode(&syncOut)
	syncResp.Body.Close()
	if syncResp.StatusCode != http.StatusOK || len(syncOut.Zones) != 1 {
		t.Fatalf("POST /sync-zones failed: %d zones=%v", syncResp.StatusCode, syncOut.Zones)
	}

	// Seed project + app for DNS, Secret Manager, and Vulnerabilities
	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Slice12 Proj"})
	var pOut struct {
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()
	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
		"name": "s12-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": "localhost",
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "1.27",
	})
	var appOut struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	appResp.Body.Close()

	// Managed DNS create + list + release
	dnsPost := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/dns", rootCookies, rootCSRF, "", map[string]any{
		"hostname": "app.example.com", "zone_uuid": syncOut.Zones[0].UUID, "type": "A", "content": "203.0.113.1",
	})
	dnsPost.Body.Close()
	if dnsPost.StatusCode != http.StatusCreated {
		t.Fatalf("POST /applications/dns failed: %d", dnsPost.StatusCode)
	}
	dnsDel := doJSON(t, http.MethodDelete, ts.URL+"/api/v1/applications/"+appOut.UUID+"/dns/app.example.com", rootCookies, rootCSRF, "", nil)
	dnsDel.Body.Close()
	if dnsDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /applications/dns failed: %d", dnsDel.StatusCode)
	}

	// Secret Manager link + keys + import
	smLink := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/"+appOut.UUID+"/secret-manager", rootCookies, rootCSRF, "", map[string]any{
		"integration_token_uuid": tokOut.UUID, "settings": map[string]any{"project": "prod"},
	})
	smLink.Body.Close()
	if smLink.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /secret-manager failed: %d", smLink.StatusCode)
	}
	smKeys := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appOut.UUID+"/secret-manager/keys", rootCookies, "", "", nil)
	smKeysBody, _ := io.ReadAll(smKeys.Body)
	smKeys.Body.Close()
	if smKeys.StatusCode != http.StatusOK || !strings.Contains(string(smKeysBody), "DB_PASS") {
		t.Fatalf("GET /secret-manager/keys failed: %d %s", smKeys.StatusCode, string(smKeysBody))
	}
	smImp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/secret-manager/import", rootCookies, rootCSRF, "", nil)
	smImp.Body.Close()
	if smImp.StatusCode != http.StatusOK {
		t.Fatalf("POST /secret-manager/import failed: %d", smImp.StatusCode)
	}

	// 3. Cloud Tokens, Cloud-Init Scripts, Hetzner Catalog & Server Provisioning
	ctResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/cloud-tokens", rootCookies, rootCSRF, "", map[string]any{
		"provider": "hetzner", "name": "Hetzner Prod", "token": "hcloud-secret-tok",
	})
	var ctOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(ctResp.Body).Decode(&ctOut)
	ctResp.Body.Close()
	if ctResp.StatusCode != http.StatusCreated || ctOut.UUID == "" {
		t.Fatalf("POST /cloud-tokens failed: %d", ctResp.StatusCode)
	}
	ciResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/cloud-init-scripts", rootCookies, rootCSRF, "", map[string]any{
		"name": "harden-ubuntu", "script": "#cloud-config\npackage_update: true\n",
	})
	ciResp.Body.Close()
	if ciResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /cloud-init-scripts failed: %d", ciResp.StatusCode)
	}
	hetLoc := doJSON(t, http.MethodGet, ts.URL+"/api/v1/hetzner/locations?cloud_provider_token_uuid="+ctOut.UUID, rootCookies, "", "", nil)
	hetLocBody, _ := io.ReadAll(hetLoc.Body)
	hetLoc.Body.Close()
	if hetLoc.StatusCode != http.StatusOK || !strings.Contains(string(hetLocBody), "fsn1") {
		t.Fatalf("GET /hetzner/locations failed: %d %s", hetLoc.StatusCode, string(hetLocBody))
	}
	hetSrv := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/hetzner", rootCookies, rootCSRF, "", map[string]any{
		"cloud_provider_token_uuid": ctOut.UUID, "name": "hetzner-edge-1", "location": "fsn1", "server_type": "cpx11",
	})
	var hetSrvOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(hetSrv.Body).Decode(&hetSrvOut)
	hetSrv.Body.Close()
	if hetSrv.StatusCode != http.StatusCreated || hetSrvOut.UUID == "" {
		t.Fatalf("POST /servers/hetzner failed: %d", hetSrv.StatusCode)
	}

	// Cross-team user MUST be denied Hetzner token use
	hetDenied := doJSON(t, http.MethodGet, ts.URL+"/api/v1/hetzner/locations?cloud_provider_token_uuid="+ctOut.UUID, otherCookies, "", "", nil)
	hetDenied.Body.Close()
	if hetDenied.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cross-team hetzner token use, got %d", hetDenied.StatusCode)
	}

	// 4. Server Subsystems: Docker Cleanup + Worker, Log Drains, Sentinel + Push, Registries, Tunnel, Proxy, Transfer
	dcPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/docker-cleanup", rootCookies, rootCSRF, "", map[string]any{
		"docker_cleanup_threshold": 75, "delete_unused_volumes": true,
	})
	dcPatch.Body.Close()
	if dcPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /docker-cleanup failed: %d", dcPatch.StatusCode)
	}
	dcRun := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/docker-cleanup/run", rootCookies, rootCSRF, "", nil)
	var dcRunOut struct{ ExecutionUUID string `json:"execution_uuid"` }
	_ = json.NewDecoder(dcRun.Body).Decode(&dcRunOut)
	dcRun.Body.Close()
	if dcRun.StatusCode != http.StatusAccepted || dcRunOut.ExecutionUUID == "" {
		t.Fatalf("POST /docker-cleanup/run failed: %d", dcRun.StatusCode)
	}
	wr := worker.NewRunner(worker.Config{
		WorkerID:   "worker-slice12",
		Pool:       pool,
		Cipher:     ciph,
		JobCoord:   app.JobCoord,
		Probe:      executor.NewPostgresRemoteProbe(pool, false),
		Executor:   executor.NewClient(&sliceTestTransport{}),
		LogStore:   app.LogStore,
		Outbox:     app.Outbox,
		Dispatcher: app.Dispatcher,
		LeaseTTL:   5 * time.Second,
	})
	_, _ = wr.ProcessOneJob(ctx)
	var dcSt string
	_ = pool.QueryRow(ctx, `SELECT status FROM docker_cleanup_executions WHERE uuid = $1`, dcRunOut.ExecutionUUID).Scan(&dcSt)
	if dcSt != "success" {
		t.Fatalf("expected docker_cleanup execution status=success, got %q", dcSt)
	}

	// Log drains secret encryption
	ldPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/log-drains", rootCookies, rootCSRF, "", map[string]any{
		"is_logdrain_axiom_enabled": true, "logdrain_axiom_dataset_name": "coolify", "logdrain_axiom_api_key": "xaat-secret-key",
	})
	ldPatch.Body.Close()
	if ldPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /log-drains failed: %d", ldPatch.StatusCode)
	}
	var encAxiom string
	_ = pool.QueryRow(ctx, `SELECT ss.logdrain_axiom_api_key FROM server_settings ss JOIN servers s ON s.id = ss.server_id WHERE s.uuid = $1`, hetSrvOut.UUID).Scan(&encAxiom)
	if strings.Contains(encAxiom, "xaat-secret-key") {
		t.Fatal("logdrain_axiom_api_key stored in plaintext!")
	}

	// Sentinel config + push verification
	sentPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/sentinel", rootCookies, rootCSRF, "", map[string]any{
		"is_sentinel_enabled": true, "sentinel_token": "sent-secret-777",
	})
	sentPatch.Body.Close()
	if sentPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /sentinel failed: %d", sentPatch.StatusCode)
	}
	badPush := doJSON(t, http.MethodPost, ts.URL+"/api/v1/sentinel/push", nil, "", "wrong-token", map[string]any{
		"server_uuid": hetSrvOut.UUID, "cpu_percent": 12.5,
	})
	badPush.Body.Close()
	if badPush.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 bad sentinel push, got %d", badPush.StatusCode)
	}
	okPush := doJSON(t, http.MethodPost, ts.URL+"/api/v1/sentinel/push", nil, "", "sent-secret-777", map[string]any{
		"server_uuid": hetSrvOut.UUID, "cpu_percent": 42.5, "memory_percent": 61.0, "disk_percent": 33.2,
	})
	okPush.Body.Close()
	if okPush.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 valid sentinel push, got %d", okPush.StatusCode)
	}

	// Registries, Cloudflare Tunnel, Image Scanning, Proxy Config
	regAdd := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/registries", rootCookies, rootCSRF, "", map[string]any{
		"registry": "ghcr.io", "username": "bot", "password": "ghp_secret_password",
	})
	regAdd.Body.Close()
	if regAdd.StatusCode != http.StatusCreated {
		t.Fatalf("POST /registries failed: %d", regAdd.StatusCode)
	}
	cfEn := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/cloudflare-tunnel/enable", rootCookies, rootCSRF, "", nil)
	cfEn.Body.Close()
	if cfEn.StatusCode != http.StatusOK {
		t.Fatalf("POST /cloudflare-tunnel/enable failed: %d", cfEn.StatusCode)
	}
	prxCfg := doJSON(t, http.MethodPut, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/proxy/configuration", rootCookies, rootCSRF, "", map[string]string{
		"configuration": "services:\n  traefik:\n    image: traefik:v3.1\n",
	})
	prxCfg.Body.Close()
	if prxCfg.StatusCode != http.StatusOK {
		t.Fatalf("PUT /proxy/configuration failed: %d", prxCfg.StatusCode)
	}

	// Transfer complete MUST block subsequent remote commands (AGENTS.md transferred-away rule!)
	trComp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/transfer/complete", rootCookies, rootCSRF, "", map[string]string{
		"target_url": "https://new-coolify.example.com",
	})
	trComp.Body.Close()
	if trComp.StatusCode != http.StatusOK {
		t.Fatalf("POST /transfer/complete failed: %d", trComp.StatusCode)
	}
	blockedCleanup := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/"+hetSrvOut.UUID+"/docker-cleanup/run", rootCookies, rootCSRF, "", nil)
	blockedCleanup.Body.Close()
	if blockedCleanup.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 running remote command on transferred-away server, got %d", blockedCleanup.StatusCode)
	}

	// 5. Vulnerabilities (Fleet & Per-Application, cross-team isolated)
	_, err := pool.Exec(ctx, `
		INSERT INTO image_scans (uuid, server_id, image_id, resource_type, resource_id, image_ref, status, summary, findings, scanned_at)
		VALUES ('scan-1', 0, 'sha256:abc', 'App\Models\Application', $1, 'nginx:1.27', 'completed', '{"high":1}'::jsonb, '[{"id":"CVE-2026-0001"}]'::jsonb, NOW())
	`, appOut.ID)
	if err != nil {
		t.Fatal(err)
	}
	vFleet := doJSON(t, http.MethodGet, ts.URL+"/api/v1/vulnerabilities", rootCookies, "", "", nil)
	vBody, _ := io.ReadAll(vFleet.Body)
	vFleet.Body.Close()
	if vFleet.StatusCode != http.StatusOK || !strings.Contains(string(vBody), "scan-1") {
		t.Fatalf("GET /vulnerabilities failed: %d %s", vFleet.StatusCode, string(vBody))
	}
	vOther := doJSON(t, http.MethodGet, ts.URL+"/api/v1/vulnerabilities", otherCookies, "", "", nil)
	vOtherBody, _ := io.ReadAll(vOther.Body)
	vOther.Body.Close()
	if strings.Contains(string(vOtherBody), "scan-1") {
		t.Fatalf("CROSS-TEAM LEAK: other team saw root team scan-1: %s", string(vOtherBody))
	}
	vApp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appOut.UUID+"/vulnerabilities", rootCookies, "", "", nil)
	vAppBody, _ := io.ReadAll(vApp.Body)
	vApp.Body.Close()
	if vApp.StatusCode != http.StatusOK || !strings.Contains(string(vAppBody), "CVE-2026-0001") {
		t.Fatalf("GET /applications/vulnerabilities failed: %d %s", vApp.StatusCode, string(vAppBody))
	}
	_ = otherCSRF
}
