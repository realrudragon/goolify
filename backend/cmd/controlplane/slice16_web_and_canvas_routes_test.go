package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/web"
)

// TestProductionSlice16_CanvasAPIWebEndpointsAndFullUIAllowlistParity exercises every
// `/canvas/api/*`, `/terminal/*`, avatar/icon/download, OpenAPI, fleet metrics, and UI shell
// allowlist route from migration/routes.csv against real PostgreSQL + built Astro/Vue dist.
func TestProductionSlice16_CanvasAPIWebEndpointsAndFullUIAllowlistParity(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()

	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	assertStatus := func(label, method, path string, body any, want ...int) string {
		t.Helper()
		res := doJSON(t, method, ts.URL+path, rootCookies, rootCSRF, "", body)
		txt := readRespString(res)
		for _, w := range want {
			if res.StatusCode == w {
				return txt
			}
		}
		t.Fatalf("[%s] %s %s -> got %d, want %v; body=%s", label, method, path, res.StatusCode, want, txt)
		return ""
	}

	// 1. OpenAPI & Fleet Metrics
	oaRaw := assertStatus("openapi-json", http.MethodGet, "/api/v1/openapi.json", nil, http.StatusOK)
	if !strings.Contains(oaRaw, `"openapi": "3.0.3"`) {
		t.Fatalf("invalid openapi json: %s", oaRaw)
	}
	_, _ = pool.Exec(ctx, `INSERT INTO go_server_metrics (server_id, cpu_percent, memory_percent, disk_percent, containers) VALUES (0, 12.5, 44.0, 31.0, '[{"name":"coolify-proxy","status":"running"}]'::jsonb)`)
	fmRaw := assertStatus("fleet-metrics", http.MethodGet, "/api/v1/fleet/metrics", nil, http.StatusOK)
	if !strings.Contains(fmRaw, `"avg_cpu_percent":12.5`) {
		t.Fatalf("fleet metrics did not aggregate real DB row: %s", fmRaw)
	}

	// 2. Canvas API: Projects, Environments, Links, Tags, Shared Vars, Servers, Dashboard + Mirrored CRUD
	projRaw := assertStatus("canvas-proj-create", http.MethodPost, "/canvas/api/projects", map[string]any{"name": "CanvasProj"}, http.StatusCreated, http.StatusOK)
	var projResp struct {
		Project            struct{ UUID string `json:"uuid"` } `json:"project"`
		DefaultEnvironment struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.Unmarshal([]byte(projRaw), &projResp)
	pUUID := projResp.Project.UUID
	eUUID := projResp.DefaultEnvironment.UUID

	assertStatus("canvas-projects-list", http.MethodGet, "/canvas/api/projects", nil, http.StatusOK)
	assertStatus("canvas-projects-graph", http.MethodGet, "/canvas/api/projects/canvas", nil, http.StatusOK)
	assertStatus("canvas-projects-pos", http.MethodPatch, "/canvas/api/projects/canvas/positions", map[string]any{"positions": []map[string]any{{"node_type": "project", "node_uuid": pUUID, "x": 100, "y": 200}}}, http.StatusOK)
	assertStatus("canvas-project-get", http.MethodGet, "/canvas/api/projects/"+pUUID+"/canvas", nil, http.StatusOK)
	assertStatus("canvas-project-envs", http.MethodGet, "/canvas/api/projects/"+pUUID+"/environments", nil, http.StatusOK)
	assertStatus("canvas-project-env-create", http.MethodPost, "/canvas/api/projects/"+pUUID+"/environments", map[string]any{"name": "qa-canvas"}, http.StatusCreated, http.StatusOK)
	assertStatus("canvas-env-graph", http.MethodGet, "/canvas/api/projects/"+pUUID+"/environments/"+eUUID+"/canvas", nil, http.StatusOK)
	assertStatus("canvas-env-pos", http.MethodPatch, "/canvas/api/projects/"+pUUID+"/environments/"+eUUID+"/canvas/positions", map[string]any{"positions": []map[string]any{{"node_type": "app", "node_uuid": "a1", "x": 10, "y": 20}}}, http.StatusOK)

	linkRaw := assertStatus("canvas-env-link-create", http.MethodPost, "/canvas/api/projects/"+pUUID+"/environments/"+eUUID+"/canvas/links", map[string]any{
		"source_type": "application", "source_id": 1, "target_type": "database", "target_id": 2, "label": "reads",
	}, http.StatusCreated, http.StatusOK)
	var linkObj struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(linkRaw), &linkObj)
	if linkObj.UUID != "" {
		assertStatus("canvas-env-link-patch", http.MethodPatch, "/canvas/api/projects/"+pUUID+"/environments/"+eUUID+"/canvas/links/"+linkObj.UUID, map[string]any{"label": "writes"}, http.StatusOK)
		assertStatus("canvas-env-link-del", http.MethodDelete, "/canvas/api/projects/"+pUUID+"/environments/"+eUUID+"/canvas/links/"+linkObj.UUID, nil, http.StatusOK)
	}

	assertStatus("canvas-tags-graph", http.MethodGet, "/canvas/api/tags/canvas", nil, http.StatusOK)
	assertStatus("canvas-sv-team-graph", http.MethodGet, "/canvas/api/shared-variables/team/canvas", nil, http.StatusOK)
	assertStatus("canvas-sv-proj-graph", http.MethodGet, "/canvas/api/shared-variables/project/"+pUUID+"/canvas", nil, http.StatusOK)
	assertStatus("canvas-servers-list", http.MethodGet, "/canvas/api/servers", nil, http.StatusOK)
	assertStatus("canvas-servers-graph", http.MethodGet, "/canvas/api/servers/canvas", nil, http.StatusOK)
	assertStatus("canvas-servers-pos", http.MethodPatch, "/canvas/api/servers/canvas/positions", map[string]any{"positions": []map[string]any{{"node_type": "server", "node_uuid": "localhost", "x": 50, "y": 50}}}, http.StatusOK)
	assertStatus("canvas-dashboard-graph", http.MethodGet, "/canvas/api/dashboard/canvas", nil, http.StatusOK)
	assertStatus("canvas-dashboard-pos", http.MethodPatch, "/canvas/api/dashboard/canvas/positions", map[string]any{"positions": []map[string]any{{"node_type": "server", "node_uuid": "localhost", "x": 60, "y": 60}}}, http.StatusOK)
	assertStatus("canvas-server-single", http.MethodGet, "/canvas/api/servers/localhost/canvas", nil, http.StatusOK)
	assertStatus("canvas-server-containers", http.MethodGet, "/canvas/api/servers/localhost/canvas/containers", nil, http.StatusOK)
	assertStatus("canvas-server-usage", http.MethodGet, "/canvas/api/servers/localhost/canvas/usage", nil, http.StatusOK)
	assertStatus("canvas-server-dests", http.MethodGet, "/canvas/api/servers/localhost/destinations", nil, http.StatusOK)
	assertStatus("canvas-gh-apps", http.MethodGet, "/canvas/api/github-apps", nil, http.StatusOK)
	assertStatus("canvas-sec-keys", http.MethodGet, "/canvas/api/security/keys", nil, http.StatusOK)

	// Canvas mirrored application, service, and all 8 database engine creations
	appRaw := assertStatus("canvas-app-public", http.MethodPost, "/canvas/api/applications/public", map[string]any{
		"name": "c-app", "project_uuid": pUUID, "environment_uuid": eUUID, "server_uuid": "localhost", "git_repository": "https://github.com/coollabsio/coolify-examples", "git_branch": "main",
	}, http.StatusCreated, http.StatusOK)
	var cApp struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(appRaw), &cApp)
	assertStatus("canvas-app-get", http.MethodGet, "/canvas/api/applications/"+cApp.UUID, nil, http.StatusOK)
	assertStatus("canvas-app-patch", http.MethodPatch, "/canvas/api/applications/"+cApp.UUID, map[string]any{"description": "canvas-updated"}, http.StatusOK)
	assertStatus("canvas-app-envs-bulk", http.MethodPatch, "/canvas/api/applications/"+cApp.UUID+"/envs/bulk", map[string]any{"data": []map[string]any{{"key": "C_K", "value": "1"}}}, http.StatusOK, http.StatusCreated)

	svcRaw := assertStatus("canvas-svc-create", http.MethodPost, "/canvas/api/services", map[string]any{
		"name": "c-svc", "project_uuid": pUUID, "environment_uuid": eUUID, "server_uuid": "localhost", "docker_compose_raw": "services:\n  web:\n    image: nginx:alpine\n",
	}, http.StatusCreated, http.StatusOK)
	var cSvc struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(svcRaw), &cSvc)
	assertStatus("canvas-svc-envs-bulk", http.MethodPatch, "/canvas/api/services/"+cSvc.UUID+"/envs/bulk", map[string]any{"data": []map[string]any{{"key": "S_K", "value": "1"}}}, http.StatusOK, http.StatusCreated)

	for _, dbType := range []string{"postgresql", "mysql", "mariadb", "mongodb", "redis", "clickhouse", "dragonfly", "keydb"} {
		assertStatus("canvas-db-"+dbType, http.MethodPost, "/canvas/api/databases/"+dbType, map[string]any{
			"name": "c-" + dbType, "project_uuid": pUUID, "environment_uuid": eUUID, "server_uuid": "localhost",
		}, http.StatusCreated, http.StatusOK)
	}
	depRaw := assertStatus("canvas-deploy", http.MethodPost, "/canvas/api/deploy", map[string]any{"uuid": cApp.UUID}, http.StatusOK, http.StatusAccepted)
	var depResp struct {
		DeploymentUUID string `json:"deployment_uuid"`
		Deployments    []struct {
			DeploymentUUID string `json:"deployment_uuid"`
		} `json:"deployments"`
	}
	_ = json.Unmarshal([]byte(depRaw), &depResp)
	depID := depResp.DeploymentUUID
	if depID == "" && len(depResp.Deployments) > 0 {
		depID = depResp.Deployments[0].DeploymentUUID
	}
	if depID != "" {
		assertStatus("canvas-dep-get", http.MethodGet, "/canvas/api/deployments/"+depID, nil, http.StatusOK)
	}

	// 3. Web Auth, Realtime health, Terminal Session, Avatars, Icons, Backup Download
	assertStatus("web-realtime", http.MethodGet, "/realtime", nil, http.StatusOK)
	assertStatus("web-verify", http.MethodGet, "/verify", nil, http.StatusOK)
	assertStatus("web-2fa-switch", http.MethodPost, "/two-factor-required/switch-team", map[string]any{"team_id": 0}, http.StatusOK)
	assertStatus("web-profile-avatar", http.MethodGet, "/profile/avatar", nil, http.StatusOK)
	assertStatus("web-team-avatar", http.MethodGet, "/team/0/avatar", nil, http.StatusOK)
	assertStatus("web-member-avatar", http.MethodGet, "/team/members/0/avatar", nil, http.StatusOK)
	assertStatus("web-project-icon", http.MethodGet, "/project/"+pUUID+"/icon", nil, http.StatusOK)

	// Terminal auth -> issue ticket -> session redeem -> check -> end
	assertStatus("term-auth", http.MethodPost, "/terminal/auth", nil, http.StatusOK)
	assertStatus("term-ips", http.MethodGet, "/terminal/auth/ips", nil, http.StatusOK)
	tTicketRaw := assertStatus("term-ticket", http.MethodPost, "/app/terminal/ticket", map[string]any{"server_uuid": "localhost"}, http.StatusOK, http.StatusCreated)
	var tTicket struct {
		Token  string `json:"token"`
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal([]byte(tTicketRaw), &tTicket)
	tok := tTicket.Token
	if tok == "" {
		tok = tTicket.Ticket
	}
	tSessRaw := assertStatus("term-session", http.MethodPost, "/terminal/session", map[string]any{"token": tok}, http.StatusOK)
	var tSess struct{ SessionID string `json:"session_id"` }
	_ = json.Unmarshal([]byte(tSessRaw), &tSess)
	assertStatus("term-check", http.MethodPost, "/terminal/session/check", map[string]any{"session_id": tSess.SessionID}, http.StatusOK)
	assertStatus("term-end", http.MethodPost, "/terminal/session/end", map[string]any{"session_id": tSess.SessionID}, http.StatusNoContent, http.StatusOK)

	// Real backup file download (/download/backup/{executionId})
	tmpBkFile := filepath.Join(t.TempDir(), "test-bk.sql")
	_ = os.WriteFile(tmpBkFile, []byte("-- real sql dump\n"), 0o600)
	var bkSchedID, bkExecID int64
	_ = pool.QueryRow(ctx, `INSERT INTO scheduled_database_backups (uuid, frequency, timezone, enabled, database_id, database_type, team_id) VALUES ('sdb-dl-1', '0 0 * * *', 'UTC', TRUE, 0, 'postgresql', 0) RETURNING id`).Scan(&bkSchedID)
	_ = pool.QueryRow(ctx, `INSERT INTO scheduled_database_backup_executions (uuid, scheduled_database_backup_id, status, filename) VALUES ('bke-dl-1', $1, 'success', $2) RETURNING id`, bkSchedID, tmpBkFile).Scan(&bkExecID)
	dlRaw := assertStatus("web-download-backup", http.MethodGet, fmt.Sprintf("/download/backup/%d", bkExecID), nil, http.StatusOK)
	if !strings.Contains(dlRaw, "-- real sql dump") {
		t.Fatalf("unexpected backup download content: %q", dlRaw)
	}

	// 4. Full UI Shell Allowlist Verification against REAL built apps/web/dist
	distDir := filepath.Join("..", "..", "..", "apps", "web", "dist")
	if _, err := os.Stat(filepath.Join(distDir, "index.html")); err != nil {
		t.Fatalf("apps/web/dist missing (run astro build): %v", err)
	}
	shell := web.ServeUIShell(web.DefaultUIAllowlist(), os.DirFS(distDir))
	allUIRoutes := []string{
		"/", "/login", "/force-password-reset", "/two-factor-required",
		"/dashboard/canvas", "/analytics", "/metrics", "/canvas", "/canvas/tags",
		"/admin", "/onboarding", "/select-team", "/registries", "/subscription", "/subscription/new",
		"/profile", "/profile/appearance", "/profile/passkeys/confirm",
		"/servers", "/servers/canvas", "/servers/new", "/servers/new/hetzner", "/servers/new/hetzner/tok1",
		"/server/localhost", "/server/localhost/proxy", "/server/localhost/proxy/certificates",
		"/server/localhost/proxy/dynamic", "/server/localhost/proxy/logs",
		"/server/localhost/docker-cleanup", "/server/localhost/log-drains", "/server/localhost/sentinel",
		"/server/localhost/sentinel/logs", "/server/localhost/cloudflare-tunnel", "/server/localhost/destinations",
		"/server/localhost/github-runners", "/server/localhost/registries", "/server/localhost/security",
		"/server/localhost/security/patches", "/server/localhost/security/vulnerabilities", "/server/localhost/security/image-scanning",
		"/server/localhost/transfer", "/server/localhost/swarm", "/server/localhost/advanced", "/server/localhost/danger",
		"/destinations", "/destination/standalone-docker-0", "/destination/standalone-docker-0/resources", "/destination/standalone-docker-0/danger",
		"/projects", "/projects/canvas",
		"/project/p1", "/project/p1/edit", "/project/p1/environment/e1", "/project/p1/environment/e1/canvas",
		"/project/p1/environment/e1/application/a1", "/project/p1/environment/e1/application/a1/environment-variables",
		"/project/p1/environment/e1/application/a1/persistent-storage", "/project/p1/environment/e1/application/a1/scheduled-tasks",
		"/project/p1/environment/e1/application/a1/preview-deployments", "/project/p1/environment/e1/application/a1/webhooks",
		"/project/p1/environment/e1/application/a1/deployments", "/project/p1/environment/e1/application/a1/logs",
		"/project/p1/environment/e1/database/d1", "/project/p1/environment/e1/database/d1/backups",
		"/project/p1/environment/e1/service/s1", "/project/p1/environment/e1/service/s1/storages",
		"/databases", "/services", "/deployments",
		"/teams", "/team", "/team/members", "/team/audit-log", "/team/image-scanning", "/team/admin", "/team/danger",
		"/settings", "/settings/advanced", "/settings/updates", "/settings/image-scanning", "/settings/backup", "/settings/email", "/settings/oauth",
		"/notifications/email", "/notifications/discord", "/notifications/slack", "/notifications/telegram", "/notifications/pushover", "/notifications/webhook",
		"/storages", "/storages/s3-1", "/storages/s3-1/danger", "/storages/s3-1/resources",
		"/shared-variables/team", "/shared-variables/team/canvas", "/shared-variables/projects", "/shared-variables/environments", "/shared-variables/servers",
		"/tags", "/tags/prod",
		"/security", "/security/private-key", "/security/private-key/new", "/security/private-key/pk-0",
		"/security/integration-tokens", "/security/cloud-tokens/ct1", "/security/cloud-init-scripts", "/security/cloud-init-scripts/ci1", "/security/api-tokens",
		"/sources", "/source/github/gh1", "/source/github/gh1/danger", "/source/github/gh1/resources",
		"/source/gitlab/gl1", "/source/gitea/gt1",
		"/vulnerabilities", "/terminal",
	}
	for _, u := range allUIRoutes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, u, nil)
		shell.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("UI shell route %s failed: status=%d ct=%s", u, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}
