package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhase2_S3ValidationDatabaseRotationAndServerMetricsAdvanced(t *testing.T) {
	_, ts, _, _, _ := setupRealControlPlane(t)
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 1. S3 Storage real HTTP SigV4 validation (pass & fail)
	s3OK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3OK.Close()

	s3Fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer s3Fail.Close()

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/s3-storages", cookies, csrf, "", map[string]any{
		"name": "Real S3 OK", "bucket": "backups", "region": "us-east-1", "endpoint": s3OK.URL, "key": "AKIA1", "secret": "SECRET1",
	})
	var s3Good struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(resp.Body).Decode(&s3Good)
	resp.Body.Close()

	valOK := doJSON(t, http.MethodPost, ts.URL+"/api/v1/s3-storages/"+s3Good.UUID+"/validate", cookies, csrf, "", nil)
	var valOKBody struct{ IsUsable bool `json:"is_usable"` }
	_ = json.NewDecoder(valOK.Body).Decode(&valOKBody)
	valOK.Body.Close()
	if valOK.StatusCode != http.StatusOK || !valOKBody.IsUsable {
		t.Fatalf("expected valid S3 to return 200 is_usable=true, got %d %+v", valOK.StatusCode, valOKBody)
	}

	resp = doJSON(t, http.MethodPost, ts.URL+"/api/v1/s3-storages", cookies, csrf, "", map[string]any{
		"name": "Real S3 Bad", "bucket": "backups", "region": "us-east-1", "endpoint": s3Fail.URL, "key": "BAD", "secret": "BAD",
	})
	var s3Bad struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(resp.Body).Decode(&s3Bad)
	resp.Body.Close()

	valBad := doJSON(t, http.MethodPost, ts.URL+"/api/v1/s3-storages/"+s3Bad.UUID+"/validate", cookies, csrf, "", nil)
	var valBadBody struct{ IsUsable bool `json:"is_usable"` }
	_ = json.NewDecoder(valBad.Body).Decode(&valBadBody)
	valBad.Body.Close()
	if valBad.StatusCode != http.StatusUnprocessableEntity || valBadBody.IsUsable {
		t.Fatalf("expected invalid S3 to return 422 is_usable=false, got %d %+v", valBad.StatusCode, valBadBody)
	}

	// 2. Database configuration & password rotation
	projResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", cookies, csrf, "", map[string]string{"name": "Phase2 DB"})
	var proj struct {
		DefaultEnvironment struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(projResp.Body).Decode(&proj)
	projResp.Body.Close()

	dbCreate := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/postgresql", cookies, csrf, "", map[string]any{
		"name": "Phase2 PG", "environment_uuid": proj.DefaultEnvironment.UUID, "server_uuid": "localhost",
	})
	var dbCreated struct {
		UUID     string `json:"uuid"`
		Password string `json:"password"`
	}
	_ = json.NewDecoder(dbCreate.Body).Decode(&dbCreated)
	dbCreate.Body.Close()

	dbPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/databases/"+dbCreated.UUID, cookies, csrf, "", map[string]any{
		"name": "Phase2 PG Renamed", "description": "Primary DB", "image": "postgres:16.4-alpine", "is_public": true, "public_port": 5439,
	})
	var dbUpdated struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		Image         string `json:"image"`
		IsPublic      bool   `json:"is_public"`
		PublicPort    int    `json:"public_port"`
		Password      string `json:"password"`
		InternalDBURL string `json:"internal_db_url"`
		ExternalDBURL string `json:"external_db_url"`
	}
	_ = json.NewDecoder(dbPatch.Body).Decode(&dbUpdated)
	dbPatch.Body.Close()
	if dbPatch.StatusCode != http.StatusOK || dbUpdated.Name != "Phase2 PG Renamed" || !dbUpdated.IsPublic || dbUpdated.PublicPort != 5439 || dbUpdated.InternalDBURL == "" || dbUpdated.ExternalDBURL == "" {
		t.Fatalf("database config update failed: status=%d body=%+v", dbPatch.StatusCode, dbUpdated)
	}

	rotResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/"+dbCreated.UUID+"/rotate-password", cookies, csrf, "", nil)
	var dbRotated struct {
		Password      string `json:"password"`
		InternalDBURL string `json:"internal_db_url"`
	}
	_ = json.NewDecoder(rotResp.Body).Decode(&dbRotated)
	rotResp.Body.Close()
	if rotResp.StatusCode != http.StatusOK || dbRotated.Password == "" || dbRotated.Password == dbUpdated.Password || !strings.Contains(dbRotated.InternalDBURL, dbRotated.Password) {
		t.Fatalf("password rotation did not produce a new password: status=%d old=%q new=%q", rotResp.StatusCode, dbUpdated.Password, dbRotated.Password)
	}

	// 3. Server advanced settings & live/historical metrics
	advPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/servers/localhost/advanced", cookies, csrf, "", map[string]any{
		"server_timezone":                          "Asia/Bangkok",
		"wildcard_domain":                          "https://apps.example.com",
		"concurrent_builds":                        4,
		"server_disk_usage_notification_threshold": 85,
		"is_swarm_manager":                         true,
	})
	var advBody struct {
		Timezone         string `json:"server_timezone"`
		WildcardDomain   string `json:"wildcard_domain"`
		ConcurrentBuilds int    `json:"concurrent_builds"`
		DiskThreshold    int    `json:"server_disk_usage_notification_threshold"`
		IsSwarmManager   bool   `json:"is_swarm_manager"`
	}
	_ = json.NewDecoder(advPatch.Body).Decode(&advBody)
	advPatch.Body.Close()
	if advPatch.StatusCode != http.StatusOK || advBody.Timezone != "Asia/Bangkok" || advBody.WildcardDomain != "https://apps.example.com" || advBody.ConcurrentBuilds != 4 || advBody.DiskThreshold != 85 || !advBody.IsSwarmManager {
		t.Fatalf("server advanced settings did not persist: status=%d body=%+v", advPatch.StatusCode, advBody)
	}

	metResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers/localhost/metrics", cookies, csrf, "", nil)
	var metBody struct {
		HasMetrics bool             `json:"has_metrics"`
		History    []map[string]any `json:"history"`
	}
	_ = json.NewDecoder(metResp.Body).Decode(&metBody)
	metResp.Body.Close()
	if metResp.StatusCode != http.StatusOK || !metBody.HasMetrics || len(metBody.History) == 0 {
		t.Fatalf("expected live localhost metrics history, got status=%d body=%+v", metResp.StatusCode, metBody)
	}
}
