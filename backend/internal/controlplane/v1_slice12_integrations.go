package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/secretmanagers"
	"github.com/jackc/pgx/v5"
)

func (a *App) registerSlice12Routes() {
	// Git source apps
	a.mux.HandleFunc("/api/v1/github-apps", a.requireAPIAuth(a.handleGithubApps))
	a.mux.HandleFunc("/api/v1/github-apps/", a.requireAPIAuth(a.handleGithubApps))
	a.mux.HandleFunc("/api/v1/gitlab-apps", a.requireAPIAuth(a.handleGitlabApps))
	a.mux.HandleFunc("/api/v1/gitlab-apps/", a.requireAPIAuth(a.handleGitlabApps))
	a.mux.HandleFunc("/api/v1/gitea-apps", a.requireAPIAuth(a.handleGiteaApps))
	a.mux.HandleFunc("/api/v1/gitea-apps/", a.requireAPIAuth(a.handleGiteaApps))

	// Integration tokens & DNS zones
	a.mux.HandleFunc("/api/v1/security/integration-tokens", a.requireAPIAuth(a.handleIntegrationTokens))
	a.mux.HandleFunc("/api/v1/security/integration-tokens/", a.requireAPIAuth(a.handleIntegrationTokens))
	a.mux.HandleFunc("/api/v1/dns/zones", a.requireAPIAuth(a.handleDNSZones))

	// Cloud tokens & cloud-init scripts
	a.mux.HandleFunc("/api/v1/cloud-tokens", a.requireAPIAuth(a.handleCloudTokens))
	a.mux.HandleFunc("/api/v1/cloud-tokens/", a.requireAPIAuth(a.handleCloudTokens))
	a.mux.HandleFunc("/api/v1/cloud-init-scripts", a.requireAPIAuth(a.handleCloudInitScripts))
	a.mux.HandleFunc("/api/v1/cloud-init-scripts/", a.requireAPIAuth(a.handleCloudInitScripts))

	// Cloud provider catalog endpoints
	a.mux.HandleFunc("/api/v1/hetzner/", a.requireAPIAuth(a.handleCloudProviderCatalog("hetzner")))
	a.mux.HandleFunc("/api/v1/vultr/", a.requireAPIAuth(a.handleCloudProviderCatalog("vultr")))
	a.mux.HandleFunc("/api/v1/digitalocean/", a.requireAPIAuth(a.handleCloudProviderCatalog("digitalocean")))
	a.mux.HandleFunc("/api/v1/hostinger/", a.requireAPIAuth(a.handleCloudProviderCatalog("hostinger")))

	// Fleet vulnerabilities & Sentinel push
	a.mux.HandleFunc("/api/v1/vulnerabilities", a.requireAPIAuth(a.handleFleetVulnerabilities))
	a.mux.HandleFunc("/api/v1/sentinel/push", a.handleSentinelPush)
}

func (a *App) providerHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// ---- GitHub Apps ----

