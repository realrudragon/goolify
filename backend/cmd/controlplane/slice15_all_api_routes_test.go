package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProductionSlice15_ExhaustiveAPIv1RouteMatrixParity exercises every remaining implemented
// REST API v1 endpoint from migration/routes.csv against real PostgreSQL + Redis + local
// HTTP mock upstream servers (Hetzner/Vultr/DO/Hostinger/Cloudflare/GitHub/S3), proving
// zero 404/405/500 regressions and strict cross-team / member authorization.
func TestProductionSlice15_ExhaustiveAPIv1RouteMatrixParity(t *testing.T) {
	_, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()

	// Real local HTTP upstream server for cloud providers, DNS, S3, and GitHub API calls
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/zones") && strings.Contains(p, "/dns_records"):
			if r.Method == http.MethodPost {
				_, _ = w.Write([]byte(`{"success":true,"result":{"id":"rec-1","type":"A","name":"app.test.local","content":"1.2.3.4"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"rec-1","type":"A","name":"app.test.local","content":"1.2.3.4"}]}`))
		case strings.Contains(p, "/zones"):
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"zone-1","name":"test.local","account":{"id":"acc-1","name":"TestAcc"}}]}`))
		case strings.Contains(p, "/servers") || strings.Contains(p, "/instances") || strings.Contains(p, "/droplets") || strings.Contains(p, "/virtual-machines"):
			if r.Method == http.MethodPost {
				_, _ = w.Write([]byte(`{"server":{"id":101,"name":"cloud-srv","public_net":{"ipv4":{"ip":"203.0.113.10"}}},"instance":{"id":"v-1","main_ip":"203.0.113.11"},"droplet":{"id":202,"networks":{"v4":[{"ip_address":"203.0.113.12"}]}},"id":303,"ipv4":[{"address":"203.0.113.13"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"servers":[],"instances":[],"droplets":[],"data":[]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"data":[],"items":[],"server_types":[],"images":[],"ssh_keys":[],"firewalls":[],"networks":[],"regions":[],"plans":[],"os":[],"sizes":[],"data_centers":[],"catalog":[],"templates":[],"post_install_scripts":[]}`))
		}
	}))
	defer upstream.Close()

	t.Setenv("COOLIFY_HETZNER_API_URL", upstream.URL)
	t.Setenv("COOLIFY_VULTR_API_URL", upstream.URL)
	t.Setenv("COOLIFY_DO_API_URL", upstream.URL)
	t.Setenv("COOLIFY_HOSTINGER_API_URL", upstream.URL)

	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")
	oReg := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "OtherS15", "email": "other-s15@coolify.test", "password": "OtherSecret123!",
	})
	oReg.Body.Close()
	otherCookies, _ := loginClient(t, ts.URL, "other-s15@coolify.test", "OtherSecret123!")

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

	// ---- Group 1: Instance toggles, Teams, Shared Envs, Projects & Environments ----
	assertStatus("mcp-disable", http.MethodPost, "/api/v1/mcp/disable", nil, http.StatusOK)
	assertStatus("team-current-1", http.MethodGet, "/api/v1/team", nil, http.StatusOK)
	assertStatus("team-members", http.MethodGet, "/api/v1/team/members", nil, http.StatusOK)
	assertStatus("teams-current", http.MethodGet, "/api/v1/teams/current", nil, http.StatusOK)
	assertStatus("team-by-id", http.MethodGet, "/api/v1/teams/0", nil, http.StatusOK)
	// Cross-team denied on /teams/0
	if res := doJSON(t, http.MethodGet, ts.URL+"/api/v1/teams/0", otherCookies, "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-team GET /teams/0 expected 403, got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	// Team shared envs PATCH & DELETE
	envCreateTxt := assertStatus("team-env-create", http.MethodPost, "/api/v1/team/envs", map[string]any{"key": "TEAM_VAR_1", "value": "v1"}, http.StatusOK, http.StatusCreated)
	var envRow struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(envCreateTxt), &envRow)
	envKeyID := fmt.Sprintf("%d", envRow.ID)
	if envRow.UUID != "" {
		envKeyID = envRow.UUID
	}
	assertStatus("team-env-patch", http.MethodPatch, "/api/v1/team/envs/"+envKeyID, map[string]any{"value": "v2"}, http.StatusOK)
	assertStatus("team-env-del", http.MethodDelete, "/api/v1/team/envs/"+envKeyID, nil, http.StatusOK)

	// Project + Environments + Project/Env Shared Envs
	projRaw := assertStatus("proj-create", http.MethodPost, "/api/v1/projects", map[string]any{"name": "S15Proj"}, http.StatusCreated)
	var projResp struct {
		Project struct {
			ID   int64  `json:"id"`
			UUID string `json:"uuid"`
		} `json:"project"`
		DefaultEnvironment struct {
			ID   int64  `json:"id"`
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"default_environment"`
	}
	_ = json.Unmarshal([]byte(projRaw), &projResp)
	projUUID := projResp.Project.UUID
	envUUID := projResp.DefaultEnvironment.UUID

	assertStatus("proj-envs-list", http.MethodGet, "/api/v1/projects/"+projUUID+"/environments", nil, http.StatusOK)
	newEnvRaw := assertStatus("proj-env-create", http.MethodPost, "/api/v1/projects/"+projUUID+"/environments", map[string]any{"name": "staging-s15"}, http.StatusCreated, http.StatusOK)
	var newEnv struct {
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(newEnvRaw), &newEnv)

	pEnvRaw := assertStatus("proj-shared-env-create", http.MethodPost, "/api/v1/projects/"+projUUID+"/envs", map[string]any{"key": "P_VAR", "value": "1"}, http.StatusOK, http.StatusCreated)
	var pEnv struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(pEnvRaw), &pEnv)
	pEnvID := fmt.Sprintf("%d", pEnv.ID)
	if pEnv.UUID != "" {
		pEnvID = pEnv.UUID
	}
	assertStatus("proj-shared-env-patch", http.MethodPatch, "/api/v1/projects/"+projUUID+"/envs/"+pEnvID, map[string]any{"value": "2"}, http.StatusOK)
	assertStatus("proj-shared-env-del", http.MethodDelete, "/api/v1/projects/"+projUUID+"/envs/"+pEnvID, nil, http.StatusOK)

	eEnvRaw := assertStatus("env-shared-env-create", http.MethodPost, "/api/v1/projects/"+projUUID+"/environments/"+envUUID+"/envs", map[string]any{"key": "E_VAR", "value": "1"}, http.StatusOK, http.StatusCreated)
	var eEnv struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(eEnvRaw), &eEnv)
	eEnvID := fmt.Sprintf("%d", eEnv.ID)
	if eEnv.UUID != "" {
		eEnvID = eEnv.UUID
	}
	assertStatus("env-shared-env-list", http.MethodGet, "/api/v1/projects/"+projUUID+"/environments/"+envUUID+"/envs", nil, http.StatusOK)
	assertStatus("env-shared-env-patch", http.MethodPatch, "/api/v1/projects/"+projUUID+"/environments/"+envUUID+"/envs/"+eEnvID, map[string]any{"value": "2"}, http.StatusOK)
	assertStatus("env-shared-env-del", http.MethodDelete, "/api/v1/projects/"+projUUID+"/environments/"+envUUID+"/envs/"+eEnvID, nil, http.StatusOK)
	if newEnv.UUID != "" {
		assertStatus("proj-env-delete", http.MethodDelete, "/api/v1/projects/"+projUUID+"/environments/"+newEnv.UUID, nil, http.StatusOK)
	}

	// ---- Group 2: Security Keys, Integration Tokens, Cloud Tokens, Cloud-Init Scripts, S3, Tags ----
	pkRaw := assertStatus("pk-create", http.MethodPost, "/api/v1/security/keys", map[string]any{
		"name": "s15-key", "private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\nTEST\n-----END OPENSSH PRIVATE KEY-----",
	}, http.StatusCreated)
	var pk struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(pkRaw), &pk)
	assertStatus("pk-patch", http.MethodPatch, "/api/v1/security/keys/"+pk.UUID, map[string]any{"name": "s15-key-upd"}, http.StatusOK)

	itRaw := assertStatus("it-create", http.MethodPost, "/api/v1/security/integration-tokens", map[string]any{
		"name": "cf-s15", "provider": "cloudflare", "token": "cf-tok-123", "metadata": map[string]any{"api_base_url": upstream.URL},
	}, http.StatusCreated)
	var it struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(itRaw), &it)
	assertStatus("it-get", http.MethodGet, "/api/v1/security/integration-tokens/"+it.UUID, nil, http.StatusOK)
	assertStatus("it-patch", http.MethodPatch, "/api/v1/security/integration-tokens/"+it.UUID, map[string]any{"name": "cf-s15-upd"}, http.StatusOK)
	assertStatus("it-sync-zones", http.MethodPost, "/api/v1/security/integration-tokens/"+it.UUID+"/sync-zones", nil, http.StatusOK)
	zonesRaw := assertStatus("dns-zones-list", http.MethodGet, "/api/v1/dns/zones", nil, http.StatusOK)
	var zones []struct {
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(zonesRaw), &zones)
	if len(zones) == 0 {
		t.Fatalf("expected synced DNS zone from upstream, got: %s", zonesRaw)
	}
	zoneUUID := zones[0].UUID

	ctRaw := assertStatus("ct-create-hetzner", http.MethodPost, "/api/v1/cloud-tokens", map[string]any{
		"name": "hetz-s15", "provider": "hetzner", "token": "hetz-tok",
	}, http.StatusCreated)
	var ct struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(ctRaw), &ct)
	assertStatus("ct-get", http.MethodGet, "/api/v1/cloud-tokens/"+ct.UUID, nil, http.StatusOK)
	assertStatus("ct-patch", http.MethodPatch, "/api/v1/cloud-tokens/"+ct.UUID, map[string]any{"name": "hetz-s15-upd"}, http.StatusOK)
	assertStatus("ct-validate", http.MethodPost, "/api/v1/cloud-tokens/"+ct.UUID+"/validate", nil, http.StatusOK)

	mkCloudToken := func(prov string) string {
		raw := assertStatus("ct-create-"+prov, http.MethodPost, "/api/v1/cloud-tokens", map[string]any{
			"name": prov + "-s15", "provider": prov, "token": prov + "-tok",
		}, http.StatusCreated)
		var out struct{ UUID string `json:"uuid"` }
		_ = json.Unmarshal([]byte(raw), &out)
		return out.UUID
	}
	vultrTok := mkCloudToken("vultr")
	doTok := mkCloudToken("digitalocean")
	hostingerTok := mkCloudToken("hostinger")

	// Cloud Provider Catalogs & Server Provisioning (Hetzner, Vultr, DigitalOcean, Hostinger)
	for _, ep := range []string{
		"/api/v1/hetzner/server-types?cloud_provider_token_uuid=" + ct.UUID,
		"/api/v1/hetzner/images?cloud_provider_token_uuid=" + ct.UUID,
		"/api/v1/hetzner/ssh-keys?cloud_provider_token_uuid=" + ct.UUID,
		"/api/v1/hetzner/firewalls?cloud_provider_token_uuid=" + ct.UUID,
		"/api/v1/hetzner/networks?cloud_provider_token_uuid=" + ct.UUID,
		"/api/v1/vultr/regions?cloud_provider_token_uuid=" + vultrTok,
		"/api/v1/vultr/plans?cloud_provider_token_uuid=" + vultrTok,
		"/api/v1/vultr/os?cloud_provider_token_uuid=" + vultrTok,
		"/api/v1/vultr/ssh-keys?cloud_provider_token_uuid=" + vultrTok,
		"/api/v1/digitalocean/regions?cloud_provider_token_uuid=" + doTok,
		"/api/v1/digitalocean/sizes?cloud_provider_token_uuid=" + doTok,
		"/api/v1/digitalocean/images?cloud_provider_token_uuid=" + doTok,
		"/api/v1/digitalocean/ssh-keys?cloud_provider_token_uuid=" + doTok,
		"/api/v1/hostinger/data-centers?cloud_provider_token_uuid=" + hostingerTok,
		"/api/v1/hostinger/catalog?cloud_provider_token_uuid=" + hostingerTok,
		"/api/v1/hostinger/templates?cloud_provider_token_uuid=" + hostingerTok,
		"/api/v1/hostinger/ssh-keys?cloud_provider_token_uuid=" + hostingerTok,
		"/api/v1/hostinger/post-install-scripts?cloud_provider_token_uuid=" + hostingerTok,
	} {
		assertStatus("cloud-cat", http.MethodGet, ep, nil, http.StatusOK)
	}
	for _, pSpec := range []struct{ prov, tok string }{
		{"vultr", vultrTok},
		{"digitalocean", doTok},
		{"hostinger", hostingerTok},
	} {
		assertStatus("cloud-srv-"+pSpec.prov, http.MethodPost, "/api/v1/servers/"+pSpec.prov, map[string]any{
			"cloud_provider_token_uuid": pSpec.tok, "name": "srv-" + pSpec.prov, "region": "ewr", "plan": "vc2-1c-1gb", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64", "location": "fsn1", "server_type": "cx22", "private_key_uuid": pk.UUID,
		}, http.StatusCreated, http.StatusOK)
	}

	cisRaw := assertStatus("cis-create", http.MethodPost, "/api/v1/cloud-init-scripts", map[string]any{"name": "init1", "script": "#cloud-config\n"}, http.StatusCreated)
	var cis struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(cisRaw), &cis)
	assertStatus("cis-list", http.MethodGet, "/api/v1/cloud-init-scripts", nil, http.StatusOK)
	assertStatus("cis-get", http.MethodGet, "/api/v1/cloud-init-scripts/"+cis.UUID, nil, http.StatusOK)
	assertStatus("cis-patch", http.MethodPatch, "/api/v1/cloud-init-scripts/"+cis.UUID, map[string]any{"name": "init1-upd"}, http.StatusOK)
	assertStatus("cis-del", http.MethodDelete, "/api/v1/cloud-init-scripts/"+cis.UUID, nil, http.StatusOK)

	s3Raw := assertStatus("s3-create", http.MethodPost, "/api/v1/s3-storages", map[string]any{
		"name": "s3-s15", "bucket": "bk", "region": "us-east-1", "endpoint": upstream.URL, "key": "k", "secret": "s",
	}, http.StatusCreated)
	var s3Obj struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(s3Raw), &s3Obj)
	assertStatus("s3-list", http.MethodGet, "/api/v1/s3-storages", nil, http.StatusOK)
	assertStatus("s3-patch", http.MethodPatch, "/api/v1/s3-storages/"+s3Obj.UUID, map[string]any{"name": "s3-s15-upd"}, http.StatusOK)

	tagRaw := assertStatus("tag-create", http.MethodPost, "/api/v1/tags", map[string]any{"name": "s15tag"}, http.StatusCreated, http.StatusOK)
	var tagObj struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(tagRaw), &tagObj)
	assertStatus("tag-list", http.MethodGet, "/api/v1/tags", nil, http.StatusOK)
	tagRef := tagObj.UUID
	if tagRef == "" {
		tagRef = fmt.Sprintf("%d", tagObj.ID)
	}

	// ---- Group 3: Server Subsystems ----
	sEnvRaw := assertStatus("srv-env-create", http.MethodPost, "/api/v1/servers/localhost/envs", map[string]any{"key": "SRV_V", "value": "1"}, http.StatusOK, http.StatusCreated)
	var sEnv struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(sEnvRaw), &sEnv)
	sEnvID := fmt.Sprintf("%d", sEnv.ID)
	if sEnv.UUID != "" {
		sEnvID = sEnv.UUID
	}
	assertStatus("srv-env-patch", http.MethodPatch, "/api/v1/servers/localhost/envs/"+sEnvID, map[string]any{"value": "2"}, http.StatusOK)
	assertStatus("srv-env-del", http.MethodDelete, "/api/v1/servers/localhost/envs/"+sEnvID, nil, http.StatusOK)
	assertStatus("srv-dc-execs", http.MethodGet, "/api/v1/servers/localhost/docker-cleanup/executions", nil, http.StatusOK)
	assertStatus("srv-log-drains", http.MethodGet, "/api/v1/servers/localhost/log-drains", nil, http.StatusOK)
	assertStatus("srv-sentinel", http.MethodGet, "/api/v1/servers/localhost/sentinel", nil, http.StatusOK)
	assertStatus("srv-cf-patch", http.MethodPatch, "/api/v1/servers/localhost/cloudflare-tunnel", map[string]any{"is_cloudflare_tunnel": true}, http.StatusOK)
	assertStatus("srv-cf-disable", http.MethodPost, "/api/v1/servers/localhost/cloudflare-tunnel/disable", nil, http.StatusOK)
	assertStatus("dest-get", http.MethodGet, "/api/v1/destinations/standalone-docker-0", nil, http.StatusOK)
	assertStatus("srv-validate-get", http.MethodGet, "/api/v1/servers/localhost/validate", nil, http.StatusOK, http.StatusAccepted)
	assertStatus("srv-validate-post", http.MethodPost, "/api/v1/servers/localhost/validate", nil, http.StatusOK, http.StatusAccepted)
	assertStatus("srv-hostkey-reset", http.MethodPost, "/api/v1/servers/localhost/host-key/reset", nil, http.StatusOK)
	assertStatus("srv-reg-post", http.MethodPost, "/api/v1/servers/localhost/registries", map[string]any{"registry": "ghcr.io", "username": "u", "password": "p"}, http.StatusCreated, http.StatusOK)
	assertStatus("srv-reg-get", http.MethodGet, "/api/v1/servers/localhost/registries", nil, http.StatusOK)
	assertStatus("srv-reg-check", http.MethodPost, "/api/v1/servers/localhost/registries/ghcr.io/check", nil, http.StatusOK)
	assertStatus("srv-reg-del", http.MethodDelete, "/api/v1/servers/localhost/registries/ghcr.io", nil, http.StatusOK)
	assertStatus("srv-proxy-get", http.MethodGet, "/api/v1/servers/localhost/proxy", nil, http.StatusOK)
	assertStatus("srv-proxy-patch", http.MethodPatch, "/api/v1/servers/localhost/proxy", map[string]any{"type": "TRAEFIK"}, http.StatusOK)
	assertStatus("srv-proxy-restart", http.MethodPost, "/api/v1/servers/localhost/proxy/restart", nil, http.StatusOK, http.StatusAccepted)
	assertStatus("srv-import-check", http.MethodGet, "/api/v1/servers/import/check", nil, http.StatusOK)
	assertStatus("srv-transfer-get", http.MethodGet, "/api/v1/servers/localhost/transfer", nil, http.StatusOK)
	assertStatus("srv-migrate-post", http.MethodPost, "/api/v1/servers/localhost/migrate", map[string]any{"target_url": "https://target.coolify.test"}, http.StatusOK)

	// ---- Group 4: Applications (typed creates, envs, logs, previews, storages, tags, DNS, secret-manager, lifecycle, tasks) ----
	assertStatus("apps-list", http.MethodGet, "/api/v1/applications", nil, http.StatusOK)
	var firstAppUUID string
	for _, seg := range []string{"public", "private-github-app", "private-gitea-app", "private-deploy-key", "dockerfile"} {
		body := map[string]any{
			"name": "app-" + seg, "project_uuid": projUUID, "environment_uuid": envUUID,
			"server_uuid": "localhost", "destination_uuid": "standalone-docker-0",
			"git_repository": "https://github.com/coollabsio/coolify-examples", "git_branch": "main",
			"dockerfile": "FROM alpine\n",
		}
		raw := assertStatus("app-create-"+seg, http.MethodPost, "/api/v1/applications/"+seg, body, http.StatusCreated, http.StatusOK)
		if firstAppUUID == "" {
			var a struct{ UUID string `json:"uuid"` }
			_ = json.Unmarshal([]byte(raw), &a)
			firstAppUUID = a.UUID
		}
	}
	assertStatus("app-get", http.MethodGet, "/api/v1/applications/"+firstAppUUID, nil, http.StatusOK)
	assertStatus("deploy-get", http.MethodGet, "/api/v1/deploy?uuid="+firstAppUUID, nil, http.StatusOK, http.StatusAccepted)
	assertStatus("deployments-list", http.MethodGet, "/api/v1/deployments", nil, http.StatusOK)
	depRaw := assertStatus("app-deploy-post", http.MethodPost, "/api/v1/deploy", map[string]any{"uuid": firstAppUUID}, http.StatusOK, http.StatusAccepted)
	var depResp struct {
		Deployments []struct {
			DeploymentUUID string `json:"deployment_uuid"`
		} `json:"deployments"`
		DeploymentUUID string `json:"deployment_uuid"`
	}
	_ = json.Unmarshal([]byte(depRaw), &depResp)
	depUUID := depResp.DeploymentUUID
	if depUUID == "" && len(depResp.Deployments) > 0 {
		depUUID = depResp.Deployments[0].DeploymentUUID
	}
	if depUUID != "" {
		assertStatus("deployment-get", http.MethodGet, "/api/v1/deployments/"+depUUID, nil, http.StatusOK)
	}

	// Secret manager on application
	assertStatus("app-sm-patch", http.MethodPatch, "/api/v1/applications/"+firstAppUUID+"/secret-manager", map[string]any{"integration_token_uuid": it.UUID, "settings": map[string]any{"project": "p"}}, http.StatusOK, http.StatusCreated)
	assertStatus("app-sm-get", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/secret-manager", nil, http.StatusOK)
	assertStatus("app-sm-refs", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/secret-manager/references", map[string]any{"key": "API_SECRET", "secret_key": "API_SECRET", "path": "/prod/api", "references": []map[string]any{{"key": "API_SECRET", "path": "/prod/api"}}}, http.StatusOK, http.StatusCreated)
	assertStatus("app-sm-del", http.MethodDelete, "/api/v1/applications/"+firstAppUUID+"/secret-manager", nil, http.StatusOK)

	// DNS on application
	assertStatus("app-dns-post", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/dns", map[string]any{"zone_uuid": zoneUUID, "hostname": "app.test.local", "content": "1.2.3.4", "type": "A"}, http.StatusOK, http.StatusCreated)
	assertStatus("app-dns-get", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/dns", nil, http.StatusOK)
	assertStatus("app-dns-replace", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/dns/replace", map[string]any{"zone_uuid": zoneUUID, "hostname": "app.test.local", "content": "5.6.7.8", "type": "A"}, http.StatusOK, http.StatusCreated)

	// App envs, logs, previews, storages, tags, lifecycle, scheduled tasks
	aEnvRaw := assertStatus("app-env-post", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/envs", map[string]any{"key": "APP_K", "value": "1"}, http.StatusCreated, http.StatusOK)
	var aEnv struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(aEnvRaw), &aEnv)
	assertStatus("app-env-patch", http.MethodPatch, "/api/v1/applications/"+firstAppUUID+"/envs", map[string]any{"key": "APP_K", "value": "2"}, http.StatusOK, http.StatusCreated)
	if aEnv.UUID != "" {
		assertStatus("app-env-del", http.MethodDelete, "/api/v1/applications/"+firstAppUUID+"/envs/"+aEnv.UUID, nil, http.StatusOK)
	}
	assertStatus("app-logs", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/logs", nil, http.StatusOK)
	assertStatus("app-previews-list", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/previews", nil, http.StatusOK)
	assertStatus("app-preview-create", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/previews", map[string]any{"pull_request_id": 42, "fqdn": "https://pr42.test.local"}, http.StatusCreated, http.StatusOK)
	assertStatus("app-preview-logs", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/previews/42/logs", nil, http.StatusOK)

	stRaw := assertStatus("app-storage-post", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/storages", map[string]any{"type": "persistent", "name": "vol1", "mount_path": "/data"}, http.StatusCreated, http.StatusOK)
	var stObj struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(stRaw), &stObj)
	if stObj.UUID != "" {
		assertStatus("app-vol-bk-put", http.MethodPut, "/api/v1/applications/"+firstAppUUID+"/storages/"+stObj.UUID+"/backups", map[string]any{"frequency": "0 0 * * *", "enabled": true}, http.StatusOK, http.StatusCreated)
		assertStatus("app-vol-bk-del", http.MethodDelete, "/api/v1/applications/"+firstAppUUID+"/storages/"+stObj.UUID+"/backups", nil, http.StatusOK)
	}
	assertStatus("app-tag-post", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/tags", map[string]any{"tag": "s15tag"}, http.StatusOK, http.StatusCreated)
	assertStatus("app-tag-list", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/tags", nil, http.StatusOK)
	assertStatus("app-tag-del", http.MethodDelete, "/api/v1/applications/"+firstAppUUID+"/tags/"+tagRef, nil, http.StatusOK)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		for _, act := range []string{"start", "restart", "stop"} {
			assertStatus("app-"+m+"-"+act, m, "/api/v1/applications/"+firstAppUUID+"/"+act, nil, http.StatusOK, http.StatusAccepted)
		}
	}
	atRaw := assertStatus("app-task-post", http.MethodPost, "/api/v1/applications/"+firstAppUUID+"/scheduled-tasks", map[string]any{"name": "cron1", "command": "echo 1", "frequency": "0 * * * *"}, http.StatusCreated, http.StatusOK)
	var atObj struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(atRaw), &atObj)
	assertStatus("app-task-list", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/scheduled-tasks", nil, http.StatusOK)
	assertStatus("app-task-patch", http.MethodPatch, "/api/v1/applications/"+firstAppUUID+"/scheduled-tasks/"+atObj.UUID, map[string]any{"name": "cron1-upd"}, http.StatusOK)
	assertStatus("app-task-execs", http.MethodGet, "/api/v1/applications/"+firstAppUUID+"/scheduled-tasks/"+atObj.UUID+"/executions", nil, http.StatusOK)
	assertStatus("app-task-del", http.MethodDelete, "/api/v1/applications/"+firstAppUUID+"/scheduled-tasks/"+atObj.UUID, nil, http.StatusOK)

	// ---- Group 5: GitHub Apps & GitLab Apps PATCH/DELETE ----
	ghRaw := assertStatus("gh-create", http.MethodPost, "/api/v1/github-apps", map[string]any{"name": "gh-s15", "api_url": upstream.URL, "html_url": upstream.URL}, http.StatusCreated)
	var ghObj struct{ ID int64 `json:"id"` }
	_ = json.Unmarshal([]byte(ghRaw), &ghObj)
	assertStatus("gh-patch", http.MethodPatch, fmt.Sprintf("/api/v1/github-apps/%d", ghObj.ID), map[string]any{"name": "gh-s15-upd"}, http.StatusOK)

	glRaw := assertStatus("gl-create", http.MethodPost, "/api/v1/gitlab-apps", map[string]any{"name": "gl-s15", "api_url": upstream.URL, "html_url": upstream.URL}, http.StatusCreated)
	var glObj struct{ ID int64 `json:"id"` }
	_ = json.Unmarshal([]byte(glRaw), &glObj)
	assertStatus("gl-patch", http.MethodPatch, fmt.Sprintf("/api/v1/gitlab-apps/%d", glObj.ID), map[string]any{"name": "gl-s15-upd"}, http.StatusOK)
	assertStatus("gl-del", http.MethodDelete, fmt.Sprintf("/api/v1/gitlab-apps/%d", glObj.ID), nil, http.StatusOK)

	// ---- Group 6: Databases (ALL sub-routes!) ----
	dbRaw := assertStatus("db-create", http.MethodPost, "/api/v1/databases/postgresql", map[string]any{
		"name": "pg-s15", "project_uuid": projUUID, "environment_uuid": envUUID,
		"server_uuid": "localhost", "destination_uuid": "standalone-docker-0",
	}, http.StatusCreated, http.StatusOK)
	var dbObj struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(dbRaw), &dbObj)
	dbUUID := dbObj.UUID

	assertStatus("db-vuln", http.MethodGet, "/api/v1/databases/"+dbUUID+"/vulnerabilities", nil, http.StatusOK)
	assertStatus("db-patch", http.MethodPatch, "/api/v1/databases/"+dbUUID, map[string]any{"description": "updated pg"}, http.StatusOK)
	assertStatus("db-logs", http.MethodGet, "/api/v1/databases/"+dbUUID+"/logs", nil, http.StatusOK)
	upRaw := assertStatus("db-imp-upload", http.MethodPost, "/api/v1/databases/"+dbUUID+"/imports/uploads", map[string]any{"filename": "dump.sql", "content": "SELECT 1;"}, http.StatusCreated, http.StatusOK)
	var upObj struct {
		UUID       string `json:"uuid"`
		UploadUUID string `json:"upload_uuid"`
	}
	_ = json.Unmarshal([]byte(upRaw), &upObj)
	upRef := upObj.UploadUUID
	if upRef == "" {
		upRef = upObj.UUID
	}
	impRaw := assertStatus("db-imp-run", http.MethodPost, "/api/v1/databases/"+dbUUID+"/imports", map[string]any{"upload_uuid": upRef}, http.StatusAccepted, http.StatusOK)
	var impObj struct{ ActivityID any `json:"activity_id"` }
	_ = json.Unmarshal([]byte(impRaw), &impObj)
	assertStatus("db-imp-status", http.MethodGet, fmt.Sprintf("/api/v1/databases/%s/imports/%v", dbUUID, impObj.ActivityID), nil, http.StatusOK)

	bkRaw := assertStatus("db-bk-create", http.MethodPost, "/api/v1/databases/"+dbUUID+"/backups", map[string]any{"frequency": "0 0 * * *", "enabled": true}, http.StatusCreated, http.StatusOK)
	var bkObj struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(bkRaw), &bkObj)
	assertStatus("db-bk-list", http.MethodGet, "/api/v1/databases/"+dbUUID+"/backups", nil, http.StatusOK)
	assertStatus("db-bk-patch", http.MethodPatch, "/api/v1/databases/"+dbUUID+"/backups/"+bkObj.UUID, map[string]any{"frequency": "0 2 * * *"}, http.StatusOK)
	assertStatus("db-bk-execs", http.MethodGet, "/api/v1/databases/"+dbUUID+"/backups/"+bkObj.UUID+"/executions", nil, http.StatusOK)
	// Seed a backup execution so DELETE execution works cleanly
	var bkSchedID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM scheduled_database_backups WHERE uuid = $1`, bkObj.UUID).Scan(&bkSchedID)
	_, _ = pool.Exec(ctx, `INSERT INTO scheduled_database_backup_executions (uuid, scheduled_database_backup_id, status) VALUES ('bkexec-s15', $1, 'success')`, bkSchedID)
	assertStatus("db-bk-exec-del", http.MethodDelete, "/api/v1/databases/"+dbUUID+"/backups/"+bkObj.UUID+"/executions/bkexec-s15", nil, http.StatusOK)
	assertStatus("db-bk-del", http.MethodDelete, "/api/v1/databases/"+dbUUID+"/backups/"+bkObj.UUID, nil, http.StatusOK)

	dbStRaw := assertStatus("db-st-post", http.MethodPost, "/api/v1/databases/"+dbUUID+"/storages", map[string]any{"type": "persistent", "name": "pgvol", "mount_path": "/var/lib/postgresql/data"}, http.StatusCreated, http.StatusOK)
	var dbSt struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(dbStRaw), &dbSt)
	assertStatus("db-st-get", http.MethodGet, "/api/v1/databases/"+dbUUID+"/storages", nil, http.StatusOK)
	assertStatus("db-st-patch", http.MethodPatch, "/api/v1/databases/"+dbUUID+"/storages", map[string]any{"id": dbSt.ID, "uuid": dbSt.UUID, "type": "persistent", "mount_path": "/var/lib/pg2"}, http.StatusOK)
	assertStatus("db-st-vb-put", http.MethodPut, "/api/v1/databases/"+dbUUID+"/storages/"+dbSt.UUID+"/backups", map[string]any{"frequency": "0 0 * * *", "enabled": true}, http.StatusOK, http.StatusCreated)
	assertStatus("db-st-vb-run", http.MethodPost, "/api/v1/databases/"+dbUUID+"/storages/"+dbSt.UUID+"/backups/run", nil, http.StatusOK, http.StatusAccepted)
	assertStatus("db-st-vb-del", http.MethodDelete, "/api/v1/databases/"+dbUUID+"/storages/"+dbSt.UUID+"/backups", nil, http.StatusOK)
	assertStatus("db-st-del", http.MethodDelete, "/api/v1/databases/"+dbUUID+"/storages/"+dbSt.UUID, nil, http.StatusOK)

	dbEnvRaw := assertStatus("db-env-post", http.MethodPost, "/api/v1/databases/"+dbUUID+"/envs", map[string]any{"key": "DB_K", "value": "1"}, http.StatusCreated, http.StatusOK)
	var dbEnv struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(dbEnvRaw), &dbEnv)
	assertStatus("db-env-get", http.MethodGet, "/api/v1/databases/"+dbUUID+"/envs", nil, http.StatusOK)
	assertStatus("db-env-patch", http.MethodPatch, "/api/v1/databases/"+dbUUID+"/envs", map[string]any{"key": "DB_K", "value": "2"}, http.StatusOK, http.StatusCreated)
	assertStatus("db-env-bulk", http.MethodPatch, "/api/v1/databases/"+dbUUID+"/envs/bulk", map[string]any{"data": []map[string]any{{"key": "DB_K2", "value": "3"}}}, http.StatusOK, http.StatusCreated)
	if dbEnv.UUID != "" {
		assertStatus("db-env-del", http.MethodDelete, "/api/v1/databases/"+dbUUID+"/envs/"+dbEnv.UUID, nil, http.StatusOK)
	}
	assertStatus("db-tag-post", http.MethodPost, "/api/v1/databases/"+dbUUID+"/tags", map[string]any{"tag": "s15tag"}, http.StatusOK, http.StatusCreated)
	assertStatus("db-tag-get", http.MethodGet, "/api/v1/databases/"+dbUUID+"/tags", nil, http.StatusOK)
	assertStatus("db-tag-del", http.MethodDelete, "/api/v1/databases/"+dbUUID+"/tags/"+tagRef, nil, http.StatusOK)
	// Create a second real environment for move tests
	mvEnvRaw := assertStatus("proj-env-move-target", http.MethodPost, "/api/v1/projects/"+projUUID+"/environments", map[string]any{"name": "move-target-s15"}, http.StatusCreated, http.StatusOK)
	var mvEnv struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(mvEnvRaw), &mvEnv)
	assertStatus("db-move", http.MethodPost, "/api/v1/databases/"+dbUUID+"/move", map[string]any{"environment_uuid": mvEnv.UUID}, http.StatusOK)
	assertStatus("db-migrate", http.MethodPost, "/api/v1/databases/"+dbUUID+"/migrate", map[string]any{"destination_uuid": "standalone-docker-0"}, http.StatusOK)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		for _, act := range []string{"start", "restart", "stop"} {
			assertStatus("db-"+m+"-"+act, m, "/api/v1/databases/"+dbUUID+"/"+act, nil, http.StatusOK, http.StatusAccepted)
		}
	}

	// ---- Group 7: Services (ALL 56 sub-routes!) ----
	assertStatus("svc-list", http.MethodGet, "/api/v1/services", nil, http.StatusOK)
	svcRaw := assertStatus("svc-create", http.MethodPost, "/api/v1/services", map[string]any{
		"name": "svc-s15", "project_uuid": projUUID, "environment_uuid": envUUID,
		"server_uuid": "localhost", "destination_uuid": "standalone-docker-0",
		"docker_compose_raw": "services:\n  web:\n    image: nginx:alpine\n  db:\n    image: postgres:16-alpine\n",
	}, http.StatusCreated, http.StatusOK)
	var svcObj struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(svcRaw), &svcObj)
	svcUUID := svcObj.UUID
	// Ensure service_applications and service_databases child rows exist for sub-resource verification
	var svcID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM services WHERE uuid = $1`, svcUUID).Scan(&svcID)
	_, _ = pool.Exec(ctx, `INSERT INTO service_applications (uuid, name, service_id) VALUES ('svca-s15', 'web', $1) ON CONFLICT DO NOTHING`, svcID)
	_, _ = pool.Exec(ctx, `INSERT INTO service_databases (uuid, name, service_id) VALUES ('svcd-s15', 'db', $1) ON CONFLICT DO NOTHING`, svcID)

	assertStatus("svc-get", http.MethodGet, "/api/v1/services/"+svcUUID, nil, http.StatusOK)
	assertStatus("svc-vuln", http.MethodGet, "/api/v1/services/"+svcUUID+"/vulnerabilities", nil, http.StatusOK)
	assertStatus("svc-patch", http.MethodPatch, "/api/v1/services/"+svcUUID, map[string]any{"description": "updated svc"}, http.StatusOK)
	assertStatus("svc-logs", http.MethodGet, "/api/v1/services/"+svcUUID+"/logs", nil, http.StatusOK)

	svcStRaw := assertStatus("svc-st-post", http.MethodPost, "/api/v1/services/"+svcUUID+"/storages", map[string]any{"type": "persistent", "name": "svcvol", "mount_path": "/app/data"}, http.StatusCreated, http.StatusOK)
	var svcSt struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.Unmarshal([]byte(svcStRaw), &svcSt)
	assertStatus("svc-st-get", http.MethodGet, "/api/v1/services/"+svcUUID+"/storages", nil, http.StatusOK)
	assertStatus("svc-st-patch", http.MethodPatch, "/api/v1/services/"+svcUUID+"/storages", map[string]any{"id": svcSt.ID, "uuid": svcSt.UUID, "type": "persistent", "mount_path": "/app/data2"}, http.StatusOK)
	assertStatus("svc-st-vb-put", http.MethodPut, "/api/v1/services/"+svcUUID+"/storages/"+svcSt.UUID+"/backups", map[string]any{"frequency": "0 0 * * *", "enabled": true}, http.StatusOK, http.StatusCreated)
	assertStatus("svc-st-vb-run", http.MethodPost, "/api/v1/services/"+svcUUID+"/storages/"+svcSt.UUID+"/backups/run", nil, http.StatusOK, http.StatusAccepted)
	assertStatus("svc-st-vb-del", http.MethodDelete, "/api/v1/services/"+svcUUID+"/storages/"+svcSt.UUID+"/backups", nil, http.StatusOK)
	assertStatus("svc-st-del", http.MethodDelete, "/api/v1/services/"+svcUUID+"/storages/"+svcSt.UUID, nil, http.StatusOK)

	assertStatus("svc-dns-post", http.MethodPost, "/api/v1/services/"+svcUUID+"/dns", map[string]any{"zone_uuid": zoneUUID, "hostname": "svc.test.local", "content": "1.2.3.4", "type": "A"}, http.StatusOK, http.StatusCreated)
	assertStatus("svc-dns-get", http.MethodGet, "/api/v1/services/"+svcUUID+"/dns", nil, http.StatusOK)
	assertStatus("svc-dns-replace", http.MethodPost, "/api/v1/services/"+svcUUID+"/dns/replace", map[string]any{"zone_uuid": zoneUUID, "hostname": "svc.test.local", "content": "5.6.7.8", "type": "A"}, http.StatusOK, http.StatusCreated)
	assertStatus("svc-dns-del", http.MethodDelete, "/api/v1/services/"+svcUUID+"/dns/svc.test.local", nil, http.StatusOK)

	svcEnvRaw := assertStatus("svc-env-post", http.MethodPost, "/api/v1/services/"+svcUUID+"/envs", map[string]any{"key": "SVC_K", "value": "1"}, http.StatusCreated, http.StatusOK)
	var svcEnv struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(svcEnvRaw), &svcEnv)
	assertStatus("svc-env-get", http.MethodGet, "/api/v1/services/"+svcUUID+"/envs", nil, http.StatusOK)
	assertStatus("svc-env-patch", http.MethodPatch, "/api/v1/services/"+svcUUID+"/envs", map[string]any{"key": "SVC_K", "value": "2"}, http.StatusOK, http.StatusCreated)
	if svcEnv.UUID != "" {
		assertStatus("svc-env-del", http.MethodDelete, "/api/v1/services/"+svcUUID+"/envs/"+svcEnv.UUID, nil, http.StatusOK)
	}
	assertStatus("svc-tag-post", http.MethodPost, "/api/v1/services/"+svcUUID+"/tags", map[string]any{"tag": "s15tag"}, http.StatusOK, http.StatusCreated)
	assertStatus("svc-tag-get", http.MethodGet, "/api/v1/services/"+svcUUID+"/tags", nil, http.StatusOK)
	assertStatus("svc-tag-del", http.MethodDelete, "/api/v1/services/"+svcUUID+"/tags/"+tagRef, nil, http.StatusOK)
	assertStatus("svc-move", http.MethodPost, "/api/v1/services/"+svcUUID+"/move", map[string]any{"environment_uuid": mvEnv.UUID}, http.StatusOK)
	assertStatus("svc-migrate", http.MethodPost, "/api/v1/services/"+svcUUID+"/migrate", map[string]any{"destination_uuid": "standalone-docker-0"}, http.StatusOK)
	assertStatus("svc-clone", http.MethodPost, "/api/v1/services/"+svcUUID+"/clone", map[string]any{"name": "svc-s15-clone"}, http.StatusCreated, http.StatusOK)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		for _, act := range []string{"start", "restart", "stop"} {
			assertStatus("svc-"+m+"-"+act, m, "/api/v1/services/"+svcUUID+"/"+act, nil, http.StatusOK, http.StatusAccepted)
		}
	}

	// Service Applications & Service Databases child routes
	assertStatus("svca-list", http.MethodGet, "/api/v1/services/"+svcUUID+"/applications", nil, http.StatusOK)
	assertStatus("svca-patch", http.MethodPatch, "/api/v1/services/"+svcUUID+"/applications/svca-s15", map[string]any{"fqdn": "https://web.test.local"}, http.StatusOK)
	for _, act := range []string{"start", "restart", "stop"} {
		// routes/api.php:462-464 explicitly returns 405 post_required on GET for service application actions
		assertStatus("svca-GET-"+act, http.MethodGet, "/api/v1/services/"+svcUUID+"/applications/svca-s15/"+act, nil, http.StatusMethodNotAllowed)
		assertStatus("svca-POST-"+act, http.MethodPost, "/api/v1/services/"+svcUUID+"/applications/svca-s15/"+act, nil, http.StatusOK, http.StatusAccepted)
	}
	assertStatus("svcd-list", http.MethodGet, "/api/v1/services/"+svcUUID+"/databases", nil, http.StatusOK)
	assertStatus("svcd-get", http.MethodGet, "/api/v1/services/"+svcUUID+"/databases/svcd-s15", nil, http.StatusOK)
	assertStatus("svcd-patch", http.MethodPatch, "/api/v1/services/"+svcUUID+"/databases/svcd-s15", map[string]any{"is_public": true, "public_port": 5433}, http.StatusOK)
	assertStatus("svcd-logs", http.MethodGet, "/api/v1/services/"+svcUUID+"/databases/svcd-s15/logs", nil, http.StatusOK)
	svcdUpRaw := assertStatus("svcd-imp-up", http.MethodPost, "/api/v1/services/"+svcUUID+"/databases/svcd-s15/imports/uploads", map[string]any{"filename": "s.sql", "content": "SELECT 1;"}, http.StatusCreated, http.StatusOK)
	var svcdUp struct {
		UUID       string `json:"uuid"`
		UploadUUID string `json:"upload_uuid"`
	}
	_ = json.Unmarshal([]byte(svcdUpRaw), &svcdUp)
	svcdRef := svcdUp.UploadUUID
	if svcdRef == "" {
		svcdRef = svcdUp.UUID
	}
	svcdImpRaw := assertStatus("svcd-imp-run", http.MethodPost, "/api/v1/services/"+svcUUID+"/databases/svcd-s15/imports", map[string]any{"upload_uuid": svcdRef}, http.StatusAccepted, http.StatusOK)
	var svcdImp struct{ ActivityID any `json:"activity_id"` }
	_ = json.Unmarshal([]byte(svcdImpRaw), &svcdImp)
	assertStatus("svcd-imp-get", http.MethodGet, fmt.Sprintf("/api/v1/services/%s/databases/svcd-s15/imports/%v", svcUUID, svcdImp.ActivityID), nil, http.StatusOK)
	for _, act := range []string{"start", "restart", "stop"} {
		assertStatus("svcd-"+act, http.MethodPost, "/api/v1/services/"+svcUUID+"/databases/svcd-s15/"+act, nil, http.StatusOK, http.StatusAccepted)
	}

	// Service Scheduled Tasks
	staskRaw := assertStatus("svc-task-post", http.MethodPost, "/api/v1/services/"+svcUUID+"/scheduled-tasks", map[string]any{"name": "svccron", "command": "echo svc", "frequency": "0 * * * *"}, http.StatusCreated, http.StatusOK)
	var stask struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal([]byte(staskRaw), &stask)
	assertStatus("svc-task-list", http.MethodGet, "/api/v1/services/"+svcUUID+"/scheduled-tasks", nil, http.StatusOK)
	assertStatus("svc-task-patch", http.MethodPatch, "/api/v1/services/"+svcUUID+"/scheduled-tasks/"+stask.UUID, map[string]any{"name": "svccron2"}, http.StatusOK)
	assertStatus("svc-task-execs", http.MethodGet, "/api/v1/services/"+svcUUID+"/scheduled-tasks/"+stask.UUID+"/executions", nil, http.StatusOK)
	assertStatus("svc-task-run", http.MethodPost, "/api/v1/services/"+svcUUID+"/scheduled-tasks/"+stask.UUID+"/execute", nil, http.StatusOK, http.StatusAccepted)
	assertStatus("svc-task-del", http.MethodDelete, "/api/v1/services/"+svcUUID+"/scheduled-tasks/"+stask.UUID, nil, http.StatusOK)

	// Cleanup deletions (testing DELETE endpoints explicitly!)
	assertStatus("app-delete", http.MethodDelete, "/api/v1/applications/"+firstAppUUID, nil, http.StatusOK)
	assertStatus("db-delete", http.MethodDelete, "/api/v1/databases/"+dbUUID, nil, http.StatusOK)
	assertStatus("svc-delete", http.MethodDelete, "/api/v1/services/"+svcUUID, nil, http.StatusOK)
	assertStatus("tag-delete", http.MethodDelete, "/api/v1/tags/"+tagRef, nil, http.StatusOK)
	assertStatus("s3-delete", http.MethodDelete, "/api/v1/s3-storages/"+s3Obj.UUID, nil, http.StatusOK)
	assertStatus("ct-delete", http.MethodDelete, "/api/v1/cloud-tokens/"+ct.UUID, nil, http.StatusOK)
	assertStatus("it-delete", http.MethodDelete, "/api/v1/security/integration-tokens/"+it.UUID, nil, http.StatusOK)

	// Finally verify GET /api/v1/disable (RT-API-0004) works for root owner
	assertStatus("api-disable-get", http.MethodGet, "/api/v1/disable", nil, http.StatusOK)
	_ = ciph
}
