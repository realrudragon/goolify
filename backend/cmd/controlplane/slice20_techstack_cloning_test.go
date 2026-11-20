package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/applications"
	"github.com/coollabsio/coolify/backend/internal/domains/deployments"
	"github.com/coollabsio/coolify/backend/internal/domains/transfer"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

func TestProductionSlice20_TechStackCloningTemplatesAndFullParity(t *testing.T) {
	t.Setenv("COOLIFY_TEMPLATES_DIR", "../../../templates")
	_, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()

	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 1. Service template catalog (382+ Coolify one-click templates)
	tplResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/services/templates", rootCookies, rootCSRF, "", nil)
	if tplResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/services/templates failed: %d %s", tplResp.StatusCode, readRespString(tplResp))
	}
	var tplList []struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Category string `json:"category"`
	}
	if err := json.NewDecoder(tplResp.Body).Decode(&tplList); err != nil {
		t.Fatalf("decode templates: %v", err)
	}
	tplResp.Body.Close()
	if len(tplList) < 350 {
		t.Fatalf("expected >= 350 service templates, got %d", len(tplList))
	}

	singleTpl := doJSON(t, http.MethodGet, ts.URL+"/api/v1/services/templates?type=activepieces", rootCookies, rootCSRF, "", nil)
	if singleTpl.StatusCode != http.StatusOK {
		t.Fatalf("GET template ?type=activepieces failed: %d", singleTpl.StatusCode)
	}
	var apTpl struct {
		Type       string `json:"type"`
		ComposeRaw string `json:"docker_compose_raw"`
	}
	_ = json.NewDecoder(singleTpl.Body).Decode(&apTpl)
	singleTpl.Body.Close()
	if !strings.Contains(apTpl.ComposeRaw, "activepieces") {
		t.Fatalf("expected activepieces compose yaml, got: %s", apTpl.ComposeRaw)
	}

	// Create project for resources
	projResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]string{
		"name": "Slice20 TechStack", "description": "Full stack templates and clone",
	})
	if projResp.StatusCode != http.StatusCreated {
		t.Fatalf("create project failed: %d", projResp.StatusCode)
	}
	var projOut struct {
		Project struct {
			ID   int64  `json:"id"`
			UUID string `json:"uuid"`
		} `json:"project"`
		Environment struct {
			ID   int64  `json:"id"`
			UUID string `json:"uuid"`
		} `json:"default_environment"`
	}
	_ = json.NewDecoder(projResp.Body).Decode(&projOut)
	projResp.Body.Close()

	// 2. Instantiate one-click service templates (activepieces, actualbudget, 9router + base64 raw compose) & verify sub-resources in Postgres
	var activePiecesSvcUUID string
	for _, slug := range []string{"activepieces", "actualbudget", "9router"} {
		res := doJSON(t, http.MethodPost, ts.URL+"/api/v1/services", rootCookies, rootCSRF, "", map[string]string{
			"type":         slug,
			"project_uuid": projOut.Project.UUID,
			"server_uuid":  "localhost",
		})
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("create one-click service %s failed: %d %s", slug, res.StatusCode, readRespString(res))
		}
		var createdSvc struct {
			ID   int64  `json:"id"`
			UUID string `json:"uuid"`
		}
		_ = json.NewDecoder(res.Body).Decode(&createdSvc)
		res.Body.Close()
		if slug == "activepieces" {
			activePiecesSvcUUID = createdSvc.UUID
		}

		var appCount, dbCount int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM service_applications WHERE service_id = $1`, createdSvc.ID).Scan(&appCount)
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM service_databases WHERE service_id = $1`, createdSvc.ID).Scan(&dbCount)
		if appCount == 0 {
			t.Fatalf("expected service_applications auto-populated for template %s", slug)
		}
		if slug == "activepieces" && dbCount < 2 {
			t.Fatalf("expected >= 2 service_databases (postgres+redis) for activepieces, got %d", dbCount)
		}
	}

	// Base64 docker_compose_raw compatibility
	b64Compose := base64.StdEncoding.EncodeToString([]byte("services:\n  api:\n    image: node:24-alpine\n"))
	b64Res := doJSON(t, http.MethodPost, ts.URL+"/api/v1/services", rootCookies, rootCSRF, "", map[string]string{
		"name":               "b64-service",
		"environment_uuid":   projOut.Environment.UUID,
		"server_uuid":        "localhost",
		"docker_compose_raw": b64Compose,
	})
	if b64Res.StatusCode != http.StatusCreated {
		t.Fatalf("base64 service create failed: %d %s", b64Res.StatusCode, readRespString(b64Res))
	}
	b64Res.Body.Close()

	// 3. Create apps across all build packs & verify idempotent git clone/fetch deployment planner
	planner := deployments.NewDeploymentPlanner()
	appSvc := applications.NewPostgresService(pool, access.NewAuthorizer(), compat.NewEloquentHooks(), ciph, realtime.NewPostgresOutboxStore(pool), jobs.NewPostgresJobCoordinator(pool, 2))
	sysP := auth.Principal{UserID: 0, AuthMethod: "session", SessionTeamID: 0, TeamRoles: map[int64]auth.Role{0: auth.RoleOwner}, IsInstanceAdmin: true}

	buildPackEndpoints := []struct {
		endpoint string
		wantBP   string
	}{
		{"public", "railpack"},
		{"dockerfile", "dockerfile"},
		{"dockerimage", "dockerimage"},
		{"dockercompose", "dockercompose"},
		{"static", "static"},
		{"private-github-app", "railpack"},
		{"private-deploy-key", "railpack"},
	}

	var primaryApp struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	for idx, tc := range buildPackEndpoints {
		res := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+tc.endpoint, rootCookies, rootCSRF, "", map[string]any{
			"name":                      "app-" + tc.endpoint,
			"project_uuid":              projOut.Project.UUID,
			"server_uuid":               "localhost",
			"git_repository":            "https://github.com/coollabsio/coolify-examples",
			"git_branch":                "main",
			"docker_registry_image_name": "nginx",
			"docker_registry_image_tag": "alpine",
			"docker_compose_raw":        "services:\n  web:\n    image: nginx:alpine\n",
		})
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("create app %s failed: %d %s", tc.endpoint, res.StatusCode, readRespString(res))
		}
		var createdApp struct {
			ID        int64  `json:"id"`
			UUID      string `json:"uuid"`
			BuildPack string `json:"build_pack"`
		}
		_ = json.NewDecoder(res.Body).Decode(&createdApp)
		res.Body.Close()
		if createdApp.BuildPack != tc.wantBP {
			t.Fatalf("endpoint %s: got build_pack=%s want %s", tc.endpoint, createdApp.BuildPack, tc.wantBP)
		}
		if idx == 0 {
			primaryApp.ID = createdApp.ID
			primaryApp.UUID = createdApp.UUID
		}
		dctx, err := appSvc.Resolver().Resolve(ctx, sysP, createdApp.UUID, nil)
		if err != nil {
			t.Fatalf("resolve deployment context for %s: %v", createdApp.UUID, err)
		}
		plan, err := planner.BuildPlan(dctx, "op-"+createdApp.UUID, "")
		if err != nil {
			t.Fatalf("BuildPlan for %s: %v", createdApp.UUID, err)
		}
		joined := strings.Join(plan.Commands, " && ")
		if tc.wantBP != "dockerimage" && !strings.Contains(joined, "git clone --depth=1") {
			t.Fatalf("expected git clone in plan for %s, got: %s", tc.wantBP, joined)
		}
	}

	// 4. Deep Resource Cloning (Application, Service, Database, Environment)
	// Seed env var, custom setting, storage volume, scheduled task & tag onto primaryApp
	_, _ = pool.Exec(ctx, `UPDATE application_settings SET disable_build_cache = TRUE WHERE application_id = $1`, primaryApp.ID)
	_, _ = pool.Exec(ctx, `INSERT INTO environment_variables (uuid, key, value, resourceable_type, resourceable_id) VALUES ('ev-s20-1', 'STACK_KEY', 'val-1', 'App\Models\Application', $1)`, primaryApp.ID)
	_, _ = pool.Exec(ctx, `INSERT INTO local_persistent_volumes (uuid, name, mount_path, resource_type, resource_id) VALUES ('vol-s20-1', $1, '/data', 'App\Models\Application', $2)`, primaryApp.UUID+"_data", primaryApp.ID)
	_, _ = pool.Exec(ctx, `INSERT INTO scheduled_tasks (uuid, name, command, frequency, application_id, team_id) VALUES ('task-s20-1', 'nightly', 'echo ok', '0 0 * * *', $1, 0)`, primaryApp.ID)
	tagResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+primaryApp.UUID+"/tags", rootCookies, rootCSRF, "", map[string]string{"name": "prod-stack"})
	tagResp.Body.Close()

	cloneAppResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+primaryApp.UUID+"/clone", rootCookies, rootCSRF, "", map[string]string{"name": "cloned-app"})
	if cloneAppResp.StatusCode != http.StatusCreated {
		t.Fatalf("clone app failed: %d %s", cloneAppResp.StatusCode, readRespString(cloneAppResp))
	}
	var clonedApp struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(cloneAppResp.Body).Decode(&clonedApp)
	cloneAppResp.Body.Close()

	var disableCache bool
	var volName string
	var taskCount, tagCount int
	_ = pool.QueryRow(ctx, `SELECT disable_build_cache FROM application_settings WHERE application_id = $1`, clonedApp.ID).Scan(&disableCache)
	_ = pool.QueryRow(ctx, `SELECT name FROM local_persistent_volumes WHERE resource_type = 'App\Models\Application' AND resource_id = $1`, clonedApp.ID).Scan(&volName)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM scheduled_tasks WHERE application_id = $1`, clonedApp.ID).Scan(&taskCount)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM taggables WHERE taggable_type = 'App\Models\Application' AND taggable_id = $1`, clonedApp.ID).Scan(&tagCount)
	if !disableCache || !strings.HasPrefix(volName, clonedApp.UUID+"_") || taskCount != 1 || tagCount != 1 {
		t.Fatalf("deep app clone mismatch: disableCache=%v vol=%q tasks=%d tags=%d", disableCache, volName, taskCount, tagCount)
	}

	// Database create + backup schedule + clone
	dbRes := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/postgresql", rootCookies, rootCSRF, "", map[string]string{
		"name": "src-pg", "environment_uuid": projOut.Environment.UUID, "server_uuid": "localhost",
	})
	if dbRes.StatusCode != http.StatusCreated {
		t.Fatalf("create db failed: %d", dbRes.StatusCode)
	}
	var srcDB struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(dbRes.Body).Decode(&srcDB)
	dbRes.Body.Close()
	bkRes := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/"+srcDB.UUID+"/backups", rootCookies, rootCSRF, "", map[string]any{
		"frequency": "0 3 * * *", "enabled": true,
	})
	bkRes.Body.Close()

	cloneDBResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/"+srcDB.UUID+"/clone", rootCookies, rootCSRF, "", map[string]string{"name": "cloned-pg"})
	if cloneDBResp.StatusCode != http.StatusCreated {
		t.Fatalf("clone db failed: %d %s", cloneDBResp.StatusCode, readRespString(cloneDBResp))
	}
	var clonedDB struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(cloneDBResp.Body).Decode(&clonedDB)
	cloneDBResp.Body.Close()
	var clonedBackups int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM scheduled_database_backups WHERE database_id = $1 AND database_type IN ('App\Models\StandalonePostgresql','postgresql')`, clonedDB.ID).Scan(&clonedBackups)
	if clonedBackups != 1 {
		t.Fatalf("expected cloned database to copy backup schedule, got %d", clonedBackups)
	}

	// Service deep clone (including service_applications + service_databases)
	cloneSvcResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/services/"+activePiecesSvcUUID+"/clone", rootCookies, rootCSRF, "", map[string]string{"name": "cloned-activepieces"})
	if cloneSvcResp.StatusCode != http.StatusCreated {
		t.Fatalf("clone service failed: %d %s", cloneSvcResp.StatusCode, readRespString(cloneSvcResp))
	}
	var clonedSvc struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(cloneSvcResp.Body).Decode(&clonedSvc)
	cloneSvcResp.Body.Close()
	var cSvcApps, cSvcDBs int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM service_applications WHERE service_id = $1`, clonedSvc.ID).Scan(&cSvcApps)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM service_databases WHERE service_id = $1`, clonedSvc.ID).Scan(&cSvcDBs)
	if cSvcApps == 0 || cSvcDBs < 2 {
		t.Fatalf("expected deep service clone to copy sub-resources, got apps=%d dbs=%d", cSvcApps, cSvcDBs)
	}

	// Environment clone
	cloneEnvResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects/"+projOut.Project.UUID+"/environments/"+projOut.Environment.UUID+"/clone", rootCookies, rootCSRF, "", map[string]string{"name": "staging"})
	if cloneEnvResp.StatusCode != http.StatusCreated {
		t.Fatalf("clone environment failed: %d %s", cloneEnvResp.StatusCode, readRespString(cloneEnvResp))
	}
	var clonedEnv struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(cloneEnvResp.Body).Decode(&clonedEnv)
	cloneEnvResp.Body.Close()
	var clonedEnvApps, clonedEnvSvcs int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM applications WHERE environment_id = $1`, clonedEnv.ID).Scan(&clonedEnvApps)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM services WHERE environment_id = $1`, clonedEnv.ID).Scan(&clonedEnvSvcs)
	if clonedEnvApps < 7 || clonedEnvSvcs < 4 {
		t.Fatalf("expected cloned environment to duplicate apps (%d) and services (%d)", clonedEnvApps, clonedEnvSvcs)
	}

	// 5. Feedback /api/v1/feedback (subject + description payload)
	fbRes := doJSON(t, http.MethodPost, ts.URL+"/api/v1/feedback", rootCookies, rootCSRF, "", map[string]string{
		"subject": "Parity check", "description": "Testing live feedback endpoint",
	})
	if fbRes.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/feedback failed: %d", fbRes.StatusCode)
	}
	fbRes.Body.Close()

	// 6. Full Server Transfer (Apps + Services + Databases + Backups + Volumes + Tags) + Transferred Sentinel Lock + Cross-Team Avatar 403
	_, _ = pool.Exec(ctx, `INSERT INTO teams (id, name, personal_team) VALUES (20, 'Target Transfer Team', FALSE) ON CONFLICT DO NOTHING`)
	_, _ = pool.Exec(ctx, `INSERT INTO team_user (team_id, user_id, role) VALUES (20, 0, 'owner') ON CONFLICT DO NOTHING`)
	regOther := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Isolated User", "email": "iso-s20@coolify.test", "password": "IsoSecret123!",
	})
	regOther.Body.Close()
	isoCookies, _ := loginClient(t, ts.URL, "iso-s20@coolify.test", "IsoSecret123!")

	// Cross-team member avatar lookup for root user (id=0) must return 403 Forbidden
	avResp := doJSON(t, http.MethodGet, ts.URL+"/team/members/0/avatar", isoCookies, "", "", nil)
	if avResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-team /team/members/0/avatar to return 403, got %d", avResp.StatusCode)
	}
	avResp.Body.Close()

	// Export dedicated non-localhost server on team 0 and mark transferred-away
	srvRes := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers", rootCookies, rootCSRF, "", map[string]any{
		"name": "xfer-node", "ip": "10.0.0.55", "private_key_uuid": "pk-0", "user": "root",
	})
	if srvRes.StatusCode != http.StatusCreated {
		t.Fatalf("create server xfer-node failed: %d %s", srvRes.StatusCode, readRespString(srvRes))
	}
	var xferSrv struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(srvRes.Body).Decode(&xferSrv)
	srvRes.Body.Close()
	_, _ = pool.Exec(ctx, `INSERT INTO server_settings (server_id, sentinel_token) VALUES ($1, 'sent-tok-s20') ON CONFLICT (server_id) DO UPDATE SET sentinel_token = 'sent-tok-s20'`, xferSrv.ID)

	// Place an app, service, and database on xferSrv
	xApp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]string{
		"name": "xfer-app", "environment_uuid": projOut.Environment.UUID, "server_uuid": xferSrv.UUID, "docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
	})
	xApp.Body.Close()
	xSvc := doJSON(t, http.MethodPost, ts.URL+"/api/v1/services", rootCookies, rootCSRF, "", map[string]string{
		"type": "actualbudget", "environment_uuid": projOut.Environment.UUID, "server_uuid": xferSrv.UUID,
	})
	xSvc.Body.Close()
	xDB := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/postgresql", rootCookies, rootCSRF, "", map[string]string{
		"name": "xfer-pg", "environment_uuid": projOut.Environment.UUID, "server_uuid": xferSrv.UUID,
	})
	xDB.Body.Close()

	xferSvc := transfer.NewPostgresTransferService(pool, access.NewAuthorizer(), ciph, compat.NewEloquentHooks())
	targetOwner := auth.Principal{UserID: 0, AuthMethod: "session", IsInstanceAdmin: true, TeamRoles: map[int64]auth.Role{0: auth.RoleOwner, 20: auth.RoleOwner}}
	bundle, err := xferSvc.ExportServer(ctx, targetOwner, xferSrv.UUID, true)
	if err != nil {
		t.Fatalf("ExportServer failed: %v", err)
	}
	// Verify exported bundle contains applications, services (+sub-resources), and standalone databases
	var totalApps, totalSvcs, totalDBs int
	for _, pr := range bundle.Projects {
		for _, en := range pr.Environments {
			totalApps += len(en.Applications)
			totalSvcs += len(en.Services)
			totalDBs += len(en.Databases)
		}
	}
	if totalApps == 0 || totalSvcs == 0 || totalDBs == 0 {
		t.Fatalf("expected full graph export (apps=%d svcs=%d dbs=%d)", totalApps, totalSvcs, totalDBs)
	}
	receipt, err := xferSvc.ImportServer(ctx, targetOwner, 20, bundle)
	if err != nil {
		t.Fatalf("ImportServer into team 20 failed: %v", err)
	}
	var importedSvcs, importedDBs int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM services sv JOIN environments e ON e.id = sv.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = 20`).Scan(&importedSvcs)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM standalone_postgresqls d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = 20`).Scan(&importedDBs)
	if importedSvcs == 0 || importedDBs == 0 || receipt.NewServerUUID == "" {
		t.Fatalf("expected imported services (%d) and databases (%d) on team 20", importedSvcs, importedDBs)
	}

	// Sentinel push on transferred-away archive must be rejected (401)
	sentReq := doJSON(t, http.MethodPost, ts.URL+"/api/v1/sentinel/push", nil, "", "sent-tok-s20", map[string]any{
		"server_uuid": xferSrv.UUID, "cpu_percent": 12.5,
	})
	if sentReq.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected transferred-away server sentinel push to be rejected (401), got %d", sentReq.StatusCode)
	}
	sentReq.Body.Close()
}