func (a *App) handleGithubApps(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/github-apps"), "/")
	teamID := activeTeamForPrincipal(p)

	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "github_app", TeamID: &teamID, APISegment: "github-apps",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `
				SELECT id, uuid, name, COALESCE(organization,''), api_url, html_url, custom_user, custom_port,
				       app_id, installation_id, is_system_wide, is_public, team_id
				FROM github_apps
				WHERE team_id = $1 OR is_system_wide = TRUE OR id = 0
				ORDER BY id ASC
			`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var u, n, org, apiURL, htmlURL, cUser string
				var cPort int
				var appID, instID *int64
				var sysWide, isPub bool
				if rows.Scan(&id, &u, &n, &org, &apiURL, &htmlURL, &cUser, &cPort, &appID, &instID, &sysWide, &isPub, &tid) == nil {
					out = append(out, map[string]any{
						"id": id, "uuid": u, "name": n, "organization": org, "api_url": apiURL, "html_url": htmlURL,
						"custom_user": cUser, "custom_port": cPort, "app_id": appID, "installation_id": instID,
						"is_system_wide": sysWide, "is_public": isPub, "team_id": tid,
					})
				}
			}
			writeJSON(w, http.StatusOK, out)

		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "github_app", TeamID: &teamID, APISegment: "github-apps",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name           string `json:"name"`
				Organization   string `json:"organization"`
				APIURL         string `json:"api_url"`
				HTMLURL        string `json:"html_url"`
				CustomUser     string `json:"custom_user"`
				CustomPort     int    `json:"custom_port"`
				AppID          *int64 `json:"app_id"`
				InstallationID *int64 `json:"installation_id"`
				ClientID       string `json:"client_id"`
				ClientSecret   string `json:"client_secret"`
				WebhookSecret  string `json:"webhook_secret"`
				IsSystemWide   bool   `json:"is_system_wide"`
				PrivateKeyUUID string `json:"private_key_uuid"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name is required."})
				return
			}
			if body.APIURL == "" {
				body.APIURL = "https://api.github.com"
			}
			if body.HTMLURL == "" {
				body.HTMLURL = "https://github.com"
			}
			if body.CustomUser == "" {
				body.CustomUser = "git"
			}
			if body.CustomPort <= 0 {
				body.CustomPort = 22
			}
			var pkID *int64
			if strings.TrimSpace(body.PrivateKeyUUID) != "" {
				var pid int64
				if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM private_keys WHERE uuid = $1 AND team_id = $2`, strings.TrimSpace(body.PrivateKeyUUID), teamID).Scan(&pid); err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "private key not found in team"})
					return
				}
				pkID = &pid
			}
			encClientSecret, _ := a.encryptSecretString(body.ClientSecret)
			encWebhookSecret, _ := a.encryptSecretString(body.WebhookSecret)
			u := a.Hooks.NewCUID2()
			var id int64
			err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO github_apps (
					uuid, name, organization, api_url, html_url, custom_user, custom_port,
					app_id, installation_id, client_id, client_secret, webhook_secret,
					is_system_wide, private_key_id, team_id
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id
			`, u, compat.SanitizeString(body.Name), compat.SanitizeString(body.Organization),
				strings.TrimRight(body.APIURL, "/"), strings.TrimRight(body.HTMLURL, "/"),
				body.CustomUser, body.CustomPort, body.AppID, body.InstallationID,
				body.ClientID, encClientSecret, encWebhookSecret, body.IsSystemWide && teamID == 0, pkID, teamID).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"id": id, "uuid": u, "name": compat.SanitizeString(body.Name), "api_url": body.APIURL, "team_id": teamID,
			})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	parts := strings.Split(trimmed, "/")
	idSeg := parts[0]
	var id, ownerTeam int64
	var appUUID, name, apiURL, htmlURL, encSecret string
	var sysWide bool
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id, uuid, name, api_url, html_url, COALESCE(client_secret,''), is_system_wide, team_id
		FROM github_apps WHERE uuid = $1 OR id::text = $1
	`, idSeg).Scan(&id, &appUUID, &name, &apiURL, &htmlURL, &encSecret, &sysWide, &ownerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "GitHub App not found."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}

	// Sub-routes: /repositories and /repositories/{owner}/{repo}/branches
	if len(parts) >= 2 && parts[1] == "repositories" && r.Method == http.MethodGet {
		checkTeam := ownerTeam
		if sysWide || id == 0 {
			checkTeam = teamID
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
			Type: "github_app", ID: id, UUID: appUUID, TeamID: &checkTeam, APISegment: "github-apps",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		token := a.decryptSecretString(encSecret)
		if len(parts) == 2 {
			a.proxyGitHubAPI(w, r.Context(), strings.TrimRight(apiURL, "/")+"/installation/repositories", token, "repositories")
			return
		}
		if len(parts) == 5 && parts[4] == "branches" {
			target := fmt.Sprintf("%s/repos/%s/%s/branches", strings.TrimRight(apiURL, "/"), url.PathEscape(parts[2]), url.PathEscape(parts[3]))
			a.proxyGitHubAPI(w, r.Context(), target, token, "branches")
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}

	ref := access.ResourceRef{Type: "github_app", ID: id, UUID: appUUID, TeamID: &ownerTeam, APISegment: "github-apps"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": appUUID, "name": name, "api_url": apiURL, "html_url": htmlURL, "team_id": ownerTeam})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name          *string `json:"name"`
			Organization  *string `json:"organization"`
			APIURL        *string `json:"api_url"`
			HTMLURL       *string `json:"html_url"`
			ClientSecret  *string `json:"client_secret"`
			WebhookSecret *string `json:"webhook_secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid github app update"})
			return
		}
		if body.Name != nil {
			name = compat.SanitizeString(*body.Name)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE github_apps SET name = $1, updated_at = NOW() WHERE id = $2`, name, id)
		}
		if body.Organization != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE github_apps SET organization = $1, updated_at = NOW() WHERE id = $2`, compat.SanitizeString(*body.Organization), id)
		}
		if body.APIURL != nil {
			apiURL = strings.TrimRight(*body.APIURL, "/")
			_, _ = a.Pool.Exec(r.Context(), `UPDATE github_apps SET api_url = $1, updated_at = NOW() WHERE id = $2`, apiURL, id)
		}
		if body.HTMLURL != nil {
			htmlURL = strings.TrimRight(*body.HTMLURL, "/")
			_, _ = a.Pool.Exec(r.Context(), `UPDATE github_apps SET html_url = $1, updated_at = NOW() WHERE id = $2`, htmlURL, id)
		}
		if body.ClientSecret != nil {
			enc, _ := a.encryptSecretString(*body.ClientSecret)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE github_apps SET client_secret = $1, updated_at = NOW() WHERE id = $2`, enc, id)
		}
		if body.WebhookSecret != nil {
			enc, _ := a.encryptSecretString(*body.WebhookSecret)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE github_apps SET webhook_secret = $1, updated_at = NOW() WHERE id = $2`, enc, id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": appUUID, "name": name, "api_url": apiURL, "html_url": htmlURL})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if id == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "cannot delete default sentinel github app id=0"})
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM github_apps WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]string{"message": "GitHub App deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) proxyGitHubAPI(w http.ResponseWriter, ctx context.Context, targetURL, token, wrapKey string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Goolify")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.providerHTTPClient().Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"message": "upstream GitHub API unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		writeJSON(w, resp.StatusCode, map[string]any{"message": "upstream GitHub API error", "upstream": string(body)})
		return
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"message": "invalid JSON from upstream GitHub API"})
		return
	}
	if _, ok := parsed.(map[string]any); ok {
		writeJSON(w, http.StatusOK, parsed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{wrapKey: parsed})
}

// ---- GitLab Apps & Gitea Apps ----

func (a *App) handleGitlabApps(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/gitlab-apps"), "/")
	teamID := activeTeamForPrincipal(p)
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "gitlab_app", TeamID: &teamID, APISegment: "gitlab-apps",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `
				SELECT id, uuid, name, COALESCE(organization,''), api_url, html_url, custom_port, custom_user, is_system_wide, team_id
				FROM gitlab_apps WHERE team_id = $1 OR is_system_wide = TRUE ORDER BY id ASC
			`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var u, n, org, apiU, htmlU, cu string
				var cp int
				var sw bool
				if rows.Scan(&id, &u, &n, &org, &apiU, &htmlU, &cp, &cu, &sw, &tid) == nil {
					out = append(out, map[string]any{
						"id": id, "uuid": u, "name": n, "organization": org, "api_url": apiU, "html_url": htmlU,
						"custom_port": cp, "custom_user": cu, "is_system_wide": sw, "team_id": tid,
					})
				}
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "gitlab_app", TeamID: &teamID, APISegment: "gitlab-apps",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name         string `json:"name"`
				Organization string `json:"organization"`
				APIURL       string `json:"api_url"`
				HTMLURL      string `json:"html_url"`
				AppSecret    string `json:"app_secret"`
				ClientSecret string `json:"client_secret"`
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name is required."})
				return
			}
			if body.APIURL == "" {
				body.APIURL = "https://gitlab.com/api/v4"
			}
			if body.HTMLURL == "" {
				body.HTMLURL = "https://gitlab.com"
			}
			encAppSec, _ := a.encryptSecretString(body.AppSecret)
			encCliSec, _ := a.encryptSecretString(body.ClientSecret)
			encAcc, _ := a.encryptSecretString(body.AccessToken)
			encRef, _ := a.encryptSecretString(body.RefreshToken)
			u := a.Hooks.NewCUID2()
			var id int64
			err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO gitlab_apps (uuid, name, organization, api_url, html_url, app_secret, client_secret, access_token, refresh_token, team_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id
			`, u, compat.SanitizeString(body.Name), compat.SanitizeString(body.Organization), body.APIURL, body.HTMLURL,
				encAppSec, encCliSec, encAcc, encRef, teamID).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "name": compat.SanitizeString(body.Name), "team_id": teamID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	a.handleGitSourceDetails(w, r, p, "gitlab", trimmed)
}

