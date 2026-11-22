package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestUICreationPreservesConfiguration(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")
	response := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", cookies, csrf, "", map[string]string{"name": "UI configuration"})
	var project struct {
		Project struct {
			UUID string `json:"uuid"`
		}
		DefaultEnvironment struct {
			UUID string `json:"uuid"`
		} `json:"default_environment"`
	}
	if err := json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects/"+project.Project.UUID+"/environments", cookies, csrf, "", map[string]string{"name": "staging", "description": "Staging configuration"})
	var environment struct {
		UUID string `json:"uuid"`
	}
	json.NewDecoder(response.Body).Decode(&environment)
	response.Body.Close()
	var description string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(description,'') FROM environments WHERE uuid=$1`, environment.UUID).Scan(&description); err != nil || description != "Staging configuration" {
		t.Fatalf("description lost: %q %v", description, err)
	}
	var destinationID int64
	if err := pool.QueryRow(ctx, `INSERT INTO standalone_dockers (uuid,name,network,server_id) VALUES ('chosen-network','Chosen network','chosen',0) RETURNING id`).Scan(&destinationID); err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerfile", cookies, csrf, "", map[string]string{"name": "Inline Dockerfile", "environment_uuid": environment.UUID, "server_uuid": "localhost", "destination_uuid": "chosen-network", "dockerfile": "FROM alpine:3.21\nRUN printf '%s' '$value'\n"})
	var app struct {
		UUID          string `json:"uuid"`
		Dockerfile    string `json:"dockerfile"`
		DestinationID int64  `json:"destination_id"`
	}
	json.NewDecoder(response.Body).Decode(&app)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || app.DestinationID != destinationID || app.Dockerfile != "FROM alpine:3.21\nRUN printf '%s' '$value'\n" {
		t.Fatalf("creation discarded configuration: status=%d response=%#v", response.StatusCode, app)
	}
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+app.UUID, cookies, "", "", nil)
	var reloaded struct {
		Dockerfile    string `json:"dockerfile"`
		DestinationID int64  `json:"destination_id"`
	}
	json.NewDecoder(response.Body).Decode(&reloaded)
	response.Body.Close()
	if reloaded.Dockerfile != app.Dockerfile || reloaded.DestinationID != destinationID {
		t.Fatalf("reload lost configuration: %#v", reloaded)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+app.UUID+"/clone", cookies, csrf, "", map[string]string{"name": "Cloned Dockerfile"})
	var cloned struct {
		Dockerfile    string `json:"dockerfile"`
		DestinationID int64  `json:"destination_id"`
	}
	json.NewDecoder(response.Body).Decode(&cloned)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || cloned.Dockerfile != app.Dockerfile || cloned.DestinationID != destinationID {
		t.Fatalf("clone lost configuration: status=%d %#v", response.StatusCode, cloned)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO teams(id,name) VALUES(29,'Other');INSERT INTO servers(id,uuid,name,ip,team_id,private_key_id) VALUES(29,'other-ui-server','Other','127.0.0.2',29,0);INSERT INTO standalone_dockers(uuid,name,network,server_id) VALUES('foreign-ui-destination','Other','other',29)`); err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerfile", cookies, csrf, "", map[string]string{"name": "Invalid placement", "environment_uuid": environment.UUID, "server_uuid": "localhost", "destination_uuid": "foreign-ui-destination", "dockerfile": "FROM alpine"})
	response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("foreign destination returned %d", response.StatusCode)
	}
	var invalidCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM applications WHERE name='Invalid placement'`).Scan(&invalidCount); err != nil || invalidCount != 0 {
		t.Fatalf("invalid placement persisted: %d %v", invalidCount, err)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/transfer", cookies, csrf, "", map[string]any{"mark_transferred_away": false})
	var bundle map[string]any
	if err := json.NewDecoder(response.Body).Decode(&bundle); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("export failed: %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/import", cookies, csrf, "", map[string]any{"target_team_id": 0, "bundle": bundle})
	response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		t.Fatalf("import failed: %d", response.StatusCode)
	}
	var transferred string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(dockerfile,'') FROM applications WHERE name='Inline Dockerfile' AND uuid<>$1 LIMIT 1`, app.UUID).Scan(&transferred); err != nil || transferred != app.Dockerfile {
		t.Fatalf("transfer lost Dockerfile: %q %v", transferred, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(e.description,'') FROM environments e JOIN applications a ON a.environment_id=e.id WHERE a.name='Inline Dockerfile' AND a.uuid<>$1 LIMIT 1`, app.UUID).Scan(&description); err != nil || description != "Staging configuration" {
		t.Fatalf("transfer lost environment description: %q %v", description, err)
	}
}
