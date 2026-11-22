package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestDestinationResourcesAndOccupiedDeletion(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")
	var destinationID, environmentID, projectID int64
	for _, step := range []struct {
		query string
		id    *int64
	}{
		{`INSERT INTO standalone_dockers (uuid,name,network,server_id) VALUES ('ui-network','UI network','ui-network',0) RETURNING id`, &destinationID},
		{`INSERT INTO projects (uuid,name,team_id) VALUES ('ui-project','UI project',0) RETURNING id`, &projectID},
		{`INSERT INTO environments (uuid,name,project_id) VALUES ('ui-env','production',(SELECT id FROM projects WHERE uuid='ui-project')) RETURNING id`, &environmentID},
	} {
		if err := pool.QueryRow(ctx, step.query).Scan(step.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`INSERT INTO applications (uuid,name,environment_id,destination_id) VALUES ('ui-app','UI app',$1,$2)`,
		`INSERT INTO services (uuid,name,environment_id,destination_id,server_id) VALUES ('ui-service','UI service',$1,$2,0)`,
		`INSERT INTO standalone_redis (uuid,name,environment_id,destination_id) VALUES ('ui-redis','UI Redis',$1,$2)`,
	} {
		if _, err := pool.Exec(ctx, query, environmentID, destinationID); err != nil {
			t.Fatal(err)
		}
	}
	response := doJSON(t, http.MethodGet, ts.URL+"/api/v1/destinations/ui-network/resources", cookies, "", "", nil)
	var raw []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatalf("decode resources (status %d): %v", response.StatusCode, err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(raw) != 3 {
		t.Fatalf("expected all three resource types, got status %d and %#v", response.StatusCode, raw)
	}
	for _, resource := range raw {
		if resource["project_uuid"] != "ui-project" || resource["environment_uuid"] != "ui-env" {
			t.Fatalf("missing route context: %#v", resource)
		}
	}
	response = doJSON(t, http.MethodDelete, ts.URL+"/api/v1/destinations/ui-network", cookies, csrf, "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("occupied destination deletion returned %d", response.StatusCode)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM standalone_dockers WHERE uuid='ui-network'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("destination lost: count=%d err=%v", count, err)
	}
	reg := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{"name": "Other", "email": "ui-other@coolify.test", "password": "OtherSecret123!"})
	reg.Body.Close()
	otherCookies, _ := loginClient(t, ts.URL, "ui-other@coolify.test", "OtherSecret123!")
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/destinations/ui-network/resources", otherCookies, "", "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-team resources returned %d", response.StatusCode)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO standalone_dockers (uuid,name,network,server_id) VALUES ('ui-secondary','Secondary','secondary',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO additional_destinations (application_id,server_id,standalone_docker_id) SELECT a.id,0,d.id FROM applications a,standalone_dockers d WHERE a.uuid='ui-app' AND d.uuid='ui-secondary'`); err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/destinations/ui-secondary/resources", cookies, "", "", nil)
	var secondary []map[string]any
	json.NewDecoder(response.Body).Decode(&secondary)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(secondary) != 1 || secondary[0]["uuid"] != "ui-app" {
		t.Fatalf("secondary attachment missing: status %d %#v", response.StatusCode, secondary)
	}
	response = doJSON(t, http.MethodDelete, ts.URL+"/api/v1/destinations/ui-secondary", cookies, csrf, "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("secondary destination deletion returned %d", response.StatusCode)
	}
}