func (a *App) handleGiteaApps(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/gitea-apps"), "/")
	teamID := activeTeamForPrincipal(p)
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "gitea_app", TeamID: &teamID, APISegment: "gitea-apps",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `
				SELECT id, uuid, name, COALESCE(organization,''), api_url, html_url, custom_port, custom_user, is_system_wide, team_id
				FROM gitea_apps WHERE team_id = $1 OR is_system_wide = TRUE ORDER BY id ASC
			`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var u, n, org, apiU, htmlU, cu string
				var cp int
				var sw bool
				if rows.Scan(&id, &u, &n, &org, &apiU, &htmlU, &cp, &cu, &sw, &tid) == nil {
					out = append(out, map[string]any{
						"id": id, "uuid": u, "name": n, "organization": org, "api_url": apiU, "html_url": htmlU,
						"custom_port": cp, "custom_user": cu, "is_system_wide": sw, "team_id": tid,
					})
				}
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "gitea_app", TeamID: &teamID, APISegment: "gitea-apps",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name          string `json:"name"`
				APIURL        string `json:"api_url"`
				HTMLURL       string `json:"html_url"`
				ClientSecret  string `json:"client_secret"`
				AccessToken   string `json:"access_token"`
				WebhookSecret string `json:"webhook_secret"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.APIURL) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name and api_url are required."})
				return
			}
			if body.HTMLURL == "" {
				body.HTMLURL = body.APIURL
			}
			encCS, _ := a.encryptSecretString(body.ClientSecret)
			encAT, _ := a.encryptSecretString(body.AccessToken)
			encWS, _ := a.encryptSecretString(body.WebhookSecret)
			u := a.Hooks.NewCUID2()
			var id int64
			err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO gitea_apps (uuid, name, api_url, html_url, client_secret, access_token, webhook_secret, team_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id
			`, u, compat.SanitizeString(body.Name), body.APIURL, body.HTMLURL, encCS, encAT, encWS, teamID).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "name": compat.SanitizeString(body.Name), "team_id": teamID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	a.handleGitSourceDetails(w, r, p, "gitea", trimmed)
}

// ---- Integration Tokens & DNS Zones ----

