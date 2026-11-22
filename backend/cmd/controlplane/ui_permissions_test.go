package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestUIResourcePermissionsUseLoadedResourceTeamAndGrants(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	for _, query := range []string{
		`INSERT INTO projects(uuid,name,team_id) VALUES('permissions-project','Permissions',0)`,
		`INSERT INTO environments(uuid,name,project_id) SELECT 'permissions-env','production',id FROM projects WHERE uuid='permissions-project'`,
		`INSERT INTO applications(uuid,name,environment_id,destination_id) SELECT 'permissions-app','Granted',id,0 FROM environments WHERE uuid='permissions-env'`,
		`INSERT INTO applications(uuid,name,environment_id,destination_id) SELECT 'permissions-other','Outside grant',id,0 FROM environments WHERE uuid='permissions-env'`,
		`INSERT INTO services(uuid,name,environment_id,server_id,docker_compose_raw) SELECT 'permissions-service','Granted service',id,0,'private-config-value' FROM environments WHERE uuid='permissions-env'`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	response := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{"name": "Granted member", "email": "permissions@coolify.test", "password": "MemberSecret123!"})
	response.Body.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO team_user(team_id,user_id,role,access) SELECT 0,id,'member','[{"scope":"resource","uuid":"permissions-app","levels":["write"]}]'::jsonb FROM users WHERE email='permissions@coolify.test'`); err != nil {
		t.Fatal(err)
	}
	cookies, csrf := loginClient(t, ts.URL, "permissions@coolify.test", "MemberSecret123!")
	endpoint := ts.URL + "/api/v1/resource-permissions?type=application&uuid=permissions-app"
	read := func() map[string]bool {
		t.Helper()
		res := doJSON(t, http.MethodGet, endpoint, cookies, "", "", nil)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("permissions returned %d", res.StatusCode)
		}
		var result map[string]bool
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	granted := read()
	if !granted["update"] || !granted["deploy"] {
		t.Fatalf("write grant lost while session is in personal team: %#v", granted)
	}
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/resource-permissions?type=application&uuid=permissions-other", cookies, "", "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("outside grant returned %d", response.StatusCode)
	}
	if _, err := pool.Exec(ctx, `UPDATE team_user SET access='[{"scope":"resource","uuid":"permissions-app","levels":["deploy"]}]'::jsonb WHERE team_id=0 AND user_id=(SELECT id FROM users WHERE email='permissions@coolify.test')`); err != nil {
		t.Fatal(err)
	}
	deployOnly := read()
	if deployOnly["update"] || !deployOnly["deploy"] {
		t.Fatalf("deploy grant was treated as edit: %#v", deployOnly)
	}
	if _, err := pool.Exec(ctx, `UPDATE team_user SET access='[{"scope":"project","uuid":"permissions-project","levels":["write"]}]'::jsonb WHERE team_id=0 AND user_id=(SELECT id FROM users WHERE email='permissions@coolify.test')`); err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/permissions-app", cookies, csrf, "", map[string]string{"name": "Edited through project grant"})
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("project-grant edit returned %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/permissions-app/stop", cookies, csrf, "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("project-grant lifecycle returned %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/services/permissions-service", cookies, "", "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("switched-session service read returned %d", response.StatusCode)
	}
	if _, err := pool.Exec(ctx, `UPDATE team_user SET access='[{"scope":"resource","uuid":"permissions-other","levels":["delete"]}]'::jsonb WHERE team_id=0 AND user_id=(SELECT id FROM users WHERE email='permissions@coolify.test')`); err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, http.MethodDelete, ts.URL+"/api/v1/applications/permissions-other", cookies, csrf, "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete-only grant returned %d", response.StatusCode)
	}
	if _, err := pool.Exec(ctx, `UPDATE team_user SET access=NULL WHERE team_id=0 AND user_id=(SELECT id FROM users WHERE email='permissions@coolify.test')`); err != nil {
		t.Fatal(err)
	}
	readonly := read()
	if readonly["update"] || readonly["deploy"] || readonly["delete"] || readonly["read_sensitive"] {
		t.Fatalf("personal-team ownership leaked into resource permissions: %#v", readonly)
	}
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/services/permissions-service", cookies, "", "", nil)
	var service map[string]any
	json.NewDecoder(response.Body).Decode(&service)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || service["docker_compose_raw"] == "private-config-value" {
		t.Fatalf("readonly service data was not redacted: status=%d %#v", response.StatusCode, service)
	}
	response = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/permissions-app", cookies, csrf, "", map[string]string{"name": "Unauthorized"})
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly mutation returned %d", response.StatusCode)
	}
}
