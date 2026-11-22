package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestUISourceConfigurationPersists(t *testing.T) {
	_, ts, _, _, _ := setupRealControlPlane(t)
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")
	registered := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{"name": "Other source team", "email": "source-other@coolify.test", "password": "OtherSecret123!"})
	registered.Body.Close()
	otherCookies, otherCSRF := loginClient(t, ts.URL, "source-other@coolify.test", "OtherSecret123!")
	for _, provider := range []string{"gitlab", "gitea"} {
		endpoint := ts.URL + "/api/v1/" + provider + "-apps"
		response := doJSON(t, http.MethodPost, endpoint, cookies, csrf, "", map[string]string{"name": "UI source", "api_url": "https://source.example.test/api", "html_url": "https://source.example.test"})
		var source struct {
			ID   int64  `json:"id"`
			UUID string `json:"uuid"`
		}
		json.NewDecoder(response.Body).Decode(&source)
		response.Body.Close()
		if response.StatusCode != http.StatusCreated || source.UUID == "" {
			t.Fatalf("create %s returned %d", provider, response.StatusCode)
		}
		for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
			denied := doJSON(t, method, endpoint+"/"+source.UUID, otherCookies, otherCSRF, "", map[string]string{"name": "Unauthorized"})
			denied.Body.Close()
			if denied.StatusCode != http.StatusForbidden {
				t.Fatalf("%s cross-team %s returned %d", provider, method, denied.StatusCode)
			}
		}
		updateID := source.UUID
		if provider == "gitlab" {
			updateID = fmt.Sprint(source.ID)
		}
		response = doJSON(t, http.MethodPatch, endpoint+"/"+updateID, cookies, csrf, "", map[string]string{"name": "Updated source", "api_url": "https://changed.example.test/api", "html_url": "https://changed.example.test"})
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("update %s returned %d", provider, response.StatusCode)
		}
		response = doJSON(t, http.MethodGet, endpoint+"/"+source.UUID, cookies, "", "", nil)
		var saved struct {
			Name    string `json:"name"`
			APIURL  string `json:"api_url"`
			HTMLURL string `json:"html_url"`
		}
		json.NewDecoder(response.Body).Decode(&saved)
		response.Body.Close()
		if saved.Name != "Updated source" || saved.APIURL != "https://changed.example.test/api" || saved.HTMLURL != "https://changed.example.test" {
			t.Fatalf("%s discarded configuration: %#v", provider, saved)
		}
		response = doJSON(t, http.MethodDelete, endpoint+"/"+source.UUID, cookies, csrf, "", nil)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("delete %s returned %d", provider, response.StatusCode)
		}
	}
}
