package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestUINotificationConfigurationHydrationAndRedaction(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")
	response := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/discord", cookies, csrf, "", map[string]any{"enabled": false, "webhook_url": "https://notify.example.test/discord", "discord_ping_enabled": true})
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("save returned %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/notifications/discord", cookies, "", "", nil)
	var config map[string]any
	json.NewDecoder(response.Body).Decode(&config)
	response.Body.Close()
	if config["webhook_url"] != "https://notify.example.test/discord" || config["discord_ping_enabled"] != true {
		t.Fatalf("cannot hydrate saved configuration: %#v", config)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		response = doJSON(t, method, ts.URL+"/api/v1/notifications", cookies, csrf, "", map[string]any{"channel": "discord", "enabled": false})
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("collection toggle returned %d", response.StatusCode)
		}
		response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/notifications/discord", cookies, "", "", nil)
		config = nil
		json.NewDecoder(response.Body).Decode(&config)
		response.Body.Close()
		if config["webhook_url"] != "https://notify.example.test/discord" {
			t.Fatalf("collection %s discarded existing credentials", method)
		}
	}
	response = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/discord", cookies, csrf, "", map[string]any{"webhook_url": ""})
	response.Body.Close()
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/notifications/discord", cookies, "", "", nil)
	config = nil
	json.NewDecoder(response.Body).Decode(&config)
	response.Body.Close()
	if config["configured"] != false {
		t.Fatalf("clearing credential did not persist: %#v", config)
	}
	response = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/discord", cookies, csrf, "", map[string]any{"webhook_url": "https://notify.example.test/discord"})
	response.Body.Close()
	if _, err := pool.Exec(context.Background(), `INSERT INTO teams(id,name) VALUES(31,'Other owned team');INSERT INTO team_user(team_id,user_id,role) VALUES(31,0,'owner')`); err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/app/auth/switch-team", cookies, csrf, "", map[string]int{"team_id": 31})
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("owner switch returned %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/discord", cookies, csrf, "", map[string]any{"enabled": true, "expected_team_id": 0})
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale page save returned %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/notifications/test", cookies, csrf, "", map[string]any{"channel": "discord", "expected_team_id": 0})
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale page test returned %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/app/auth/switch-team", cookies, csrf, "", map[string]int{"team_id": 0})
	response.Body.Close()
	reg := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{"name": "Notification member", "email": "notify-member@coolify.test", "password": "MemberSecret123!"})
	reg.Body.Close()
	if _, err := pool.Exec(context.Background(), `INSERT INTO team_user(team_id,user_id,role) SELECT 0,id,'member' FROM users WHERE email='notify-member@coolify.test'`); err != nil {
		t.Fatal(err)
	}
	memberCookies, memberCSRF := loginClient(t, ts.URL, "notify-member@coolify.test", "MemberSecret123!")
	response = doJSON(t, http.MethodPost, ts.URL+"/app/auth/switch-team", memberCookies, memberCSRF, "", map[string]int{"team_id": 0})
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("switch returned %d", response.StatusCode)
	}
	response = doJSON(t, http.MethodGet, ts.URL+"/api/v1/notifications/discord", memberCookies, "", "", nil)
	config = nil
	json.NewDecoder(response.Body).Decode(&config)
	response.Body.Close()
	if _, leaked := config["webhook_url"]; leaked {
		t.Fatal("member could read the notification credential")
	}
	response = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/discord", memberCookies, memberCSRF, "", map[string]any{"enabled": true})
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member update returned %d", response.StatusCode)
	}
}