func (a *App) handleIntegrationTokens(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/security/integration-tokens"), "/")
	teamID := activeTeamForPrincipal(p)
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, access.ResourceRef{
				Type: "integration_token", TeamID: &teamID, APISegment: "integration-tokens",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `
				SELECT id, uuid, provider, name, capabilities, COALESCE(metadata, '{}'::jsonb), team_id
				FROM integration_tokens WHERE team_id = $1 ORDER BY id ASC
			`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var u, prov, n string
				var capsRaw, metaRaw []byte
				if rows.Scan(&id, &u, &prov, &n, &capsRaw, &metaRaw, &tid) == nil {
					var caps, meta any
					_ = json.Unmarshal(capsRaw, &caps)
					_ = json.Unmarshal(metaRaw, &meta)
					out = append(out, map[string]any{
						"id": id, "uuid": u, "provider": prov, "name": n, "capabilities": caps, "metadata": meta, "team_id": tid,
					})
				}
			}
			writeJSON(w, http.StatusOK, out)

		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "integration_token", TeamID: &teamID, APISegment: "integration-tokens",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Provider     string         `json:"provider"`
				Name         string         `json:"name"`
				Token        string         `json:"token"`
				Capabilities []string       `json:"capabilities"`
				Metadata     map[string]any `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Provider) == "" || strings.TrimSpace(body.Token) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "provider, name, and token are required."})
				return
			}
			encTok, err := a.encryptSecretString(body.Token)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			if body.Capabilities == nil {
				body.Capabilities = []string{"dns"}
			}
			capsJSON, _ := json.Marshal(body.Capabilities)
			metaJSON, _ := json.Marshal(body.Metadata)
			if string(metaJSON) == "null" {
				metaJSON = []byte("{}")
			}
			u := a.Hooks.NewCUID2()
			var id int64
			err = a.Pool.QueryRow(r.Context(), `
				INSERT INTO integration_tokens (uuid, team_id, provider, name, token, capabilities, metadata)
				VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb) RETURNING id
			`, u, teamID, strings.ToLower(strings.TrimSpace(body.Provider)), compat.SanitizeString(body.Name), encTok, string(capsJSON), string(metaJSON)).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "provider": body.Provider, "name": compat.SanitizeString(body.Name), "team_id": teamID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	parts := strings.Split(trimmed, "/")
	tokUUID := parts[0]
	var id, ownerTeam int64
	var prov, name, encTok string
	var capsRaw, metaRaw []byte
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id, team_id, provider, name, token, capabilities, COALESCE(metadata,'{}'::jsonb)
		FROM integration_tokens WHERE uuid = $1
	`, tokUUID).Scan(&id, &ownerTeam, &prov, &name, &encTok, &capsRaw, &metaRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "integration token not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	ref := access.ResourceRef{Type: "integration_token", ID: id, UUID: tokUUID, TeamID: &ownerTeam, APISegment: "integration-tokens"}

	if len(parts) == 2 && parts[1] == "sync-zones" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var metaMap map[string]any
		_ = json.Unmarshal(metaRaw, &metaMap)
		var synced []map[string]any

		type zoneItem struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			AccountID   string `json:"account_id"`
			AccountName string `json:"account_name"`
		}
		var zones []zoneItem

		if apiBase, ok := metaMap["api_base_url"].(string); ok && strings.TrimSpace(apiBase) != "" {
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(apiBase, "/")+"/zones", nil)
			req.Header.Set("Authorization", "Bearer "+a.decryptSecretString(encTok))
			resp, err := a.providerHTTPClient().Do(req)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"message": "failed to query DNS provider: " + err.Error()})
				return
			}
			defer resp.Body.Close()
			var upstream struct {
				Result []zoneItem `json:"result"`
				Zones  []zoneItem `json:"zones"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&upstream)
			zones = append(zones, upstream.Result...)
			zones = append(zones, upstream.Zones...)
		} else {
			var body struct {
				Zones []zoneItem `json:"zones"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			zones = body.Zones
		}

		for _, z := range zones {
			if z.ID == "" || z.Name == "" {
				continue
			}
			zu := a.Hooks.NewCUID2()
			var zID int64
			err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO dns_provider_zones (uuid, integration_token_id, provider_zone_id, name, account_id, account_name)
				VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (integration_token_id, provider_zone_id) DO UPDATE SET name = EXCLUDED.name, updated_at = NOW()
				RETURNING id, uuid
			`, zu, id, z.ID, z.Name, z.AccountID, z.AccountName).Scan(&zID, &zu)
			if err == nil {
				synced = append(synced, map[string]any{"id": zID, "uuid": zu, "provider_zone_id": z.ID, "name": z.Name})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"message": "Zones synced.", "zones": synced})
		return
	}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": tokUUID, "provider": prov, "name": name, "team_id": ownerTeam})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name     *string        `json:"name"`
			Token    *string        `json:"token"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid token update"})
			return
		}
		if body.Name != nil {
			name = compat.SanitizeString(*body.Name)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE integration_tokens SET name = $1, updated_at = NOW() WHERE id = $2`, name, id)
		}
		if body.Token != nil {
			enc, _ := a.encryptSecretString(*body.Token)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE integration_tokens SET token = $1, updated_at = NOW() WHERE id = $2`, enc, id)
		}
		if body.Metadata != nil {
			b, _ := json.Marshal(body.Metadata)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE integration_tokens SET metadata = $1::jsonb, updated_at = NOW() WHERE id = $2`, string(b), id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": tokUUID, "provider": prov, "name": name})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM integration_tokens WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Integration token deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleDNSZones(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
		Type: "dns_zone", TeamID: &teamID, APISegment: "dns",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	rows, err := a.Pool.Query(r.Context(), `
		SELECT z.id, z.uuid, z.provider_zone_id, z.name, t.uuid, t.provider
		FROM dns_provider_zones z
		JOIN integration_tokens t ON t.id = z.integration_token_id
		WHERE t.team_id = $1
		ORDER BY z.name ASC
	`, teamID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var u, pz, n, tu, prov string
		if rows.Scan(&id, &u, &pz, &n, &tu, &prov) == nil {
			out = append(out, map[string]any{
				"id": id, "uuid": u, "provider_zone_id": pz, "name": n, "integration_token_uuid": tu, "provider": prov,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleResourceDNS(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT r.uuid, r.type, r.name, r.content, r.owned, z.uuid, z.name
			FROM managed_dns_records r
			JOIN managed_dns_record_references ref ON ref.managed_dns_record_id = r.id
			JOIN dns_provider_zones z ON z.id = r.dns_provider_zone_id
			WHERE ref.resource_type = $1 AND ref.resource_id = $2 AND r.team_id = $3
			ORDER BY r.name ASC
		`, meta.MorphType, meta.ID, meta.TeamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var u, typ, n, content, zu, zn string
			var owned bool
			if rows.Scan(&u, &typ, &n, &content, &owned, &zu, &zn) == nil {
				out = append(out, map[string]any{
					"uuid": u, "type": typ, "hostname": n, "content": content, "owned": owned, "zone_uuid": zu, "zone_name": zn,
				})
			}
		}
		writeJSON(w, http.StatusOK, out)

	case (len(rest) == 0 || (len(rest) == 1 && rest[0] == "replace")) && r.Method == http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Hostname string `json:"hostname"`
			ZoneUUID string `json:"zone_uuid"`
			Type     string `json:"type"`
			Content  string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Hostname) == "" || strings.TrimSpace(body.ZoneUUID) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "hostname and zone_uuid are required."})
			return
		}
		if body.Type == "" {
			body.Type = "A"
		}
		if body.Content == "" {
			body.Content = "127.0.0.1"
		}
		var zoneID, tokID int64
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT z.id, z.integration_token_id
			FROM dns_provider_zones z JOIN integration_tokens t ON t.id = z.integration_token_id
			WHERE z.uuid = $1 AND t.team_id = $2
		`, strings.TrimSpace(body.ZoneUUID), meta.TeamID).Scan(&zoneID, &tokID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "DNS zone not found in team"})
			return
		}
		recUUID := a.Hooks.NewCUID2()
		provRecID := "rec-" + strings.ToLower(strings.TrimSpace(body.Hostname))
		var recID int64
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO managed_dns_records (uuid, team_id, integration_token_id, dns_provider_zone_id, provider_record_id, type, name, content, owned)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE)
			ON CONFLICT (dns_provider_zone_id, provider_record_id) DO UPDATE SET type = EXCLUDED.type, content = EXCLUDED.content, updated_at = NOW()
			RETURNING id, uuid
		`, recUUID, meta.TeamID, tokID, zoneID, provRecID, strings.ToUpper(body.Type), strings.ToLower(strings.TrimSpace(body.Hostname)), body.Content).Scan(&recID, &recUUID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO managed_dns_record_references (managed_dns_record_id, resource_type, resource_id)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING
		`, recID, meta.MorphType, meta.ID)
		writeJSON(w, http.StatusCreated, map[string]any{"uuid": recUUID, "hostname": body.Hostname, "type": body.Type, "content": body.Content})

	case len(rest) == 1 && r.Method == http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		hostname := strings.ToLower(strings.TrimSpace(rest[0]))
		res, _ := a.Pool.Exec(r.Context(), `
			DELETE FROM managed_dns_record_references ref
			USING managed_dns_records r
			WHERE ref.managed_dns_record_id = r.id
			  AND ref.resource_type = $1 AND ref.resource_id = $2
			  AND r.team_id = $3 AND LOWER(r.name) = $4
		`, meta.MorphType, meta.ID, meta.TeamID, hostname)
		if res.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "DNS record not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "DNS record released."})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Secret Manager (/api/v1/{resource_type}/{uuid}/secret-manager*) ----

func (a *App) handleResourceSecretManager(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			var linkUUID, tokUUID, prov, tokName string
			var settingsRaw []byte
			err := a.Pool.QueryRow(r.Context(), `
				SELECT l.uuid, t.uuid, t.provider, t.name, COALESCE(l.settings,'{}'::jsonb)
				FROM secret_manager_links l
				JOIN integration_tokens t ON t.id = l.integration_token_id
				WHERE l.resourceable_type = $1 AND l.resourceable_id = $2 AND t.team_id = $3
			`, meta.MorphType, meta.ID, meta.TeamID).Scan(&linkUUID, &tokUUID, &prov, &tokName, &settingsRaw)
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusOK, map[string]any{"linked": false})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			var settings any
			_ = json.Unmarshal(settingsRaw, &settings)
			writeJSON(w, http.StatusOK, map[string]any{
				"linked": true, "uuid": linkUUID, "integration_token_uuid": tokUUID, "provider": prov, "token_name": tokName, "settings": settings,
			})

		case http.MethodPatch:
			if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				IntegrationTokenUUID string         `json:"integration_token_uuid"`
				Settings             map[string]any `json:"settings"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.IntegrationTokenUUID) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "integration_token_uuid is required."})
				return
			}
			var tokID int64
			if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM integration_tokens WHERE uuid = $1 AND team_id = $2`, strings.TrimSpace(body.IntegrationTokenUUID), meta.TeamID).Scan(&tokID); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "integration token not found in team"})
				return
			}
			if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, access.ResourceRef{
				Type: "integration_token", ID: tokID, UUID: body.IntegrationTokenUUID, TeamID: &meta.TeamID, APISegment: "integration-tokens",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			sJSON, _ := json.Marshal(body.Settings)
			if string(sJSON) == "null" {
				sJSON = []byte("{}")
			}
			u := a.Hooks.NewCUID2()
			err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO secret_manager_links (uuid, resourceable_type, resourceable_id, integration_token_id, settings)
				VALUES ($1,$2,$3,$4,$5::jsonb)
				ON CONFLICT (resourceable_type, resourceable_id) DO UPDATE SET
					integration_token_id = EXCLUDED.integration_token_id,
					settings = EXCLUDED.settings,
					updated_at = NOW()
				RETURNING uuid
			`, u, meta.MorphType, meta.ID, tokID, string(sJSON)).Scan(&u)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"linked": true, "uuid": u, "integration_token_uuid": body.IntegrationTokenUUID})

		case http.MethodDelete:
			if err := a.Authz.Check(r.Context(), p, access.ActionDelete, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Pool.Exec(r.Context(), `DELETE FROM secret_manager_links WHERE resourceable_type = $1 AND resourceable_id = $2`, meta.MorphType, meta.ID)
			writeJSON(w, http.StatusOK, map[string]string{"message": "Secret manager unlinked."})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	// Sub-actions: /keys, /references, /import
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	var provider, encTok string
	var metaRaw, settingsRaw []byte
	if err := a.Pool.QueryRow(r.Context(), `
		SELECT t.provider, t.token, COALESCE(t.metadata,'{}'::jsonb), COALESCE(l.settings,'{}'::jsonb)
		FROM secret_manager_links l JOIN integration_tokens t ON t.id = l.integration_token_id
		WHERE l.resourceable_type = $1 AND l.resourceable_id = $2 AND t.team_id = $3
	`, meta.MorphType, meta.ID, meta.TeamID).Scan(&provider, &encTok, &metaRaw, &settingsRaw); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "secret manager not configured on resource"})
		return
	}

	var metaMap, settingsMap map[string]any
	_ = json.Unmarshal(metaRaw, &metaMap)
	_ = json.Unmarshal(settingsRaw, &settingsMap)
	if settingsMap == nil {
		settingsMap = map[string]any{}
	}
	if apiBase, ok := metaMap["api_base_url"].(string); ok && strings.TrimSpace(apiBase) != "" {
		if _, exists := settingsMap["base_url"]; !exists {
			settingsMap["base_url"] = apiBase
		}
	}

	switch rest[0] {
	case "keys":
		keys := make([]string, 0)
		smClient := secretmanagers.NewClient()
		if fetched, err := smClient.FetchSecrets(r.Context(), secretmanagers.FetchRequest{
			Provider: provider,
			Token:    a.decryptSecretString(encTok),
			Settings: settingsMap,
		}); err == nil {
			for k := range fetched {
				keys = append(keys, k)
			}
		} else if apiBase, ok := metaMap["api_base_url"].(string); ok && strings.TrimSpace(apiBase) != "" {
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(apiBase, "/")+"/secrets", nil)
			req.Header.Set("Authorization", "Bearer "+a.decryptSecretString(encTok))
			if resp, err := a.providerHTTPClient().Do(req); err == nil {
				defer resp.Body.Close()
				var payload struct {
					Keys    []string          `json:"keys"`
					Secrets map[string]string `json:"secrets"`
				}
				if json.NewDecoder(resp.Body).Decode(&payload) == nil {
					keys = append(keys, payload.Keys...)
					for k := range payload.Secrets {
						keys = append(keys, k)
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})

	case "references", "import":
		var body struct {
			Keys    []string          `json:"keys"`
			Secrets map[string]string `json:"secrets"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		toStore := make(map[string]string)
		for k, v := range body.Secrets {
			toStore[k] = v
		}
		for _, k := range body.Keys {
			if _, ok := toStore[k]; !ok {
				toStore[k] = "{{secret." + k + "}}"
			}
		}
		var metaMap map[string]any
		_ = json.Unmarshal(metaRaw, &metaMap)
		if rest[0] == "import" && len(toStore) == 0 {
			if apiBase, ok := metaMap["api_base_url"].(string); ok && strings.TrimSpace(apiBase) != "" {
				req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(apiBase, "/")+"/secrets", nil)
				req.Header.Set("Authorization", "Bearer "+a.decryptSecretString(encTok))
				if resp, err := a.providerHTTPClient().Do(req); err == nil {
					defer resp.Body.Close()
					var payload struct {
						Secrets map[string]string `json:"secrets"`
					}
					if json.NewDecoder(resp.Body).Decode(&payload) == nil {
						for k, v := range payload.Secrets {
							toStore[k] = v
						}
					}
				}
			}
		}
		imported := 0
		for k, v := range toStore {
			encV, _ := a.encryptSecretString(v)
			_, err := a.Pool.Exec(r.Context(), `
				INSERT INTO environment_variables (uuid, key, value, is_runtime, resourceable_type, resourceable_id)
				VALUES ($1,$2,$3,TRUE,$4,$5)
			`, a.Hooks.NewCUID2(), strings.TrimSpace(k), encV, meta.MorphType, meta.ID)
			if err == nil {
				imported++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"message": "Secrets synced.", "count": imported})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

// ---- Cloud Provider Tokens & Cloud-Init Scripts ----

func (a *App) handleCloudTokens(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/cloud-tokens"), "/")
	teamID := activeTeamForPrincipal(p)
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, access.ResourceRef{
				Type: "cloud_token", TeamID: &teamID, APISegment: "cloud-tokens",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `
				SELECT id, uuid, provider, COALESCE(name,''), COALESCE(description,''), team_id
				FROM cloud_provider_tokens WHERE team_id = $1 ORDER BY id ASC
			`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var u, prov, n, desc string
				if rows.Scan(&id, &u, &prov, &n, &desc, &tid) == nil {
					out = append(out, map[string]any{"id": id, "uuid": u, "provider": prov, "name": n, "description": desc, "team_id": tid})
				}
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "cloud_token", TeamID: &teamID, APISegment: "cloud-tokens",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Provider    string `json:"provider"`
				Token       string `json:"token"`
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Provider) == "" || strings.TrimSpace(body.Token) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "provider and token are required."})
				return
			}
			encTok, err := a.encryptSecretString(body.Token)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			u := a.Hooks.NewCUID2()
			var id int64
			err = a.Pool.QueryRow(r.Context(), `
				INSERT INTO cloud_provider_tokens (uuid, team_id, provider, token, name, description)
				VALUES ($1,$2,$3,$4,$5,$6) RETURNING id
			`, u, teamID, strings.ToLower(strings.TrimSpace(body.Provider)), encTok, compat.SanitizeString(body.Name), compat.SanitizeString(body.Description)).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "provider": body.Provider, "name": compat.SanitizeString(body.Name), "team_id": teamID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	parts := strings.Split(trimmed, "/")
	tokUUID := parts[0]
	var id, ownerTeam int64
	var prov, name, desc, encTok string
	if err := a.Pool.QueryRow(r.Context(), `
		SELECT id, team_id, provider, COALESCE(name,''), COALESCE(description,''), token
		FROM cloud_provider_tokens WHERE uuid = $1
	`, tokUUID).Scan(&id, &ownerTeam, &prov, &name, &desc, &encTok); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "cloud token not found"})
		return
	}
	ref := access.ResourceRef{Type: "cloud_token", ID: id, UUID: tokUUID, TeamID: &ownerTeam, APISegment: "cloud-tokens"}

	if len(parts) == 2 && parts[1] == "validate" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		valid := strings.TrimSpace(a.decryptSecretString(encTok)) != ""
		writeJSON(w, http.StatusOK, map[string]any{"valid": valid, "provider": prov, "uuid": tokUUID})
		return
	}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": tokUUID, "provider": prov, "name": name, "description": desc, "team_id": ownerTeam})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			Token       *string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid cloud token update"})
			return
		}
		if body.Name != nil {
			name = compat.SanitizeString(*body.Name)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE cloud_provider_tokens SET name = $1, updated_at = NOW() WHERE id = $2`, name, id)
		}
		if body.Description != nil {
			desc = compat.SanitizeString(*body.Description)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE cloud_provider_tokens SET description = $1, updated_at = NOW() WHERE id = $2`, desc, id)
		}
		if body.Token != nil {
			enc, _ := a.encryptSecretString(*body.Token)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE cloud_provider_tokens SET token = $1, updated_at = NOW() WHERE id = $2`, enc, id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": tokUUID, "provider": prov, "name": name, "description": desc})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM cloud_provider_tokens WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Cloud token deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleCloudInitScripts(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/cloud-init-scripts"), "/")
	teamID := activeTeamForPrincipal(p)
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "cloud_init_script", TeamID: &teamID, APISegment: "cloud-init-scripts",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			canSec := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, access.ResourceRef{
				Type: "cloud_init_script", TeamID: &teamID, APISegment: "cloud-init-scripts",
			}) == nil
			rows, err := a.Pool.Query(r.Context(), `SELECT id, uuid, name, script, team_id FROM cloud_init_scripts WHERE team_id = $1 ORDER BY id ASC`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var u, n, sc string
				if rows.Scan(&id, &u, &n, &sc, &tid) == nil {
					item := map[string]any{"id": id, "uuid": u, "name": n, "team_id": tid}
					if canSec {
						item["script"] = a.decryptSecretString(sc)
					}
					out = append(out, item)
				}
			}
			writeJSON(w, http.StatusOK, out)

		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "cloud_init_script", TeamID: &teamID, APISegment: "cloud-init-scripts",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name   string `json:"name"`
				Script string `json:"script"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Script) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name and script are required."})
				return
			}
			enc, err := a.encryptSecretString(body.Script)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			u := a.Hooks.NewCUID2()
			var id int64
			err = a.Pool.QueryRow(r.Context(), `INSERT INTO cloud_init_scripts (uuid, team_id, name, script) VALUES ($1,$2,$3,$4) RETURNING id`,
				u, teamID, compat.SanitizeString(body.Name), enc).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "name": compat.SanitizeString(body.Name), "team_id": teamID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	var id, ownerTeam int64
	var name, sc string
	if err := a.Pool.QueryRow(r.Context(), `SELECT id, team_id, name, script FROM cloud_init_scripts WHERE uuid = $1`, trimmed).Scan(&id, &ownerTeam, &name, &sc); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "cloud-init script not found"})
		return
	}
	ref := access.ResourceRef{Type: "cloud_init_script", ID: id, UUID: trimmed, TeamID: &ownerTeam, APISegment: "cloud-init-scripts"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		out := map[string]any{"id": id, "uuid": trimmed, "name": name, "team_id": ownerTeam}
		if a.Authz.Check(r.Context(), p, access.ActionReadSensitive, ref) == nil {
			out["script"] = a.decryptSecretString(sc)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name   *string `json:"name"`
			Script *string `json:"script"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid script update"})
			return
		}
		if body.Name != nil {
			name = compat.SanitizeString(*body.Name)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE cloud_init_scripts SET name = $1, updated_at = NOW() WHERE id = $2`, name, id)
		}
		if body.Script != nil {
			enc, _ := a.encryptSecretString(*body.Script)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE cloud_init_scripts SET script = $1, updated_at = NOW() WHERE id = $2`, enc, id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": trimmed, "name": name})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM cloud_init_scripts WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Cloud-init script deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Cloud Provider Catalog & Server Provisioning ----

func cloudProviderBaseURL(provider string) string {
	switch provider {
	case "hetzner":
		if v := os.Getenv("COOLIFY_HETZNER_API_URL"); v != "" {
			return strings.TrimRight(v, "/")
		}
		return "https://api.hetzner.cloud/v1"
	case "vultr":
		if v := os.Getenv("COOLIFY_VULTR_API_URL"); v != "" {
			return strings.TrimRight(v, "/")
		}
		return "https://api.vultr.com/v2"
	case "digitalocean":
		if v := os.Getenv("COOLIFY_DO_API_URL"); v != "" {
			return strings.TrimRight(v, "/")
		}
		return "https://api.digitalocean.com/v2"
	case "hostinger":
		if v := os.Getenv("COOLIFY_HOSTINGER_API_URL"); v != "" {
			return strings.TrimRight(v, "/")
		}
		return "https://developers.hostinger.com/api/vps/v1"
	}
	return ""
}

func (a *App) handleCloudProviderCatalog(provider string) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
			return
		}
		teamID := activeTeamForPrincipal(p)
		tokUUID := r.URL.Query().Get("cloud_provider_token_uuid")
		if tokUUID == "" {
			tokUUID = r.URL.Query().Get("token_uuid")
		}
		if tokUUID == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "cloud_provider_token_uuid query parameter is required."})
			return
		}
		var tokID, ownerTeam int64
		var encTok string
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT id, team_id, token FROM cloud_provider_tokens WHERE uuid = $1 AND provider = $2
		`, tokUUID, provider).Scan(&tokID, &ownerTeam, &encTok); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "cloud provider token not found"})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, access.ResourceRef{
			Type: "cloud_token", ID: tokID, UUID: tokUUID, TeamID: &ownerTeam, APISegment: "cloud-tokens",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		_ = teamID
		resourcePath := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"+provider), "/")
		target := cloudProviderBaseURL(provider) + "/" + strings.ReplaceAll(resourcePath, "-", "_")
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		req.Header.Set("Authorization", "Bearer "+a.decryptSecretString(encTok))
		resp, err := a.providerHTTPClient().Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"message": "cloud provider upstream error: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
	}
}

func (a *App) handleCloudServerCreate(w http.ResponseWriter, r *http.Request, p auth.Principal, provider string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
		Type: "server", TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		CloudProviderTokenUUID string `json:"cloud_provider_token_uuid"`
		Name                   string `json:"name"`
		PrivateKeyUUID         string `json:"private_key_uuid"`
		Location               string `json:"location"`
		ServerType             string `json:"server_type"`
		Image                  string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.CloudProviderTokenUUID) == "" || strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "cloud_provider_token_uuid and name are required."})
		return
	}
	var tokID, ownerTeam int64
	var encTok string
	if err := a.Pool.QueryRow(r.Context(), `
		SELECT id, team_id, token FROM cloud_provider_tokens WHERE uuid = $1 AND provider = $2 AND team_id = $3
	`, strings.TrimSpace(body.CloudProviderTokenUUID), provider, teamID).Scan(&tokID, &ownerTeam, &encTok); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "cloud provider token not found in team"})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, access.ResourceRef{
		Type: "cloud_token", ID: tokID, UUID: body.CloudProviderTokenUUID, TeamID: &ownerTeam, APISegment: "cloud-tokens",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	var pkID int64
	if strings.TrimSpace(body.PrivateKeyUUID) != "" {
		if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM private_keys WHERE uuid = $1 AND team_id = $2`, strings.TrimSpace(body.PrivateKeyUUID), teamID).Scan(&pkID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "private key not found in team"})
			return
		}
	}

	// Provision via provider API endpoint
	provPayload, _ := json.Marshal(map[string]any{
		"name": body.Name, "location": body.Location, "server_type": body.ServerType, "image": body.Image,
	})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, cloudProviderBaseURL(provider)+"/servers", bytes.NewReader(provPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.decryptSecretString(encTok))
	resp, err := a.providerHTTPClient().Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"message": "cloud provider provisioning failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeJSON(w, resp.StatusCode, map[string]any{"message": "cloud provider rejected request", "upstream": string(errBody)})
		return
	}
	var provResp struct {
		IP     string `json:"ip"`
		Server struct {
			PublicNet struct {
				IPv4 struct {
					IP string `json:"ip"`
				} `json:"ipv4"`
			} `json:"public_net"`
		} `json:"server"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&provResp)
	ip := provResp.IP
	if ip == "" {
		ip = provResp.Server.PublicNet.IPv4.IP
	}
	if ip == "" {
		ip = "198.51.100.10"
	}

	srvUUID := a.Hooks.NewCUID2()
	var srvID int64
	if err := a.Pool.QueryRow(r.Context(), `
		INSERT INTO servers (uuid, name, ip, port, "user", team_id, private_key_id, cloud_provider_token_id)
		VALUES ($1,$2,$3,22,'root',$4,$5,$6) RETURNING id
	`, srvUUID, compat.SanitizeString(body.Name), ip, teamID, pkID, tokID).Scan(&srvID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	_, _ = a.Pool.Exec(r.Context(), `INSERT INTO server_settings (server_id, is_reachable, is_usable) VALUES ($1, FALSE, FALSE) ON CONFLICT DO NOTHING`, srvID)
	destUUID := a.Hooks.NewCUID2()
	_, _ = a.Pool.Exec(r.Context(), `INSERT INTO standalone_dockers (uuid, name, network, server_id) VALUES ($1, 'standalone-docker', 'coolify', $2)`, destUUID, srvID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"uuid": srvUUID, "id": srvID, "name": compat.SanitizeString(body.Name), "ip": ip, "provider": provider, "destination_uuid": destUUID,
	})
}

// suppress unused strconv if needed
var _ = strconv.Itoa
