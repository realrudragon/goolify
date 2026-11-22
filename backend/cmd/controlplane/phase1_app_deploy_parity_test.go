package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestPhase1_AppHealthcheckLimitsPrivateKeyAndRollback(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// Create project
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", cookies, csrf, "", map[string]string{"name": "Phase1 Parity"})
	var proj struct {
		DefaultEnvironment struct {
			UUID string `json:"uuid"`
		} `json:"default_environment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&proj); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Create a team-owned private key and a foreign-team private key
	_, err := pool.Exec(ctx, `INSERT INTO private_keys (uuid, name, private_key, team_id) VALUES ('pk-root-1', 'Root Deploy Key', '-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO teams (id, name, personal_team) VALUES (999, 'Foreign Team', FALSE) ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO private_keys (uuid, name, private_key, team_id) VALUES ('pk-foreign-1', 'Foreign Key', '-----BEGIN OPENSSH PRIVATE KEY-----\nxyz\n-----END OPENSSH PRIVATE KEY-----', 999)`)
	if err != nil {
		t.Fatal(err)
	}

	// Create application with railpack build pack and private deploy key
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/private-deploy-key", cookies, csrf, "", map[string]any{
		"name":             "Railpack Private App",
		"environment_uuid": proj.DefaultEnvironment.UUID,
		"server_uuid":      "localhost",
		"git_repository":   "git@github.com:org/private.git",
		"git_branch":       "main",
		"build_pack":       "railpack",
		"private_key_uuid": "pk-root-1",
		"ports_exposes":    "3000",
	})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 201/200 creating app, got %d", resp.StatusCode)
	}
	var app struct {
		UUID           string `json:"uuid"`
		BuildPack      string `json:"build_pack"`
		PrivateKeyUUID string `json:"private_key_uuid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if app.BuildPack != "railpack" || app.PrivateKeyUUID != "pk-root-1" {
		t.Fatalf("unexpected created app: %+v", app)
	}

	// Cross-team private key must be rejected with 403
	badKeyResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/"+app.UUID, cookies, csrf, "", map[string]any{
		"private_key_uuid": "pk-foreign-1",
	})
	badKeyResp.Body.Close()
	if badKeyResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 binding foreign-team private key, got %d", badKeyResp.StatusCode)
	}

	// Update healthcheck, resource limits, and custom labels
	patchResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/"+app.UUID, cookies, csrf, "", map[string]any{
		"health_check_enabled":  true,
		"health_check_path":     "/healthz",
		"health_check_interval": 10,
		"limits_memory":         "512m",
		"limits_cpus":           "1.5",
		"custom_labels":         "com.example.tier=web",
	})
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 patching app config, got %d", patchResp.StatusCode)
	}
	var updated struct {
		HealthCheckEnabled  bool   `json:"health_check_enabled"`
		HealthCheckPath     string `json:"health_check_path"`
		HealthCheckInterval int    `json:"health_check_interval"`
		LimitsMemory        string `json:"limits_memory"`
		LimitsCPUs          string `json:"limits_cpus"`
		CustomLabels        string `json:"custom_labels"`
	}
	if err := json.NewDecoder(patchResp.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	patchResp.Body.Close()
	if !updated.HealthCheckEnabled || updated.HealthCheckPath != "/healthz" || updated.HealthCheckInterval != 10 || updated.LimitsMemory != "512m" || updated.LimitsCPUs != "1.5" || updated.CustomLabels != "com.example.tier=web" {
		t.Fatalf("app config did not persist: %+v", updated)
	}

	// Seed a finished deployment and trigger rollback via POST /api/v1/applications/{uuid}/rollback
	var appID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM applications WHERE uuid = $1`, app.UUID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO application_deployment_queues (deployment_uuid, application_id, application_name, server_id, server_name, status, commit)
		VALUES ('dep-prev-1', $1, 'Railpack Private App', 0, 'localhost', 'finished', 'abc1234')
	`, appID)
	if err != nil {
		t.Fatal(err)
	}
	rbResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+app.UUID+"/rollback", cookies, csrf, "", map[string]string{"commit": "dep-prev-1"})
	if rbResp.StatusCode != http.StatusAccepted && rbResp.StatusCode != http.StatusOK {
		var errBody map[string]any
		_ = json.NewDecoder(rbResp.Body).Decode(&errBody)
		t.Fatalf("expected 202/200 rollback response, got %d body=%v", rbResp.StatusCode, errBody)
	}
	var rbBody struct {
		DeploymentUUID string `json:"deployment_uuid"`
	}
	if err := json.NewDecoder(rbResp.Body).Decode(&rbBody); err != nil {
		t.Fatal(err)
	}
	rbResp.Body.Close()
	if rbBody.DeploymentUUID == "" {
		t.Fatalf("unexpected rollback response: %+v", rbBody)
	}
	var isRollback bool
	if err := pool.QueryRow(ctx, `SELECT rollback FROM application_deployment_queues WHERE deployment_uuid = $1`, rbBody.DeploymentUUID).Scan(&isRollback); err != nil || !isRollback {
		t.Fatalf("expected rollback=true in DB, got %v err=%v", isRollback, err)
	}
}
